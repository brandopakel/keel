# How Redis 8.10.1 does what Keel's phase 4 reproduces

Status: October 12, 2026, corrected October 13 after every citation was
checked against the source. A reading of the Redis 8.10.1 source, kept as the
reference for phase 4 (`docs/embedding-plan.md`: the op-descriptor table,
typed operations, the `Reply` sink) and for tier 2 of the parity plan
(`docs/redis-parity-plan.md`: blocking commands, Pub/Sub, push messages).

Every claim cites `file:line` in the Redis 8.10.1 source tree, the tarball
with sha256 `60166c95ab7aedaa9dfe516de685be0a4dd87be95ded59ba429df14c13f1b663`
that every parity workflow builds (`src/` is implied unless a path says
otherwise). A bare `:line` is in the file of the citation before it. The Redis source is not in this repository; the line numbers are
for that release and move with the next one. Where the source leaves a
question open that only a running `redis-server` settles, the question is
listed at the end for the REA toolkit (`github.com/morluto/rea`) to be
pointed at later.

## The command table

Redis's table is generated. `src/commands/*.json` (459 files: one per
command and subcommand) is the specification: `arity`, `command_flags`,
`acl_categories`, `key_specs`, `arguments`, `reply_schema`, `history`,
`since`. `utils/generate-command-code.py` turns them into `src/commands.def`
(`commands.def:1`, "Automatically generated"), whose `redisCommandTable[]`
(`commands.def:12798`) holds one `MAKE_CMD(...)` per command, for example
`SET` at `commands.def:13094`: name, summary, complexity, since, doc flags,
group, history (`SET_History`, `commands.def:12529`), tips, the handler
`setCommand`, arity `-3`, flags `CMD_WRITE|CMD_DENYOOM`, ACL category, the
key specs (`SET_Keyspecs`, `commands.def:12545`: `RW|ACCESS|UPDATE|
VARIABLE_FLAGS`, begin at index 1, range `{0,1,0}`), the key-extraction
function `setGetKeys`, and the argument tree `SET_Args`. A subcommand is a
`MAKE_CMD` in its container's `subcommands` array (`CONFIG SET` at
`commands.def:8075`, handler `configSetCommand`, arity `-4`).

The runtime struct is `struct redisCommand` (`server.h:3055-3107`): the
declared name, `proc` (`:3070`), `arity` (`:3071`, `-N` means at least `N`
words including the name), `flags` (`:3072`), `acl_categories` (`:3073`),
`key_specs` and their count (`:3074-3075`), `getkeys_proc` (`:3078`),
`args` (`:3083`), `reply_schema` only in `LOG_REQ_RES` builds (`:3084-3087`),
the per-command statistics `microseconds, calls, rejected_calls,
failed_calls` and the slowlog counters (`:3090-3091`), the `id` used by ACL
bitmaps (`:3092`), `fullname` (`:3097`, `config|set`), the latency
histogram (`:3098`), the legacy `(first,last,step)` key spec (`:3099`),
`subcommands_dict` (`:3103`) and `parent` (`:3105`).

Keel's op-descriptor table is generated from the same JSON. The fields phase
4 needs from it are the ones Redis reads around running a command: arity,
the key specs, and the flags. `processCommand`
(`server.c:4455-4836`) reads `WRITE`, `READONLY`, `DENYOOM`, `NO_AUTH`,
`NO_MULTI`, `STALE`, `LOADING`, `MAY_REPLICATE`, `ALLOW_BUSY`, `INTERNAL`,
`PROTECTED`, `MOVABLE_KEYS` and `NO_ASYNC_LOADING`. The others are
read elsewhere: `ADMIN` and `SKIP_MONITOR` in `call` (`:4120`),
`SKIP_SLOWLOG` in `slowlogPushCurrentCommand` (`:3816`),
`TOUCHES_ARBITRARY_KEYS` in `propagatePendingCommands` (`:3868`), `ADMIN`
and `BLOCKING` for the ACL categories (`:3460-3467`), and `NOSCRIPT` by
scripts (`script.c:652`).

## Looking a command up

`lookupCommandLogic` (`server.c:3633-3647`) fetches `argv[0]` from the
`server.commands` dict (names are stored lower case; the dict hashes and
compares without regard to case, `commandTableDictType`, `server.c:678-681`)
and, when the command has subcommands and `argc > 1`, looks
`argv[1]` up in `subcommands_dict` (`:3645`); a container called with one
word returns the container itself, whose `proc` is `NULL` (`:3639`).
`lookupCommandBySds` (`:3677-3679`, through `lookupCommandBySdsLogic`, `:3653-3675`) splits a `container|sub` name on `|` for
ACLs, `COMMAND INFO` and the like. `lookupCommandOrOriginal` (`:3701-3706`)
also checks the table of names before `rename-command`. `mustObeyClient`
(`:3709-3711`) is true for the AOF loader and the master link: those two
clients' commands are never rejected.

