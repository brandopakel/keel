# The public package

`github.com/brandopakel/keel` is Keel embedded: the engine the server runs,
opened in-process as a `*keel.DB`, with methods named after the Redis
commands they run. It arrives in `v0.2.0-alpha.1`
(docs/embedding-plan.md, phase 5). This page is what it promises and how it
is held to it.

## Opening an instance

```go
db, err := keel.Open(ctx, keel.Options{
    MaxMemory:   256 << 20,               // Redis's maxmemory; 0 is no bound
    Eviction:    keel.EvictLRU,           // allkeys-lru, the default
    Persistence: &keel.Persistence{Path: "cache.aof", Fsync: keel.FsyncEverySec},
})
if err != nil { ... }
defer db.Close()
```

- **In memory or logged.** With `Persistence`, every write is appended to a
  Redis-format append-only log that `Open` replays: a log a crash cut off
  mid-record is repaired to its last whole record. The log is the server's
  format, so a log written by a `DB` opens on `cmd/keel`, and the other way
  around.
- **One instance per log.** While open, a `DB` holds a lock on
  `Path + ".lock"`; a second `Open` of the same path, in this process or
  another, fails with `ErrLocked`.
- **Close** writes and syncs whatever is buffered, whatever the fsync policy,
  and releases the lock. It takes no context: what it waits for is the final
  sync, which a cancelled Close would leave undone.

## Calls

Every method takes a context and Go types and is safe for concurrent use.

- **Semantics are Redis's.** Each method runs the typed operation of the
  command it is named after, in that command's scope: the type check, the
  refusals, the keyspace hit and miss counting, and the log record are the
  command's. `Set` with `WithTTL` is `SET ... PX` and is logged as `SET` and
  `PEXPIREAT`; `Incr` refuses a value that is not an integer, and an
  overflow, in Redis's words.
- **Durability is a reply's.** A call that wrote something, or could have
  read something not yet logged, returns once it is as durable as the
  server's reply would be: written to the operating system under
  `FsyncEverySec` and `FsyncNo`, synced under `FsyncAlways`. A process that
  crashes loses no write a call has returned from; a machine that crashes
  loses what its policy allows, up to a second under `FsyncEverySec`.
- **Contexts.** A call whose context is done before it runs returns the
  context's error and changes nothing. One whose context is done while it
  waits for durability returns the context's error, but has run, as a Redis
  client that times out has had its command run.
- **No RESP.** Arguments and results are Go values; nothing is encoded or
  parsed, but in the experimental `Do`.

## The methods

| Area | Methods |
| --- | --- |
| Lifecycle | `Open`, `Close` |
| Strings | `Get`, `GetBytes`, `Set`, `SetBytes` (`WithTTL`, `WithExpireAt`, `WithKeepTTL`), `SetNX`, `SetXX`, `GetDel`, `MGet`, `MSet`, `Incr`, `Decr`, `IncrBy`, `DecrBy`, `IncrByFloat`, `Append`, `StrLen`, `GetRange`, `SetRange` |
| Keys | `Del`, `Exists`, `Type`, `Rename`, `RenameNX`, `DBSize`, `FlushDB`, `Scan` (`ScanOptions`) |
| Expiry | `Expire`, `ExpireAt` (`ExpireNX`, `ExpireXX`, `ExpireGT`, `ExpireLT`), `Persist`, `TTL` (`NoTTL`), `ExpireTime` |
| Transactions | `Atomic`, with `Tx`'s methods answering in `Result`s |
| Keel-native | `GetOrSet`, `CompareAndSwap`, `DeleteIf`, `Stats` |
| Experimental | `Do` |

Hashes, lists, sets, sorted sets, geo, the sketches and filters, and `Dump`
and `Restore` follow as their families' typed operations reach develop;
until then `Do` runs them.

- **`MGet` and `MSet` take and give maps.** `MGet` answers the keys that hold
  strings; a key that holds nothing, or another type, is left out, as `MGET`
  answers nil for both. `MSet` is one `MSET`, so that no call sees some of
  its keys stored and not the rest, logged with its keys in sorted order.
  With no keys, neither runs anything.
- **`[]byte` forms copy.** `SetBytes` copies its value before it returns,
  and `GetBytes` answers a copy the caller owns.

## Transactions

```go
var hits *keel.Result[int64]
err := db.Atomic(ctx, func(tx *keel.Tx) error {
    hits = tx.Incr("hits:" + user)
    tx.Expire("hits:"+user, time.Minute, keel.ExpireNX)
    return nil
})
n, err := hits.Value()
```

`Atomic` is Redis's `MULTI` and `EXEC`, on the same transaction block the
server's `EXEC` runs:

