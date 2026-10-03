# Transactions: MULTI, EXEC and DISCARD

Status: candidate, October 2, 2026.

Every client library's transaction API failed on Keel with
`ERR unknown command 'MULTI'`: go-redis `TxPipelined`, Redigo `MULTI`/`EXEC`,
redis-py `pipeline()` (transactional by default), node-redis `multi()` and
ioredis `multi()`, and libraries built on them, such as keyv's `setMany` and
`deleteMany`. Keel now implements Redis transactions without `WATCH`.

## Contract

After `MULTI`, a connection's commands are queued and answered `+QUEUED`.
`EXEC` runs the queue and answers one array with every command's reply, in
order; `DISCARD` drops it. The replies clients match on are Redis's:

| Situation | Reply |
| --- | --- |
| A command refused while queueing: unknown, wrong number of arguments, not allowed in a transaction, or refused by a replica | That command's error, then `EXECABORT Transaction discarded because of previous errors.` from `EXEC`; nothing runs |
| A command that fails while `EXEC` runs it | Its error in that position of `EXEC`'s array; the rest still run, and there is no rollback |
| `MULTI` inside `MULTI` | `ERR MULTI calls can not be nested`; the transaction stays open |
| `WATCH key` inside `MULTI` | `ERR WATCH inside MULTI is not allowed`; the transaction stays open |
| `EXEC` or `DISCARD` without `MULTI` | `ERR EXEC without MULTI`, `ERR DISCARD without MULTI` |
| `EXEC` with arguments inside a transaction | `EXECABORT Transaction discarded because of: wrong number of arguments for 'exec' command`; the transaction is discarded |
| Writability lost between queueing and `EXEC` | `EXECABORT Transaction discarded because of:` and the reason, for example `FENCED ...` or `MASTERDOWN ...`; nothing runs |