## processCommand: the checks, in order

`processCommand` (`server.c:4455-4836`) runs once per command (or again, for
a command re-executed after a block, with the first checks skipped: the
flag is set at `:4467` and tested at `:4470` and `:4488`). In order:

1. **Lookup** (`:4493-4498`): the previous command's struct is reused when
   `isCommandReusable` says the name is the same, otherwise `lookupCommand`.
2. **`host:` and `post`** (`:4503-4505`): an HTTP request on the port gets a
   security warning and the connection closed.
3. **Internal commands** (`:4511-4513`) are hidden from ordinary clients.
4. **Existence** (`:4517`; `commandCheckExistence`, `:4336-4372`): the
   `unknown command` and `unknown subcommand` messages, with up to 128 bytes
   of the arguments echoed (`:4360-4365`) and CR/LF in them replaced by
   spaces (`:4370`). Keel's `docs/error-replies.md` has the byte-level rules.
5. **Arity** (`:4521`; `commandCheckArity`, `:4376-4386`).
6. **Protected commands** (`:4528-4540`): `DEBUG` and `MODULE` need
   `enable-debug-command` / `enable-module-command`.
7. **The command's effective flags** (`:4543-4558`): for `EXEC` they are the
   union of the queued commands' flags (`c->mstate.cmd_flags`), so a
   transaction holding a write is a write.
8. **Authentication** (`:4561-4568`): `NOAUTH` unless the command carries
   `CMD_NO_AUTH` (`AUTH`, `HELLO`, `QUIT`, `RESET`).
9. **`CMD_NO_MULTI` inside `MULTI`** (`:4570-4573`): `Command not allowed
   inside a transaction`.
10. **ACL** (`:4577-4585`; `ACLCheckAllPerm`, `acl.c:1944`): `NOPERM`, with
    an ACL log entry.
11. **Cluster redirection** (`server.c:4591-4610`): `MOVED`/`ASK`, which also marks
    the transaction dirty (`flagTransaction`, `:4603`).
12. **Client eviction** by `maxmemory-clients` (`:4630`).
13. **Memory** (`:4642-4666`): with `maxmemory` set, `performEvictions`
    runs before the command; if it returns `EVICT_FAIL` and the command is
    `DENYOOM`, `OOM command not allowed when used memory > 'maxmemory'`
    (`:4655-4657`). The outcome is kept in `pre_command_oom_state`
    (`:4665`) so scripts can refuse writes mid-way.
14. **Disk errors** (`:4675-4698`): `writeCommandsDeniedByDiskError`
    (a failed `BGSAVE` with `stop-writes-on-bgsave-error`, or an AOF write
    error) refuses writes and `PING` with `MISCONF`. Keel adopted this in
    #159.
15. **`min-replicas-to-write`** (`:4702-4705`): `NOREPLICAS`.
16. **Read-only replica** (`:4709-4715`): `READONLY You can't write against
    a read only replica.`
17. **Subscribed RESP2 connection** (`:4739-4754`): only `(P|S)SUBSCRIBE`,
    `(P|S)UNSUBSCRIBE`, `PING`, `QUIT` and `RESET`. A RESP3 connection has
    no such mode (`c->resp == 2` in the test).
18. **Stale replica** (`:4759-4765`): `MASTERDOWN` when
    `replica-serve-stale-data` is off and the command is not `STALE`.
19. **Loading** (`:4769-4778`): `LOADING`.
20. **Busy script or module** (`:4787-4798`): `BUSY`.
21. **A replica client touching the keyspace** (`:4803-4806`).
22. **Client pause** (`:4810-4816`): the client is postponed, not refused.
23. **`MULTI` queueing** (`:4819-4828`): inside `MULTI`, every command but
    `EXEC`, `DISCARD`, `MULTI`, `WATCH`, `QUIT` and `RESET` is queued
    (`queueMultiCommand`, `multi.c:35-75`) and answered `+QUEUED`.
24. **`call`** (`server.c:4830-4831`), then `handleClientsBlockedOnKeys` if any key
    became ready (`:4832-4833`).