- **Queued, then run whole.** The function queues commands on the `Tx`, whose
  methods are the `DB`'s without a context, each answering with a `Result`.
  Once the function returns nil they run, in order, under one hold of the
  instance's lock, so that no other call runs between them, and what they
  write is logged as one `MULTI ... EXEC` block, which a replay applies whole
  or, cut off by a crash, not at all. The function runs before the lock is
  taken, and a `Result` is answered only once `Atomic` returns, so the
  function can take its time but cannot read what the transaction writes.
- **No rollback.** A command that fails fails alone, as in Redis: those after
  it run and those before it stand. `Atomic` returns nil once the
  transaction has run; each `Result` holds its command's answer and error.
- **Refused whole.** Nothing runs, and `Atomic` returns why, if the function
  returns an error, the context is done, the `DB` is closed, or its log has
  failed - while a failed write or sync is retried, a transaction that
  writes is refused with `ErrPersistence`, as Redis refuses `EXEC`. Each
  `Result` then holds that error.
- **Counted as a client's.** A transaction counts as the commands a client
  sends for it, its `MULTI` and `EXEC` with the rest.

## Scanning

```go
for key, err := range db.Scan(ctx, keel.ScanOptions{Match: "user:*", Type: "hash"}) {
    if err != nil { ... }
    ...
}
```

`Scan` walks the keys as a client walks them with `SCAN`, one step at a time,
each step a `SCAN` of its own. The lock is held for one step and never while
the loop's body runs, so the body may call the `DB`, change the keys it
walks, or take its time, and other calls run between steps. It guarantees
what `SCAN` guarantees: a key that holds something for the whole walk is
yielded at least once; a key may be yielded twice, and one added or deleted
during the walk may or may not be. `Match` and `Type` are `SCAN`'s `MATCH`
and `TYPE`, and `Count` its `COUNT`: the work each step does under the lock.

**Keel-native methods** are Keel's own surface, with Redis's semantics
underneath, and each says so in its doc comment:

- `GetOrSet` is the cache-aside read: `GET`, then the caller's load, then
  `SET NX`. Concurrent misses of a key share one load; a failed load stores
  nothing and answers every waiter with its error; a waiter whose context is
  done stops waiting without cancelling the load for the others.
- `CompareAndSwap` and `DeleteIf` are Redis 8's `SET ... IFEQ` and
  `DELEX ... IFEQ`: optimistic updates without `WATCH`.
- `Stats` is a typed snapshot of the counters `INFO` renders - keys, memory,
  hits and misses, expirations and evictions, the log's state - read from the
  same fields, so the two never disagree.

## Errors

An error keeps the text Redis answers with, and `errors.Is` matches it
against its category:

| Category | Matches |
| --- | --- |
| `ErrNotFound` | a key that holds nothing, where Redis answers nil |
| `ErrWrongType` | `WRONGTYPE ...`, and the refusals of commands that say it in words of their own |
| `ErrNotInteger` | `ERR value is not an integer or out of range` |
| `ErrOverflow` | `ERR increment or decrement would overflow`, `ERR decrement would overflow`, `ERR increment would produce NaN or Infinity` |
| `ErrOutOfMemory` | `OOM ...`, and a size past what one key may allocate |
| `ErrReadOnly` | `READONLY ...` |
| `ErrPersistence` | `MISCONF Errors writing to the AOF file: <cause>`: a failed log. Under `FsyncEverySec` and `FsyncNo` writes are refused while it is retried and reads are served; under `FsyncAlways` every call is refused until the DB is reopened |
| `ErrClosed` | `instance is closed` |
| `ErrLocked` | `log in use by another instance` |

Any other error - a malformed option, `ERR no such key` from `Rename` - is
the command's own refusal, in its words, with no category.

## Stability

- **Versioning.** Pre-1.0 semantic versioning. The API arrives in
  `v0.2.0-alpha.1`; a patch release never breaks it, a minor release may,
  with migration notes in its release notes. `Do` is experimental within that,
  and may change or go as the methods arrive.
- **The log is the stronger promise.** Every v0 release reads every earlier
  v0 log, and rolling back one minor release is checked by
  `scripts/check-upgrade.py`.
- **`apidiff` on every pull request.** `.github/workflows/apidiff.yml` runs
  `golang.org/x/exp/cmd/apidiff`, pinned, on every pull request that touches
  the package, through `scripts/api-compat.sh`: it compares the package's API
  with the base's and fails on an incompatible change - something removed or
  changed, or a type that stops being comparable. A pull request that means
  to break the API carries the label `api-break` and its migration notes; the
  job then reports the change rather than failing. `DB` is not comparable
  from the first release, so that the state it gains is never a break.
- **Dependencies.** The package adds none: `spaolacci/murmur3` remains the
  only runtime dependency.