Argument counts are checked while queueing with Redis's arity for Redis's
commands, and each handler's own count for Keel's, so a command Redis would
refuse at queue time is refused at queue time here, with Redis's wording, which
names the command in lower case. A test runs every handler with every count the
table refuses and requires it to refuse as well, so the table can never turn a
valid command into an aborted transaction. An unknown command keeps Keel's
existing `ERR unknown command 'NAME'`, without Redis's `with args beginning
with:` suffix; the error class is the same. Every reply in the table above was
checked against Redis 8.10.1.

A connection that closes inside a transaction runs nothing it queued, and the
memory the queue held is released with the connection.

### Commands inside a transaction

Every command Redis queues is queued, `AUTH` and `BGREWRITEAOF` included, and
runs in its place at `EXEC`:

- `AUTH`, `HELLO` and `CLIENT` are answered by the connection rather than the
  command table, and are handed back to it when `EXEC` reaches them; their
  argument counts are Redis's. Only an authenticated connection can open a
  transaction, so `MULTI` does not bypass `AUTH`. A failed `AUTH` inside `EXEC`
  is a `WRONGPASS` element; the commands queued while the connection was
  authenticated still run, and the connection is unauthenticated afterwards, as
  after a failed `AUTH` outside a transaction.
- `QUIT` is never queued: it answers `+OK` and closes the connection at once,
  which discards the transaction, as in Redis.
- `SELECT 0`, `ECHO`, `SETNX` and `UNLINK` are ordinary commands and are queued.
- `BGREWRITEAOF` starts the rewrite at that point of the transaction, as Redis
  does; see [rewrites](#rewrites).
- `FLUSHDB`, `KEEL.DUMP`, `KEEL.RESTORE`, `INFO`, `MEMORY`, `DBSIZE`, `KEYS` and
  `SCAN` run like any other command.

Refused with Redis's `ERR Command not allowed inside a transaction`, which aborts
the transaction:

- `KEEL.REPL.PULL` and `KEEL.REPL.PULL2`, as Redis refuses its own replication
  commands, `SYNC` and `PSYNC`. A pull serves the stream as it stands between
  commands, can start a snapshot rewrite and can fence the node.
- `KEEL.PROMOTE` and `KEEL.FENCE`, which have no Redis counterpart: a term acts
  on the node, not the dataset, and is made durable outside the log.

`WATCH` is not implemented. It remains an unknown command, so
optimistic-locking APIs (go-redis `Watch`, redis-py `pipeline.watch`,
node-redis and ioredis `watch`) fail with an error instead of silently not
watching. Inside `MULTI`, `WATCH` gets Redis's own answer, `ERR WATCH inside
MULTI is not allowed`, which leaves the transaction open; `WATCH` with no keys
is refused for its argument count and aborts the transaction, as in Redis.
`UNWATCH` answers as Redis does when nothing is watched, which here is always:
`+OK`, and inside `MULTI` it is queued and answers `+OK` in its slot. Clients
send it when they release a connection, as go-redis's `Tx.Close` and
redis-py's pipeline reset do.

## Atomicity

`EXEC` is one command to the event loop, and it runs every queued command before
it returns, on the loop's thread. No other client's command can run between two
of them:

- `-io-threads` only reads, parses and writes sockets in parallel; execution
  stays on the loop thread.
- With `-aof-async-append`, no command executes while a batch is appending.
- With `-aof-concurrent-append`, a run may overlap a pending append only if
  every command in it has a modelled log and reply bound. `MULTI`, `EXEC` and
  `DISCARD` have none, so a run containing `EXEC` waits at the drained barrier,
  and nothing executes alongside it. Commands merely being queued may still be
  admitted beside an append, since queueing runs nothing.
- A reply is released only once the log covers the offset reached when it was
  produced, so a reader cannot observe a transaction's effects before the
  append holding them.

## Persistence

### Framing

A transaction's records are written between `MULTI` and `EXEC`, as Redis
writes its AOF. `MULTI` is written lazily, before the block's first record; a
transaction that records nothing, such as one made of reads, writes no frame,
because the log grows with changes rather than traffic. Records follow the
existing canonical rules, so the block holds, for example, `PEXPIREAT` for a
relative expiry and the `SREM` an `SPOP` turned out to be. Keys a command
inside the transaction reaps on lazy expiry are deleted inside its block.

Eviction waits until the block is closed: its `DEL` records follow `EXEC`, so
a block holds what its transaction did and not the removal of an unrelated key.
Redis evicts before `EXEC` rather than between its commands for the same
reason. Meanwhile the memory budget can be exceeded by up to one transaction,
which the queue limit below bounds.

Under `appendfsync always`, `EXEC`'s reply is staged with the rest of the
cycle's replies and released only after the flush that syncs the whole block,
or, with worker appends, after the worker's write and sync cover the reply's
append offset. A block larger than the 4 MiB transcript buffer drains in
bounded fragments without a sync, exactly as one large command does.

### Torn tails and malformed frames

Startup holds a block until its `EXEC` has been read and only then replays it.
A crash that leaves a block open, at any byte of it, is a torn tail: the log is
copied from that block's `MULTI` to a `.keel-torn-tail-*` file beside it and
truncated to just before the `MULTI`, so none of the transaction is applied.
`EXEC` without `MULTI`, `MULTI` inside an open block, or either with arguments
cannot come from a crash; they are reported as damage and prevent startup, like
any other malformed record.

### Rewrites

A rewrite emits current key state, never frames. It walks the keyspace as it
stands and rewrites every key written after it began from that key's state at
the end, so a rewrite started by `BGREWRITEAOF` part way through a transaction
captures the writes before it in the walk and those after it as dirty keys,
while the old log, which stays authoritative until the handoff, holds the block
whole. The handoff itself happens between event-loop cycles, never inside
`EXEC`, so a rewritten log holds whole transactions or none of them. Under
`-aof-async-append`, `BGREWRITEAOF` after a write in the same batch returns its
existing retry error as its element, inside a transaction or not.

### Upgrade and rollback

A build without transactions stops on the first frame it replays
(`ERR unknown command 'MULTI'`). Before rolling back a node whose log may hold a
transaction, complete a `BGREWRITEAOF` (its output has no frames) or restore the
pre-upgrade backup. Logs written by older builds replay unchanged.

## Replication

### Protocol 2

The stream frames a transaction's block with `MULTI` and `EXEC` as the log
does. Between them are the bodies the commands published, canonical operations
or, for probabilistic structures, exact replacement images. A replica parses a
block as it arrives, across as many frames as it spans, holds it back from the
keyspace, and applies the whole of it in one turn of its event loop once `EXEC`
arrives. Reads in between see the state before the transaction. The replica
frames the block in its own log as well. A frame that confirms catch-up while a
block is open, `EXEC` without `MULTI`, or a nested `MULTI` fails application.

An open block counts against the replica's 64 MiB limit on held stream. A block
can only be delivered from the 16 MiB history, so the primary invalidates its
epoch when a block outgrows the history, and keeps the rest of that block out
of the new epoch. Every replica then takes a fresh snapshot, which already
contains the transaction. Opaque images are where this happens in practice: a
filter publishes its whole image for each command that changes it.

A replica refuses write transactions as it refuses writes: each queued write
is answered `READONLY replica rejects writes`, and `EXEC` answers `EXECABORT`.
Transactions of reads run on a replica with recent primary state; a stale
replica refuses reads while queueing, or at `EXEC` with `EXECABORT ... because
of: MASTERDOWN ...`.

Older protocol 2 replicas stop on the first block they receive
(`invalid replication operation MULTI`). Upgrade replicas before their primary.

### Protocol 1

Protocol 1 sends key images sealed when a replica pulls, and a pull runs between
commands, never inside `EXEC`, so a transaction's keys are always sealed into
the same batch. A replica applies a frame whole, so it receives a transaction
atomically without any change to the protocol.

## Limits and admission

A connection may hold at most 16 MiB of queued commands, measured the same way
parsed input is - 64 bytes plus the name, 16 bytes per argument and the argument
bytes - plus 16 bytes for each command's place in the queue. That is also the per-client limit on incomplete input. A command that
would pass it is refused with `ERR transaction exceeds the 16 MiB queued command
limit`, which aborts the transaction and releases the queue; until `EXEC` or
`DISCARD`, later commands are checked and answered but not kept.

Queued commands are retained input. They count toward the 192 MiB input class
and the 256 MiB aggregate of retained client buffers, connections over the
aggregate are closed as before, and `INFO clients` reports them in
`retained_input_bytes`. Request allocation admission applies unchanged when the
commands are parsed.

`EXEC` answers with one array, bound by the 64 MiB per-client output limit.
Each amplifying reply inside it - the reads and pops covered by
[reply](reply-admission.md) and [collection reply](collection-reply-admission.md)
admission, `KEEL.DUMP`, `GEOSEARCH`, `SCAN` and `KEYS` - is admitted against what
the replies before it left, less 128 bytes for each command still to run, and
is refused before construction with `ERR reply exceeds the 64 MiB output limit`
as its element, while the transaction continues. A pop's refusal comes before
it removes anything. If small replies still add up past the limit, the
transaction runs whole, because half of one must never run, its replies are
dropped and the connection is closed, which is what any reply over the output
limit already costs; the client cannot tell whether it ran.

[Command allocation reservations](command-allocation-reservations.md) accumulate
across all of a transaction's commands, which form one execution run, and later
commands observe the log growth and replies retained by earlier ones. Joining
the array is one more copy of its replies, outside the reservation and within
the 64 MiB output limit.

A transaction runs without yielding. A large one delays every other client for
its whole duration, as in Redis; split bulk loads that do not need atomicity.

Transactions require the event loop (the default `-mode kqueue`). The benchmark
`-mode net*` variants do not implement them.

## Validation

Unit tests cover queueing and execution order, every queue-time refusal, the
connection's own commands queued and run in place,
runtime errors inside `EXEC`, nested and stray control commands, the queue limit
and its release, the arity table against every handler, the reply ceiling and
an undeliverable reply, replica and fenced-primary refusals, a rewrite started
inside a transaction, log framing for
writes, reads, failed writes and lazy expiry, eviction after the block, torn
tails at six positions inside an open block with their backups, malformed
frames, protocol 2 delivery of a block split over several frames with an
opaque image inside it, replica log framing and replay, invalidation by an
oversized block, malformed replica blocks, and protocol 1 sealing.

Process tests cover a transaction pipelined in one write, `EXECABORT`,
`DISCARD`, a nested `MULTI` and two restarts under each persistence mode
(`always`, `everysec`, `no`, worker and concurrent appends); a disconnect
releasing a 4 MiB queue; three readers that never observe half of a
transaction split over two writes, with AOF off, four I/O threads, `everysec`
and concurrent appends; a crash followed by a torn block cut before its `MULTI`
over two restarts; and replicas over both protocols that never apply half a
transaction and refuse a write transaction.

The seeded Redis differential mixes `MULTI` blocks into its operations, with
queue-time refusals, nested `MULTI` and `DISCARD`, and compares every reply,
including each element of `EXEC`'s array by error class. A local 4,000-step run
against Redis 8.10.1 passed with 399 transactions, its state checks and two
crash restarts, and the hosted 20,000-step run against Redis 7.0.15 passed with
1,985. The [client-library matrix](client-library-compatibility.md)
now runs each library's transaction API; all 35 invocations passed locally.
Hosted results are recorded on the pull request.