Every refusal goes through `rejectCommand` / `rejectCommandSds` /
`rejectCommandFormat` (`:4277-4311`): the transaction is marked dirty
(`flagTransaction`, `multi.c:86-89`), `rejected_calls` is counted (`server.c:4280`),
and when the refused command is `EXEC` itself the reply is
`EXECABORT Transaction discarded because of: ...` (`execCommandAbort`,
`multi.c:115-125`). `rejectCommandFormat` maps CR/LF to spaces (`server.c:4309`).
The one exception is cluster redirection (step 11, `:4599-4608`), which does
not go through them: it discards the transaction for `EXEC` (so no
`EXECABORT`) or flags it otherwise, sends `MOVED`/`ASK` with
`clusterRedirectClient`, and counts `rejected_calls` itself.

Keel's order (`docs/error-replies.md`) matches steps 1, 4, 5, 8, 9, 16 and
23; steps 10, 11, 13 (as `OOM`), 14, 15, 18 and 19 are the ones tier 1 and
later add, and the documented departure is step 13 (see "Improvements").

## call: timing, statistics, propagation

`call` (`server.c:3992-4269`) wraps the handler:

- statistics are not updated while loading the AOF (`:4001`);
- the per-call `FORCE_AOF`, `FORCE_REPL` and `PREVENT_PROP` flags are
  cleared before the handler and restored after (`:4011`, `:4201-4203`);
- `server.dirty` is snapshotted (`:4021`) and the delta after the handler
  decides propagation (`:4075-4076`, `:4174`);
- the clock: a hardware monotonic clock when available (`:4026`), with the
  cached wall time refreshed when more than 10 µs or 25 calls have passed
  (`:4034-4043`), and the call timer taken at `:4049`; the execution unit is
  entered at `:4050` and left at `:4060`;
- the handler runs at `:4058`;
- the duration (`:4068-4074`) feeds the latency monitor (`:4102-4108`,
  `fast-command` for `CMD_FAST`), the slow log (`:4112-4113`;
  `slowlogPushCurrentCommand`, `:3814-3833`, skipping `CMD_SKIP_SLOWLOG` and
  using the original argv when the command rewrote itself), the per-command
  `calls` and `microseconds` and the latency histogram (`:4129-4134`), and
  `failed_calls` when the handler answered an error (`:4080-4086`);
- `MONITOR` clients are fed at `:4119-4125`, with the original argv, unless
  the command is `SKIP_MONITOR` or `ADMIN`;
- propagation (`:4165-4196`): when `dirty` grew, or the handler forced it,
  the command is queued for AOF and replication with `alsoPropagate`
  (`:4196`), never for `EXEC` itself (`:4167`: the queued commands propagate
  one by one, and `propagatePendingCommands` frames them);
- a read-only command remembers its keys for client tracking
  (`:4208-4219`);
- `stat_numcommands` and the client's `commands_processed` (`:4222-4229`);
- `afterCommand` (`:4232`; `:4314-4332`): `postExecutionUnitOperations`
  (`:3907-3922`: at nesting zero, propagate what accumulated and flush the
  replicas' buffers), pending tracking invalidations (`:4320`), and the
  push messages that arrived during the command are appended after its
  reply (`:4326-4327`).

Keel's command path already measures the duration and counts `calls` and
`failed_calls` as INFO part (c) wants; what phase 4 keeps from here is the
single place where propagation is decided: after the handler, from `dirty`
plus the force/prevent flags, with the handler never writing to the log
itself.

## Replies

**Where bytes go.** `_prepareClientToWrite` (`networking.c:312-344`) is
the gate: script and module clients collect replies elsewhere (`:316`), a
client being closed, one in `CLIENT REPLY OFF|SKIP` (unless pushing), the
master link and the AOF loader's fake client get nothing (`:319-331`); a
client with no pending reply is put in the pending-write queue (`:339-340`).
`_addReplyToBufferOrList` (`:514-549`) counts the bytes against the current
command (`:528`), diverts a push produced while another command runs into
`pending_push_messages` (`:539-544`), and fills the client's buffer (`PROTO_REPLY_CHUNK_BYTES`, 16 KiB at
first, `server.h:191`; the cron resizes it, `server.c:972-1014`) before the
reply list
(`networking.c:546-548`), whose blocks are 16 KiB or the payload's size
(`_addReplyPayloadToList`, `networking.c:397-453`). A large bulk string need not be copied:
`_addBulkStrRefToBufferOrList` (`:573-598`) queues a reference to the object
and formats the `$len\r\n` prefix at write time (`:561-569`), where
`isCopyAvoidPreferred` (`:1297-1321`) allows it: copy avoidance enabled, a
normal client, not a push, a RAW string that is not shared; then any length
with 7 or more I/O threads, at least 16 KiB with none, and at least 64 KiB
with fewer than 7 (`server.h:201-203`).

