# Embedding plan: Keel as a Go library

Status: accepted plan, October 2, 2026. Phases 0 and 1 are done (#88, #90), and so are step 2.1,
the stores, step 2.2, the command scope, and step 2.3, persistence (see "Step
2.1: the stores", "Step 2.2: the command scope" and "Step 2.3: persistence"
below).

The owner asked for Keel to be usable as a Go library, not only as a server:
several independent instances per process, safe for concurrent use, a typed
API over every type Keel stores, a memory budget with eviction, optional AOF
persistence per instance, transactions, and an Open/Close lifecycle. The
server should become a thin RESP layer over the same library, so there is one
implementation.

Today none of that is possible. Every implementation package is under
`internal/`. Every store, the evictor, expiry, AOF, rewrite and replication
state and the configuration are package-level variables. The code assumes
exactly one command thread and takes no locks. This document is the plan for
changing that in small, shippable steps.

## Summary

- **Instance:** a `core.Engine` owns everything that is process state today.
  The public package `github.com/brandopakel/keel` wraps it as `*keel.DB`.
  `cmd/keel` and `internal/server` drive the same engine.
- **Concurrency:** one `sync.Mutex` per instance. "One thread touches the
  stores" becomes "the lock holder touches the stores", so the invariants the
  code relies on stay true. Disk I/O stays on the worker goroutines that
  already exist; they only ever own immutable bytes. The server takes the lock
  once per event-loop cycle and keeps driving maintenance itself.
- **API layering:** typed engine operations own the semantics, canonical AOF
  records and typed errors. The command language (string arguments to typed
  operation to reply sink) stays next to the engine, because AOF replay and
  replication apply use it. RESP2, and later RESP3, are reply encoders in a
  leaf package. The public Go API calls the typed operations and never goes
  through RESP.
- **Size:** about 35 to 40 PRs in seven phases, roughly 14 to 17
  engineer-weeks. A usable first version (lifecycle; strings, keys, TTL and
  hashes; a `Do` escape hatch for the rest) comes after phases 0 to 3 plus the
  first typed families, about 9 to 10 weeks.

## What is process-global today

| Where | State | Becomes |
| --- | --- | --- |
| `internal/core/storage.go` | Ten package-level stores (`dictStore` … `cfStore`), registered by `ResetStores()` | Fields of `core.Engine`, built against the engine's own `data_structure.Space` |
| `internal/data_structure/evictor.go` | Keyspace registry, eviction clock, pool and RNG, evicted count, `OnRemove`, `SuspendEviction` | `type Space struct`; `OwnerOf`, `EachKeyspace`, `TotalKeys`, `TotalMemUsed`, `DeleteAnywhere`, `EnforceLimits`, `Touch` become methods |
| `data_structure/keyed.go`, `dict.go` | Stores call back into those globals (`Put`, `Resize`, `SetExpiryAt`, `Get`) | Each store holds a `*Space` (8 bytes per store, not per key, so memory calibration does not move) |
| `data_structure/dict.go` | `SuspendExpiry`, read by `nowMs()` | Space field |
| `data_structure/lfu.go`, `evictor.go`, `lcs.go` | Read `config.EvictStrategy`, `KeyNumberLimit`, `MaxMemory`, `LRUSamples`, `LFULogFactor`, `LFUDecayPeriod`, `LCSMaxCells` | A limits struct on the Space, filled from engine options |
| `data_structure/keymap.go` | `keyLookupSeed`; scan and walk over the global registry | The seed stays global (immutable, random per process); scan and walk become Space methods |
| `core/aof.go`, `aof_async.go`, `aof_rewrite*.go`, `persistence_io_stats.go` | `aof`, append offsets and counters, `rewrite`, rewrite I/O and wake hooks, I/O stats | Engine fields; I/O hooks become per-engine so fault-injection tests stay per instance |
| `core/budget.go`, `command_allocations.go` | `affordable()` reads globals; `CommandAllocations` installed by the server | Engine methods; the transport budget is set by the server and absent for embedded callers |
| `core/commands_server.go` | `ClientBuffers` hook; INFO reads globals everywhere | `Engine.Stats()`; the server renders INFO. Field names stay byte-identical, because the soak, validation and recovery scripts parse them |
| `core/expire.go`, `failover.go`, `replication*.go` | Expiry cursors; term and fencing; replication v1/v2 and replica state | Engine fields; replication state is nil unless the server enables it |
| `core/eval.go`, `keytype.go`, `aof.go` tables | `commandTable`, `commandKeyspace`, `writeCommands`, key patterns | Stay global (immutable); unified into one operation-descriptor table in phase 4 |
| `core/comm.go` | `FDComm`, raw descriptor I/O used only by its own test | Moves to `internal/server` or goes, so the engine builds on every GOOS |
| `internal/config` | Every setting is a package variable | Engine settings become `core.Options`; listener and transport settings become `server.Options` |
| `internal/server` | `clients`, retained-byte counters, shutdown, waker, closed-client counters, `evalMu` | A `server.Server` struct; `evalMu` becomes the engine lock |

The test suite depends on this global state: it has 339 `ResetStores()` calls,
more than a hundred `config.X =` assignments, and no `t.Parallel()` anywhere.
Running the whole suite in parallel under `-race` at the end of phase 2 is the
strongest available proof that no shared mutable state is left.

## Concurrency

The `net` benchmark modes in `internal/server/server_net.go` already prototype
two of the candidates: a mutex around execution and a single executor
goroutine. In `bench/results/ci/matrix-linux.csv` they land within a few
percent of each other. The event loop's lead over both comes from its I/O
model (batched reads, coalesced writes), not from how execution is
serialised, and this plan leaves that model alone.

- **Mutex per instance (recommended).** Core code stays unsynchronised under a
  lock-holder contract. An uncontended lock costs about one store lookup. One
  instance's throughput stays at today's serial ceiling; more throughput comes
  from more instances.
- **Actor goroutine per instance (rejected for in-process calls).** Every call
  would pay a channel send, a wakeup and a reply wait. It offers natural group
  commit and FIFO fairness, but the mutex design gets group commit from the
  existing async-append offsets.
- **Sharded locks (rejected).** They break multi-key commands (MSET, MGET, DEL,
  PFCOUNT/PFMERGE, LCS, KEYS, SCAN, FLUSHDB), the shared eviction pool, single
  log ordering and atomic transactions.
- **RWMutex (not possible yet).** Reads mutate: `Get` touches access state and
  reaps expired keys, which appends a `DEL`.

For the server:
- **Locking:** the lock is taken after the multiplexer returns and released
  before the next wait, so it costs one uncontended lock pair per cycle.
  io-thread workers never touch engine state.
- **External driver:** the server keeps calling expiry, AOF flush, rewrite
  slices, memory maintenance, replication apply and the ordered-append gates at
  exactly the points it does now, so the rule "AOF flushed after every command
  and before any reply" is unchanged.

For embedded callers:
- **Calls:** each call checks its context, takes the lock, runs the same
  command scope `EvalAndResponse` runs today, and releases the lock.
- **Maintenance:** one maintenance goroutine per instance does expiry, flush,
  rewrite slices and memory maintenance on a ticker.
- **Durability:** writers append under the lock and wait outside it until the
  published offset covers their write. That offset means synced under `always`
  and written under `everysec`/`no`, the same gate the server applies to
  replies.
- **Persistence failures:** an AOF failure latches, and every later call gets
  `ErrPersistence`.

## API

```go
db, err := keel.Open(ctx, keel.Options{
    MaxMemory:   256 << 20,
    Persistence: &keel.Persistence{Path: "cache.aof", Fsync: keel.FsyncEverySec},
})
defer db.Close(ctx)

err = db.Set(ctx, "greeting", "hello", keel.WithTTL(time.Minute))
v, err := db.Get(ctx, "greeting")           // keel.ErrNotFound when missing
n, err := db.HSet(ctx, "user:1", "name", "Ada")
seen, err := db.BloomAdd(ctx, "visitors", "ada@example.com")
err = db.Atomic(ctx, func(tx *keel.Tx) error { ... })
for key, err := range db.Scan(ctx, keel.ScanOptions{Match: "user:*"}) { ... }
```

- **Methods:** named after the commands, taking a context and Go types.
- **Errors:** typed sentinels that match the existing wire errors, for example
  `ErrWrongType`, `ErrNotFound`, `ErrNotInteger`, `ErrOverflow`, `ErrOutOfMemory`,
  `ErrReadOnly`, `ErrPersistence`, `ErrClosed` and `ErrLocked`. Internal errors
  keep their exact wire text and implement `Is` against the public categories.
- **Iterators:** they never hold the lock while calling user code.
- **`Do`:** an experimental `Do(ctx, args...)` runs the command language, for
  commands without a typed method yet.
- **`Atomic`:** reuses the transaction block primitive from the MULTI/EXEC work.
  As with Redis EXEC, it isolates and logs as one frame and never rolls back.

Package layout:

```text
/ (package keel)          db.go options.go errors.go types.go tx.go doc.go example_test.go
internal/core/            Engine, typed operations, op table, command language, AOF, expiry, replication state
internal/data_structure/  Space and stores
internal/resp/            request parsing and RESP2/RESP3 reply encoders
internal/server/          event loop, io-threads, connections, AUTH/HELLO/CLIENT/SELECT, INFO rendering
cmd/keel/                 flags to core.Options and server.Options
examples/embedded/        cache, rate limit, analytics
```

Stability:
- **Versioning:** pre-1.0 semver. The API arrives in `v0.2.0-alpha.1`. Breaking
  changes come only in minor releases, with migration notes; experimental parts
  are marked; an `apidiff` job runs on PRs that touch the public package.
- **Log compatibility:** this is a stronger promise than the Go API. Every v0
  release reads every earlier v0 log, and rolling back one minor release
  remains checked by `scripts/check-upgrade.py`.
- **Dependencies:** `spaolacci/murmur3` is the only runtime dependency, and it
  is marked `// indirect` although `data_structure` imports it directly. Fixing
  that marking keeps the dependency list honest.

Persistence per instance:
- **File locking:** one instance per AOF path, enforced with an advisory lock on
  a sidecar `path.lock`. The lock cannot sit on the AOF descriptor, because a
  rewrite renames over the path and reopens it.
- **Open:** runs today's startup sequence: replay, torn-tail repair, open.
- **Rewrite and fsync:** both become per instance.
- **Replication:** stays server-only for now. Its state is coupled to the log,
  and its transport would need a public package of its own.

## Interaction with work in flight

- **MULTI/EXEC.** It changes the same AOF staging the engine move touches, so
  it lands first, or the AOF move (step 2.3) rebases onto it. Its EXEC block
  becomes the primitive behind `Atomic`.
- **RESP3.** It stays in the server: the protocol is per connection, chosen by
  HELLO and passed down explicitly, never through a config global. Its reply
  helpers seed the `Reply` sink of phase 4.
- **HELLO, CLIENT, SELECT.** Connection state stays in the server's `client`.
  SELECT accepts 0 only. If several databases are ever needed they belong
  inside one engine as namespaces, because Redis semantics share one memory
  budget and one log.

## Phases

Each step ships with every test green. Local checks follow AGENTS.md; matrix,
race, soak and benchmark evidence come from GitHub Actions.

| Phase | Steps | Effort | Risk |
| --- | --- | --- | --- |
| 0. Guardrails | Command-path benchmarks and a baseline/candidate bench job; move `FDComm`; `GOOS=windows` build of core and data_structure; a test that fails on any new package-level mutable variable, with an allowlist that shrinks every PR | 2 to 3 days | Low |
| 1. Space | `data_structure.Space` behind a default instance; then tests on their own Spaces with `t.Parallel()` | 1 week | Medium: the Get/Touch hot path and heap calibration |
| 2. Engine | 2.1 stores into `core.Engine`, by command family; 2.2 command-scope state; 2.3 persistence state (after MULTI/EXEC); 2.4 replication and failover state; 2.5 options replace `config`; 2.6 tests on per-test engines in parallel; 2.7 remove the default engine | 3 to 4 weeks | High for 2.3 and 2.4 |
| 3. Instance contract | `core.Open`/`Close`, sidecar lock, context-aware replay; the per-instance lock and the internal/external drivers; durability for direct callers | 2 weeks | High for durability |
| 4. Operations | Golden corpora of replies and AOF transcripts from develop; one op-descriptor table; the `Reply` sink and RESP2 encoder by family; typed operations by family; `internal/resp` | 4 to 5 weeks | Medium to high: reserve-before-allocate and zero-copy replies |
| 5. Public package | Lifecycle, strings, keys, TTL, `Do`; hashes, lists, sets; sorted sets and geo; sketches and filters; Scan and Dump/Restore; `Atomic`; docs, CHANGELOG, `apidiff`, tag `v0.2.0-alpha.1` | 2.5 to 3 weeks | Medium: the API commitment |
| 6. Server struct | Server state into `server.Server`; INFO composed from engine stats; `cmd/keel` reduced to flags | 1 week | Low to medium |

How each step is verified:
- **Every step:** the existing unit tests, the `cmd/keel` subprocess integration
  tests, the race job and the client and differential suites.
- **Hot-path steps:** paired A/B runs against develop (`bench/run-ab.sh`) and
  matched hosted runs, passing at a median paired ratio of at least 0.98 with
  no increase in allocations per operation. The hosted runs come from the
  paired command-path job (`.github/workflows/command-path.yml`), which fails
  on more allocations and reports time. Its noise floor, measured with
  develop against develop, is 0.3% in the median across benchmarks and 3% in a
  single benchmark. But code layout alone moved that median by up to 3.5% and
  a single benchmark by up to 9.5%. So the 0.98 rule reads the median across
  benchmarks, and a result near the budget, or a single family over it, is
  repeated before it counts. The job's header has the runs.
- **Phase 4:** byte-identical reply and transcript goldens.
- **Persistence steps:** restart, upgrade and rollback checks.
- **Phase 5:** two-instance isolation and contention stress tests under
  `-race`, and crash-and-reopen tests.

### Step 2.1: the stores

Step 2.1 moved the stores into `core.Engine` one command family per PR, in
this order: hashes; lists; sets; sorted sets with geo; the RedisBloom filters
(BF, CF); the counting sketches (CMS, Morris, HyperLogLog); strings, with the
type check; the commands that act on a key whatever its type (DEL, EXISTS,
TYPE, KEYS, SCAN, the TTL commands, DUMP and RESTORE, DBSIZE, FLUSHDB,
MEMORY); and last the expiry and memory-maintenance state and the handlers
that touch no store. Where the plan above left a choice open, step 2.1
settles it this way:

- **The default engine.** `defaultEngine` is a package-level `*Engine` that
  lives in `data_structure.DefaultSpace`, so its limits are still read from
  `config` until step 2.5. Its pointer never changes; `ResetStores` rebuilds
  the stores inside it. `EvalAndResponse`, `ExpireCycle`, `MaintainMemory`,
  `ExpiredKeys` and `KeysWithExpiry` keep their signatures and run on it, as
  data_structure's package functions run on `DefaultSpace`, so the server,
  log replay, replica apply and EXEC are unchanged. Step 2.7 removes it, as
  the plan says. `newEngine(space)` builds an engine of its own, for tests
  until `core.Open` exists (`TestEnginesShareNoKeys`).
- **Handlers.** Every handler is an Engine method in the one `commandTable`. It
  reads the stores, and asks the space who holds a key, on the engine that
  dispatches it. During the move a second table held the families that had
  moved, and dispatch tested one field of the entry it had already looked up to
  pick the call. The last PR made the remaining handlers methods too,
  including those that touch no store yet (PING, ECHO, SELECT, UNWATCH, INFO,
  BGREWRITEAOF and the KEEL.* replication and failover commands), and merged
  the tables again.
- **Code not yet on an engine.** Append admission, the rewrite, the log's size
  estimate, the sketch rewrite stream and the dump and replication encoders
  are not engine methods until steps 2.3 and 2.4. Until then they reach the
  stores through `defaultEngine`, and the space through the package functions
  over `DefaultSpace`. Each such reference is a place those steps pass the
  engine in instead. EXEC's suspension of eviction in `runTransaction` is
  command scope and moves in step 2.2.
- **Registration order.** The order the stores register with the space is the
  order `OwnerOf` asks them, eviction samples them, and SCAN and the rewrite
  walk them, so it does not change: strings, sorted sets, sets, hashes, lists,
  bloom, CMS, Morris, HyperLogLog, cuckoo.
- **No lock yet.** The mutex per instance and the internal and external drivers
  arrive with `core.Open` in phase 3. Until then the event loop is the only
  caller, as it is today, and an Engine is documented as not safe for
  concurrent use.
- **Expiry and memory maintenance.** The expiry and memory-compaction cursors
  and the expired-key count are positions in, and counts over, the stores'
  keys, and `ResetStores` already reset the count with them. They moved with
  the last PR of step 2.1, not with the command-scope state of 2.2. The
  cursors survive `ResetStores`, as they always have.
- **Census.** `TestPackageStateIsCensused` in `internal/core` lists every
  package variable with the reason it may stay: computed once and never
  written, or the step that moves it. Each PR of a step removes the entries it
  makes obsolete, and an entry for a variable that no longer exists fails.
  After step 2.1 it lists no store and no expiry state.
- **Measured per PR.** The paired command-path job runs at least twice against
  develop and once against `65ebdbc`, develop just before phase 1, so that
  small costs cannot accumulate unseen; each PR reports all three.

### Step 2.2: the command scope

Step 2.2 moves into `core.Engine` what is held for the command running, and
for the transport running it: the name GEOSEARCH was sent as, the reply
ceiling EXEC lowers, the transport's allocation budget, EXEC's suspension of
eviction, and the protocol each reply is framed in. It took two PRs: #110
moved everything but the protocol, and #111 moved the protocol, which every
reply helper reads. Where the plan above leaves a choice open, step 2.2
settles it this way:

- **On the engine, not passed down.** The state is held in fields of the
  engine running the command, beside its stores, rather than in a value handed
  to every handler. One command runs on an engine at a time, under the
  lock-holder contract the concurrency design gives each instance, so one of
  each is enough per engine, as one package variable was enough for the
  process. Every handler is already an Engine method, so reading a field costs
  what reading the variable did, and no handler signature changes before phase
  4 replaces them with the `Reply` sink, which will carry the protocol and the
  admission checks itself. The fields keep the names the variables had.
- **The reply builders are Engine methods.** The helpers that admit and build
  large replies (`admitReply`, `encodeBoundedString`, `encodeWalkReply`,
  `hashReply`, `scoredReply`, `encodeLookupArray`, `geoSearchReply` and their
  kind) read the ceiling and the budget of the engine whose command calls
  them. `scoredReply` still inlines, which keeps the closure it hands its walk
  on the stack.
- **The transport's budget.** The server installs its
  `CommandAllocationBudget` on the engine it drives with
  `core.SetCommandAllocations`, and reads it back with
  `core.CommandAllocations()`; both act on the default engine until step 2.7.
  An engine with no budget installed (an embedded caller's, and the server's
  own during startup replay) reserves nothing and keeps each command's own
  limits, as core-only callers always have. The transport still begins and
  ends each run on the budget, and INFO reports the budget of the engine it
  runs on.
- **EXEC.** `Transact` runs on the default engine, and a transaction runs on
  the engine its EXEC runs on: its replies are bounded by that engine's
  ceiling and reserve from that engine's budget, and eviction is suspended in
  that engine's space until the block closes. A replica applies a received
  block through the same `runTransaction`, on the default engine until step
  2.4 passes the engine in.
- **The protocol** stays the connection's. The server sets it on each
  `Command`, and `evalAndResponse` holds it on the engine for exactly that
  command and restores it afterwards, so nothing after the command inherits
  it, and log replay and replica apply, which answer nobody, run as RESP2
  whatever the command says.
  - The reply helpers in `resp3.go` are methods of `framing`, a one-field
    value the engine embeds, so a handler writes `e.nullReply()` where it
    wrote `nullReply()`. `appendDouble` takes the framing as an argument,
    because a method cannot have a type parameter.
  - Every reply a handler encodes goes through `e.encode`. A reply built
    outside any command names its protocol and reads no engine: `Encode` is
    RESP2, as it always was there, and the connection layer's `EncodeAs`
    takes the connection's. The `Encode` calls left are errors and strings,
    which are the same bytes in both protocols.
  - A closure handed to an unknown walk escapes, so the RESP3 reply builders
    give their walks the framing rather than the engine.
- **Isolation.** `TestEnginesShareNoCommandScope` gives two engines different
  protocols, budgets, ceilings and names, and runs them one after the other,
  one inside the other's EXEC, and side by side on two goroutines, which the
  race job runs under `-race`.
- **Census.** Each PR removes the entries it moves. After step 2.2 the census
  lists no command-scope state: what remains is the default engine, the
  persistence state of step 2.3, the replication state of step 2.4, the
  server's INFO hook, and values computed once.
- **Measured per PR**, as in step 2.1: the paired command-path job runs at
  least twice against develop and once against `65ebdbc`.

### Step 2.3: persistence

Step 2.3 moves into `core.Engine` the state that makes an engine's writes
durable: the log file and its buffer, the append worker and its pending
result, the sync state, the rewrite and its record stream, the rewrite's
outcome and retry backoff, the I/O hooks tests inject, and the I/O counters
INFO reports. Nothing it writes may change: not a byte of a log or a rewrite,
not a sync, and not the order or the back-pressure of appends. It takes three
PRs, in this order:

1. **The log and its append worker** (`aof`, the `append*` offsets and the
   worker's result, and the `aofWrite` and `aofSync` hooks).
2. **The rewrite and what outlives one**: its walk and record stream, its
   I/O job and wake, the handoff hooks (`rewriteFileWrite`, `rewriteFileSync`,
   `rewriteOpenLog`, `rewriteRename`, `rewriteSyncDir`) and
   `keyCountForRewrite`; and its outcome, the retry backoff,
   `rewriteBudgetAborts`, `nextAutoRewrite`, `snapshotRetryAt` and
   `unsyncedLogDir`.
3. **The six I/O counters** INFO reports for the log and the rewrite.

The order follows what reads what. Every write touches the log - a handler
stages its record, `evalAndResponse` begins and commits it, and replay's
leniency is a question about the log - so the log moves first, and with it
the guardrails below. The rewrite reads the log's file, path, buffer and sync
state at every step, so it follows the log and reads it through
`defaultEngine` for one PR; the other way round, every flush would reach the
rewrite through `defaultEngine`. What outlives one rewrite was planned for the
last PR, and moves with the rewrite instead: once each engine rewrites its own
log, a rewrite scheduled inside one engine's EXEC, the wait one engine's
failures earn, and the directory sync one engine's rename leaves pending have
to be that engine's, or another engine's flush starts, delays, retries or
clears them. Review of the second PR found exactly that. The counters time
the log's I/O and the rewrite's, and only INFO reads them, so they go last,
once both their writers are methods. Where the plan above leaves a choice
open, step 2.3 settles it this way:

- **Guardrails first, captured on develop.** The first commit of the first PR
  adds them, on develop's code at 40eb2f6, before anything moves:
  - `persistence_golden_test.go` runs six scenarios under every fsync policy,
    with synchronous and worker appends: every command family with its
    writes, reads and refusals; the records logged in a command's place; each
    kind of expiry, lazy, active and in the past; SET over another type
    (#85); transactions, framed, empty, aborted and with a key reaped inside
    (#89); evictions after EXEC; rewrites with a key written before the walk
    and during the snapshot's preflush; a rewrite scheduled by EXEC; and the
    BF and CF forms written before RedisBloom parity (#94). It compares the
    log, the replies and the keyspace with `testdata/persistence-40eb2f6`, and
    replays develop's logs, and their rewrite, on the build under test. A log
    is compared record by record after two normalizations and no others: a
    relative expiry becomes the whole hours from the run's start, and map
    order inside an HSET record, and inside a ZADD record without options, is
    sorted.
  - `scripts/check-log-compatibility.py`, run by the Log compatibility
    workflow on Linux and macOS, builds the base and the change and runs one
    workload on each under every policy and append mode (synchronous, worker
    and concurrent). The logs each writes and rewrites must be the same bytes,
    after the same normalizations, and each build must replay the other's
    written, rewritten and crashed logs (kill -9 after writes during a
    rewrite) to the other's keyspace: upgrade and rollback. A build that logs
    UNLINK as itself fails both guards, which is how they were checked.
  - `BenchmarkCommandPathWithLog` is SET, INCR, HSET, LPUSH-RPOP and SADD with
    the log open under everysec, flushed once every 64 commands as a
    pipelined cycle is. Its file uses only what the package had before, and
    `command-path.yml` builds the candidate's copy of it into a baseline that
    predates it, so it pairs against develop and against `65ebdbc` alike.
- **Fields keep their names**, as in step 2.2: `e.aof`, `e.appendPending`,
  `e.appendStarted` and so on. The hooks are fields too, `e.aofWrite` and
  `e.aofSync`, set to `writeLog` and `syncLog` when an engine is made. A test
  fails the disk of the engine it names, the default engine's for the
  server, and no other engine's.
- **Methods, and the package's functions on the default engine.** Each
  exported function keeps its signature and runs on the default engine, as
  `ExpireCycle` does, and is also an Engine method of the same name (`OpenAOF`,
  `LoadAOF`, `FlushAOF`, `FlushAOFAsync`, `CloseAOF`, the append offsets,
  `AppendAdmission`, `AOFStats`). The rest become Engine methods. The engine's
  own space carries the removal hook that logs expiry and eviction, and is
  where replay holds off eviction and expiry; `AppendAdmission` and its reply
  bounds read the engine's stores. Admission still reads its limits from
  config, as the default engine's space does, until step 2.5.
- **No indirect call per command.** `evalAndResponse` deferred `aofEnd()`,
  which the compiler calls directly at each return. Deferred as the method
  `e.aofEnd()`, it was wrapped in a closure and called through it on every
  command: an indirect call and an extra frame. It is now called at each
  return instead, after the reply is written, as the deferred call ran; no
  panic is recovered on the way out of a command, so the two do not differ.
  Over the whole step against 40eb2f6, the command path went from 1.011 and
  1.015 to between 1.000 and 1.004 on EPYC 7763.
- **Replay's leniency is the replaying engine's.** A log may hold integers
  written before canonical spelling was enforced, so `counterInteger` accepts
  them while its engine replays. It and the readers built on it
  (`positiveCount`, `integerRange`, `randomCount`, the ZRANGE, score-range and
  pop-count parsers, and `argumentsBeforeType`, whose functions now take the
  engine) are methods, as are `affordable` and `replayingFilterLog`.
- **Replication where the log calls it.** The replication functions the log
  calls, which read the log's state, become methods that read their engine's
  log: `noteReplicationDirty`, `replicaCommandError`, `replicationV2Enabled`,
  the protocol 2 publication (`recordReplicationV2Body`,
  `recordReplicationV2Commit`), `captureReplicationSnapshot`, the log's digest
  and the replica checkpoint. Their replication state stays where it is until
  step 2.4, with one exception: `replicationTransaction`, the protocol 2 frame
  of the transaction running now, moves with the log, because EXEC opens and
  closes it with the log's frame of the same block, and two engines' EXECs
  would otherwise write one variable. Replication code the log does not call
  (`InitReplication`, replica apply) reaches the log through `defaultEngine`.
- **The rewrite** is an Engine method from `StartRewrite` to the handoff, and
  its state a field of a named type, `rewriteState`. Its I/O and the steps of
  its handoff are engine fields too, which `engineIn`, the constructor
  `defaultEngine` and `newEngine` now share, sets to the real thing:
  `rewriteFileWrite` and `rewriteFileSync` (`writeLog`, `syncLog`),
  `rewriteOpenLog` (`openRewrittenLog`), `rewriteRename` (`os.Rename`),
  `rewriteSyncDir` (`syncDir`), and `keyCountForRewrite`, the key count of the
  engine's own space. The loop driving an engine installs its waker on that
  engine with `SetRewriteWaker`. The walk, the key images and the dump
  encoder (`emitKey`, `dumpKey`, the sketch stream) read the engine's stores
  and space. KEEL.REPL.PULL takes its snapshot's images from the engine it
  runs on; protocol 1's `sealReplication` takes them from the default engine
  until step 2.4.
- **What outlives one rewrite** is the engine's too: `rewriteOutcome`, now of
  a named type, `rewriteOutcomeState`, which `engineIn` starts with no rewrite
  ended (-1 seconds, as Redis reports it), and which holds the retry limit
  and a BGREWRITEAOF scheduled inside EXEC; `nextAutoRewrite`;
  `rewriteBudgetAborts`; `snapshotRetryAt`; and `unsyncedLogDir`, which only
  a flush of that engine's log retries before it syncs, and only that
  engine's close or reopening clears. So one engine's failures, and the waits
  they earn, hold back no other engine's rewrites, a scheduled rewrite starts
  on the engine whose EXEC scheduled it, and each engine's INFO reports its
  own.
- **The I/O counters** are six fields of each engine, and INFO persistence
  reports the engine's own. A worker is handed the counter it times before it
  starts, as it is handed its file and its hook, so it touches nothing of the
  engine.
- **Until a part moves**, the code that owns it reaches what has moved through
  `defaultEngine`, as step 2.1's leftovers reached the stores: after the first
  PR, the rewrite read the server's log that way. The rewrite was the
  default engine's until it moved, so until then only the default engine's
  writes, removals, flushes and close reached it (`ownsRewrite`); another
  engine's would have marked keys it does not hold, or advanced or cancelled
  a rewrite of a log it does not write. The second PR removed the guard with
  the package state.
- **Isolation.** `TestEnginesShareNoLog` gives two engines a log each, with
  I/O of their own: one engine's records, transaction frames, reaped keys and
  failed disk stay its own, and then both run side by side through
  `evalAndResponse`, one appending on the worker and one synchronously, with
  transactions and expiry cycles, and each log replays to its own engine's
  keyspace. `TestEnginesShareNoCommandScope` now runs its side-by-side part
  through `evalAndResponse` too, with a log open on each engine.
  `TestEnginesShareNoRewrite` rewrites one engine while the other writes: the
  rewrite marks only its own engine's keys dirty, replaces only its own log,
  wakes only its own loop, and fails, or refuses at the key ceiling, through
  its own engine's hooks alone. A BGREWRITEAOF scheduled inside one engine's
  EXEC is started by that engine's flush and not the other's, and a directory
  sync one engine's rename leaves pending is retried by that engine's sync and
  left alone by the other's syncs, close and reopening. Then both rewrite side
  by side, written to while they do, one failing three times running on its
  own disk and the other succeeding: each engine's INFO reports its own
  outcome, only the failing one is held back by the retry limit, and each log
  replays to its own engine; each engine's write errors, on its log and on
  its rewrite, are counted on that engine. The race job runs them under
  `-race`.
- **Census.** Each PR removes the entries it moves. The first removes the ten
  of the log and its worker, and `replicationTransaction`; the second the
  fourteen of the rewrite and what outlives it; the third the six counters,
  and with them the census's `persistence` reason. What remains is the
  default engine, sixteen replication and failover entries for step 2.4, the
  server's INFO hook, and values computed once.
- **Measured per PR**, as in steps 2.1 and 2.2, now including the log-on
  benchmark: the paired command-path job runs at least twice against develop
  and once against `65ebdbc`.

## Risks, in order

1. Moving AOF, rewrite and replication state while MULTI/EXEC changes the same
   functions.
2. Group commit and read visibility for direct callers: new concurrency over
   intricate offsets.
3. The reply sink keeping reserve-before-allocate and zero-copy replies.
4. Canonical records produced by typed operations: a replay divergence there is
   silent data loss, which is why the transcript goldens come first.
5. Hot-path cost of the extra indirection and the lock.
6. Freezing the API too early, which is why it ships as an alpha with
   experimental markings.

## Decisions

The owner settled the open questions on October 2, 2026, with a standing rule
for anything left uncertain: do what Redis does.

- **Eviction default:** LRU, matching the `cmd/keel` flag. `config.EvictStrategy`
  defaulting to random is the server-side inconsistency to remove in step 2.5.
- **Key limit default:** none, as in Redis, where only `maxmemory` bounds the
  keyspace. `MaxKeys` defaults to zero, meaning unlimited. The server's current
  5,000,000 cap becomes an explicit `-maxkeys`-style setting rather than a
  hidden default. The rewrite's four-million-key ceiling and the snapshot's
  one-million-key refusal stay documented limits of persistence, not of the
  keyspace.
- **`Atomic` semantics:** as Redis EXEC. Queued work is isolated and logged as
  one frame; a command that fails inside it does not undo the others, and there
  is no rollback.
- **Release target:** the first embeddable release is `v0.2.0-alpha.1`.