**Deferred lengths.** A handler that does not know the length of an
aggregate in advance calls `addReplyDeferredLen` (`networking.c:928-952`), which plants
a `NULL` node in the reply list (`:950`), and `setDeferredReply`
(`:954-1016`) later writes the header into the previous block if it has
room (`:974-989`), otherwise ahead of the next block's data (`:991-1001`),
otherwise in a block of its own (`:1003-1014`); `setDeferredAggregateLen`
(`:1019`) takes the prefix as an argument, and `setDeferredMapLen` and
`setDeferredSetLen` (`:1053-1062`) choose it by protocol. Keel sizes most replies before
building them (`docs/reply-admission.md`); where it cannot, this is the
model.

**RESP3.** Every type that differs between protocols has one function:
`addReplyNull` (`:1259-1265`, `$-1` or `_`), `addReplyNullArray`
(`:1279`, `*-1` or `_`), `addReplyBool` (`:1267-1273`, `:1`/`:0` or `#t`/
`#f`), `addReplyDouble` (`:1075`, a bulk string or `,`), `addReplyMapLen`
(`:1237-1241`, `*` with the length doubled, or `%`), `addReplySetLen`
(`:1243-1246`, `*` or `~`), `addReplyAttributeLen` (`:1248-1251`, RESP3
only), `addReplyPushLen` (`:1253-1257`, RESP3 only and only while
`CLIENT_PUSHING`), `addReplyVerbatim` (`:1418`). Keel's `internal/core/
resp3.go` is the same split; `scripts/reply-census.py` checks the result
for every reply against Redis and against `reply_schema`.

**Writing.** `handleClientsWithPendingWrites` (`:2958`), from `beforeSleep`
(`server.c:1963`, `:2104`), hands clients to I/O threads when there are any
(`networking.c:2982-2988`) or writes inline. `writeToClient` (`:2847-2946`) stops after
`NET_MAX_WRITES_PER_EVENT` (64 KiB, `server.h:125`) per event, once a write takes it past that, for a
normal client unless the server is over `maxmemory` (`networking.c:2887-2890`), frees a
client on a write error (`:2896-2902`), uninstalls the write handler when
the buffers drain (`:2917-2920`) and then closes a `CLOSE_AFTER_REPLY`
client (`:2923-2926`).

**Output buffer limits.** `checkClientOutputBufferLimits` (`:5376-5427`)
applies the class's hard limit and the soft limit held for its seconds, and
also closes an unauthenticated client holding more than 1 KiB of reply
(`:5382-5383`); `closeClientOnOutputBufferLimitReached` (`:5440-5466`)
frees the client, asynchronously when called with `async` and at once from
the cron (`:5450-5456`), and counts
`client_output_buffer_limit_disconnections`. Keel caps one reply at 64 MiB
instead (`docs/info-compatibility.md`), a documented difference.

**Input limits**, for completeness: a bulk longer than `proto-max-bulk-len`
is a protocol error (`:3388-3391`), an unauthenticated client may not send a
bulk over 16 KiB (`:3392-3394`), a bulk of 32 KiB or more is read straight
into its own buffer (`PROTO_MBULK_BIG_ARG`, `:3398`), the error texts are at
`:3636-3651`, and the query buffer, counting the queued `MULTI` arguments,
is bounded by `client-query-buffer-limit` and by 1 MiB before
authentication (`:4030-4042`). `docs/error-replies.md` lists these as a
hardening change Keel has not adopted.

## Propagation: the log and the replicas

**Queueing.** `alsoPropagate` (`server.c:3772-3785`) copies the argv into
`server.also_propagate`, after `shouldPropagate` (`:3713-3727`: not while
loading, only when the AOF is on or a replica or backlog exists). Nothing
reaches the log until `propagatePendingCommands` (`:3851-3891`), called from
`postExecutionUnitOperations` at nesting zero (`:3915`): when more than one
operation accumulated during one execution unit, they are framed in
`MULTI` and `EXEC` (`:3861`, `:3876`, `:3887`), unless the command touches
arbitrary keys (`:3866-3871`). A command whose handler expired a key
therefore logs `MULTI`, the deletion, itself, `EXEC`. An eviction is never
framed with the command: it runs in `processCommand` before `call` (step
13), and each evicted key is an execution unit of its own
(`evict.c:684-690`; `also_propagate` is asserted empty on entry, `:564`),
so its `DEL` or `UNLINK` is logged alone, ahead of the command.
`propagateNow` (`server.c:3744-3759`) feeds the AOF (`:3753-3754`) and the replicas
(`:3755-3757`).

**The log line.** `feedAppendOnlyFile` (`aof.c:1661-1700`) prepends a
`#TS:` annotation once a second when `aof-timestamp-enabled` (`:1667-1673`),
a `SELECT` when the database changed (`:1677-1684`), then the command as a
RESP array of bulk strings (`catAppendOnlyGenericCommand`, `:1609-1632`),
into `server.aof_buf` (`:1693-1697`).

**Writing and syncing.** `flushAppendOnlyFile` (`:1399`), from `beforeSleep`
(`server.c:1962`, `:2057`) and the cron, writes the buffer with `aofWrite`
(`aof.c:1470`). Under `everysec`, while a background fsync is still running the
write itself is postponed, up to two seconds (`:1438-1457`), after which it
proceeds and `aof_delayed_fsync` is counted (`:1455`). A short write is
handled at `:1489` onward. Under `everysec` and `no` it ends in the
`MISCONF` state read by `processCommand` step 14; under `always` Redis logs
that it can't recover and exits (`:1531-1538`).
`rewriteAppendOnlyFileBackground` (`:3197-3270`) flushes (`:3217`), opens a new incremental file (`:3218`) and
forks a child (`:3239`) that writes a temporary base file (`:3245-3246`).

**Expires are logged absolute.** `EXPIRE`, `PEXPIRE` and `EXPIREAT` rewrite
themselves to `PEXPIREAT` with the absolute millisecond time
(`expire.c:826-834`); an expiry already in the past becomes `DEL` or
`UNLINK` by `lazyfree-lazy-expire` (`:807-816`). `SET` with `EX`/`PX`
rewrites to `PXAT` (`t_string.c:193-208`) and drops `GET` from what it
propagates (`:217-227`). The rewrite emits `PEXPIREAT` after each key with
an expire (`aof.c:2999-3004`). Keel's log already records absolute expiries;
phase 4's transcript goldens are the test that it keeps doing so byte for
byte.

## Expiry

**Lazy.** `keyIsExpired` (`db.c:2941-2950`) compares the key's expire with
`commandTimeSnapshot()`, the time cached when the command started
(`:2946`), so one command, however long, sees no key expire half way.
`expireIfNeeded` (`:2999-3068`) returns `KEY_VALID` for the master link
(`:3039`), `KEY_EXPIRED` without deleting on a replica (`:3040`), while
nested inside a command that touches arbitrary keys only with the hidden
`lazyexpire-nested-arbitrary-keys` turned off (`:3045`; it is on by
default, `config.c:3337`, and such keys are then deleted), when the
caller asked not to delete (`db.c:3050`) or during a pause (`:3056`), and
otherwise deletes (`:3059-3066`). The deletion (`deleteKeyAndPropagate`,
`:2839-2887`) is lazy or not by `lazyfree-lazy-expire` (`:2842`), fires the
`expired` keyspace event (`:2876`), touches watchers and tracking
(`keyModified`, `:1190-1196`) and propagates `DEL` or `UNLINK`
(`propagateDeletion`, `:2918-2935`, with replication forced on). So a
read-only replica never deletes an expired key on its own: it waits for the
primary's `DEL`. A writable replica does delete one that a write looks up
(`EXPIRE_FORCE_DELETE_EXPIRED`, `:292-295`, which skips the replica's
`KEY_EXPIRED` at `:3040`), and its cron expires the keys its own clients
gave a TTL (`expireSlaveKeys`, `server.c:1317`).

**Active.** `activeExpireCycle` (`expire.c:287`) runs slow from
`databasesCron` (`server.c:1310-1315`, from `serverCron`, `:1685`) and fast
from `beforeSleep` (`server.c:2009`). The parameters scale with
`active-expire-effort` (`expire.c:291-300`): 20 keys per loop, a 1000 µs fast
cycle, 25% of the CPU for the slow cycle, 10% acceptable stale keys
(`:96-99`). A fast cycle runs only if the previous cycle, fast or slow,
hit its time limit or the stale estimate is high, and never within twice
its duration of the last fast one (`:318-330`). The slow cycle's time limit is
`25% × 1 s / hz` (`:347`); per database it samples up to 20 keys with
expires (`:412-413`), repeats while more than 10% of a sample were expired
(`:447`), and stops at the time limit (`:484-485`); each cycle's stale share
is `total_expired / total_sampled` (`:502`), folded into the reported
estimate as a 5% running average (`:505-506`). Each expired key goes through
`activeExpireCycleTryExpire` (`:40-54`): delete and propagate, count it
in `expired_keys_active` (`stat_expiredkeys_active`, `:48`; INFO prints it
beside `expired_keys` at `server.c:6762-6763`), and propagate at once
(`postExecutionUnitOperations`, `expire.c:52`), so the log holds one `DEL` per key
rather than a transaction.

Keel samples `-active-expire-samples` keys per cron tick with no CPU cap
(`docs/info-compatibility.md`, `avg_ttl` waits on this). The configuration
census maps `active-expire-effort` to that flag under another meaning.

## Eviction

`performEvictions` (`evict.c:532-764`) returns `EVICT_OK` at once when
memory is under the limit (`:544-547`) and `EVICT_FAIL` under `noeviction`
(`:549-552`); otherwise, under a time limit (`:554`), it frees until
`mem_tofree` is reached (`:567`). For the LRU, LFU and TTL policies it
fills an eviction pool of 16 entries (`EVPOOL_SIZE`, `:36`) from each
database: `evictionPoolPopulate` (`:134`) draws `maxmemory-samples` keys
from one random dict (`:141`), scores each by idle time (LRU, `:152-153`),
`255 − LFU counter` (`:162`) or `ULLONG_MAX − expire` (`:165`), and inserts
it into the pool by score (`:173-`). The loop over databases stops sampling
a database after `maxmemory-samples` keys or when it holds fewer than ten
times that (`:605-611`); with no keys at all it gives up (`:614`), and
**with keys but an empty sample it gives up too** (`:618`). The best key is
the pool's last (`:621-647`); the random policies take a random key from a
random dict (`:652-677`). The victim is deleted and propagated as `DEL` or
`UNLINK` (`deleteEvictedKeyAndPropagate`, `:686`; `db.c:2895-2897`), and
every 16 keys the replicas' buffers are flushed (`evict.c:695-700`). When
it gives up with nothing it could evict, it first waits for pending lazy
frees, within the same time limit, and returns `EVICT_OK` if memory falls
under the limit meanwhile (`:731-748`). What counts
toward `maxmemory` is `zmalloc_used_memory()` less the replica output
buffers and the AOF buffer (`freeMemoryGetNotCountedMemory`, `:318`;
`getMaxmemoryState`, `:384`).

## Transactions and WATCH

`MULTI` (`multi.c:91-99`) sets `CLIENT_MULTI`; a nested `MULTI` is an
error. `queueMultiCommand` (`:35-75`) moves the parsed argv into
`c->mstate`, accumulates the queued commands' flags (`:62-63`) and does
nothing once the transaction is dirty (`:40-41`). `EXEC` (`:127-258`):
without `MULTI` an error (`:134-137`); a watched key that expired since
`WATCH` makes the transaction dirty (`:140-142`); a dirty transaction
answers `EXECABORT` if a queued command was refused, or a null array if a
watched key changed (`:150-158`); then `CLIENT_DENY_BLOCKING` is set so no
queued command blocks (`:164`), keys are unwatched (`:167`), `in_exec` is
set (`:169`), the array header is sent for the count (`:185`), and each
command is re-checked against the ACL (`:194-221`) and run with `call`
(`:223-228`; `CMD_CALL_NONE` for the AOF loader, so nothing propagates
twice). The commands propagate one by one and `propagatePendingCommands`
frames them in `MULTI`/`EXEC` because more than one accumulated.

`WATCH` (`watchForKey`, `:302-333`) links the client into
`db->watched_keys` and records whether the key was already expired
(`:329`); `touchWatchedKey` (`:387-426`), from `keyModified`, marks every
watcher dirty (`:417`) except where a watcher saw the key expired at `WATCH`
time (`:403-414`): if the key is now gone, that watcher is skipped and stays
clean (`:412`); if it still exists, the walk stops there (`break`, `:414`),
leaving that watcher and every later watcher of the key clean.
`isWatchedKeyExpired` (`:365-378`) is what `EXEC` checks. Keel's
`docs/transactions.md` states why `WATCH` is still an unknown command; tier
1 adds it, and `keyModified` is the hook: every write, expiry and eviction
touches watchers through it.

## Blocking clients (tier 2)

A command that must wait calls `blockForKeys` (`blocked.c:399-450`): the
keys are recorded in `c->bstate.keys` and the client appended to each key's
list in `db->blocking_keys` (`:418-429`), with `unblock_on_nokey` for the
commands that wake on deletion (`:433-441`); the client keeps its command
pending (`CLIENT_PENDING_COMMAND`, `:447-448`) and `blockClient` (`:74-87`)
sets `CLIENT_BLOCKED`, counts it and registers its timeout (`:86`). A write
that makes a key ready calls `signalKeyAsReady` (`:600-602`), which puts the
key on `server.ready_keys`; after every command, `processCommand` drains
that list with `handleClientsBlockedOnKeys` (`server.c:4832-4833`;
`blocked.c:346-390`, reentrancy-guarded at `:350-353`) and serves the
blocked clients of each key in arrival order (`handleClientsBlockedOnKey`,
`:620`). **A woken client re-executes its command**: `unblockClientOnKey`
(`:725-774`) unblocks it, flags `CLIENT_REEXECUTING_COMMAND` (`:745`) and
runs `processCommandAndResetClient` again (`:754`), so the pop that happens
is the ordinary pop, counted and propagated as one; if the command blocks
again it waits again. Timeouts come from `blockedBeforeSleep`
(`:825-830`) through `unblockClientOnTimeout` (`:804-812`), which sends the
command's timeout reply (a null); `unblockClientOnError` (`:816-823`) sends
an error. `updateStatsOnUnblock` (`:94-110`) adds the blocked time to the
command's duration for the slow log and the histogram. Inside `MULTI`,
`CLIENT_DENY_BLOCKING` (`multi.c:164`) makes the blocking commands answer
as their non-blocking forms.

For Keel this says: a queue of waiting clients per key in arrival order; a
"ready keys" set filled by writes and drained after each command, not inside
the write; re-running the command rather than a second code path for the
wake-up; timeouts checked before each sleep, as `blockedBeforeSleep` does; and the log recording the pop, never the
wait. The parity plan's tier 2 description matches.

## Pub/Sub and push (tier 2)

`SUBSCRIBE` marks the client (`markClientAsPubSub`, `pubsub.c:229-234`,
`CLIENT_PUBSUB`), which in RESP2 restricts the connection (`processCommand`
step 17) and in RESP3 does not. Every message is framed the same way in
both protocols except the header: `addReplyPubsubMessage` (`:93-104`) sends
`*3` in RESP2 and `>3` in RESP3, with `CLIENT_PUSHING` set around it so the
reply gate lets it through (`:95`, `:103`); pattern messages are four
elements (`:109-`), subscribe and unsubscribe confirmations three
(`:124-155`). `PUBLISH` (`pubsubPublishMessageInternal`, `:469-530`) walks
the channel's client dict (`:479-495`), then every pattern (`:503-528`),
and returns the receiver count. A push produced while another command is
executing on the same connection is held in `pending_push_messages` and
appended after that command's reply (`networking.c:539-544`;
`server.c:4326-4327`).

## Client-side tracking (tier 4)

Read commands remember their keys per tracking client (`call`,
`server.c:4208-4219`; `trackingRememberKeys`, `tracking.c:203`); a write
invalidates through `keyModified` (`db.c:1194`), and `sendTrackingMessage`
(`tracking.c:259-332`) sends a RESP3 push `invalidate` with the key
(`:296-298`, `:313-318`), or in RESP2 a Pub/Sub message on the
`__redis__:invalidate` channel to the redirect client (`:299-302`), and a
`tracking-redir-broken` push when that client is gone (`:267-279`). Pending
invalidations are flushed after each command (`:433`, from `afterCommand`).

## Candidate improvements over Redis

Each is a Redis behaviour that looks like an accidental flaw, with the code
that produces it, for the owner to decide under the parity plan's rule
(keep a clear improvement, document it as a departure citing the source, and
list it under "Improvements over Redis"). None is adopted by this document.

1. **`OOM` while evictable keys remain.** `performEvictions` gives up when
   one round samples no key (`evict.c:618`), which a sparse, once-grown
   table can produce, and `processCommand` then refuses the write
   (`server.c:4655-4657`). Already adopted and measured
   (`docs/redis-parity-plan.md`, "Where Keel improves on Redis").
2. **Acknowledged writes held back from the log for up to two seconds.**
   Under `appendfsync everysec`, while a background fsync runs,
   `flushAppendOnlyFile` postpones the `write(2)` itself, not only the
   fsync, for up to two seconds (`aof.c:1438-1457`), so the bytes of
   commands already answered sit in `aof_buf`, and a crash in that window
   loses up to three seconds rather than one. Keel already appends before
   it replies and syncs on the clock in a background worker (`README.md`,
   the `everysec` line of the durability table), so it has no such window;
   the candidate is to document that as a departure.
3. **A slow but live reader is disconnected.** The output buffer limits
   close the client (`networking.c:5447-5463`) rather than stop reading
   from it. Keel caps one reply and applies admission
   (`docs/reply-admission.md`); whether to also disconnect is the decision.
4. **`SET ... PX <large>` overflows by compiler.** `getExpireMillisecondsOrReply`
   checks the range only for seconds (`t_string.c:250`); milliseconds are
   added to the current time (`:258-262`) and a value near `LLONG_MAX`
   overflows, which is undefined, so Homebrew's build answers `OK`
   (`docs/error-replies.md`). Keel refuses the expiry.
5. **`SRANDMEMBER` with a huge negative count answers as many elements as
   asked.** `srandmemberWithCountCommand` takes any `long` (`t_set.c:1141-1149`)
   and, with repetition allowed, replies `count` elements; a count of a
   billion is a billion bulk strings. Keel bounds it at 16 million
   (`docs/error-replies.md`, a known difference).
6. **`LCS` is bounded by `proto-max-bulk-len` in bytes of table.** The
   table is `(alen+1)·(blen+1)` 32-bit cells (`t_string.c:1489-1496`), refused
   only above `proto-max-bulk-len` (512 MiB by default): two 11,000-byte
   strings cost about 462 MiB (484 MB). Keel's `-lcs-max-cells` (134 million cells, 512
   MiB) is the same bound under its own name; a lower default is the
   candidate.
7. **A failed `AUTH` inside `EXEC` leaves a hole in the reply array.**
   Observed on the wire by `scripts/error-parity.py` (`docs/error-replies.md`,
   "Differences that stay"); Keel sends the `WRONGPASS` element. The
   source path is not identified here; it is the first question below.

## Questions for runtime evidence (REA)

The source leaves these open, or makes them depend on the build, the kernel
or timing; a process or network capture of a real `redis-server` settles
them. Listed so that REA can be pointed at them once the owner has it set up.

1. Why a failed `AUTH` queued in `MULTI` produces no element in `EXEC`'s
   array (candidate improvement 7): which reply path drops it, and whether
   `HELLO ... AUTH` inside `MULTI` does the same.
2. The exact bytes of `HELLO` in both protocols on the 8.10.1 build CI uses:
   the `modules` list (vector sets), and whether `availability_zone` and
   any 8.x field appear with the default configuration.
3. The order of a command's reply and the push messages it caused on the
   same RESP3 connection, in a pipeline, when the command itself subscribes
   (`afterCommand` appends pending pushes after the reply; `SUBSCRIBE`
   replies are pushes themselves).
4. Timing of `everysec` under a slow disk: when the postponed write is
   released, how `aof_delayed_fsync` and `aof_last_write_status` move, and
   what a client sees during the two-second window (candidate 2).
5. The on-wire segmentation of a large pipelined reply: whether the 64 KiB
   per-event write limit (`writeToClient`) is visible as a pause, and how
   it interacts with I/O threads.
6. The effective listen backlog: `tcp-backlog` 511 capped by
   `net.core.somaxconn`, and the warning logged when capped.
7. `OBJECT ENCODING` transitions at the listpack and intset thresholds for
   the sizes the test suite uses, to know which `assert_encoding` checks
   Keel can never satisfy versus which encode a visible behaviour.
8. Active expiry under load: `expired_stale_perc` and `expired_time_cap_reached_count`
   over a run that creates keys faster than the 25% CPU cap expires them,
   to size Keel's `avg_ttl` and stale-key reporting.
9. The unauthenticated limits in practice: the exact point at which a
   >16 KiB bulk or a >1 MiB query buffer closes the connection, and what,
   if anything, is written before the close.
10. `CLIENT LIST` and `CLIENT INFO` field order and values on 8.10.1
    (`tot-mem`, `io-thread`, `lib-name`), for Keel's `CLIENT INFO`.
11. `MONITOR`'s line format with the original argv after a command rewrote
    itself (`SET ... EX` logs `PXAT`; does `MONITOR` show `EX`?).
12. `SLOWLOG` truncation: `slowlog-entry-max-argc` (32) and the per-argument
    cut, as bytes.
13. The `PSYNC` handshake and the replication stream framing, for the later
    decision on replicating from or to Redis.
14. Expired-key deletion on a replica: that a replica answers nil for an
    expired key it still holds, and the `DEL` it then receives, with
    timing.

## How this document was made

`scripts/reply-census.py` locates every command's function in the source by
its definition line and lists the `addReply*` calls in it and in the helpers
it calls; the citations above for `processCommand`, `call`, the reply
builders, propagation, expiry, eviction, transactions, blocking and Pub/Sub
were read from the tarball by hand with the same line numbering. The Redis
source was read as data: nothing in it was built or run on this machine.
