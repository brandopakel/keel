# Embedding plan: Keel as a Go library

Status: accepted plan, October 2, 2026. Phases 0 and 1 are done (#88, #90), and so are step 2.1,
the stores, step 2.2, the command scope, step 2.3, persistence (see "Step
2.1: the stores", "Step 2.2: the command scope" and "Step 2.3: persistence"
below), and step 2.4, replication and failover (see "Step 2.4:
replication"), step 2.5, options in place of `internal/config` (see "Step
2.5: options"), and step 2.6, the suite run in parallel on engines of its own
(see "Step 2.6: parallel tests"), and step 2.7, which removed the default
engine (see "Step 2.7: the default engine"). Phase 2 is complete; phase 3 is
planned in "Phase 3: the instance contract".

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
    is compared record by record after three normalizations and no others: a
    relative expiry becomes the whole hours from the run's start; the records
    a rewrite cuts one large collection into, at 256 elements, 64 KiB or a
    millisecond, are joined, because where the millisecond falls depends on
    the machine; and map order inside an HSET record, and inside a ZADD record
    without options, is sorted.
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

### Step 2.4: replication

Step 2.4 moves into `core.Engine` the state replication and failover keep
between commands: what a replica has applied of its primary's stream, the
stream a primary feeds its replicas, and the term that says whether a node may
write. Nothing a peer can see may change: not a frame's bytes, a snapshot's, a
checkpoint's or a term file's, not a reply, and not the log a replica writes.
It takes three PRs, in this order:

1. **The replica** (`replicaApplying`, `replicaReady`, `replicaEpoch`,
   `replicaOffset`, `replicaUpdated` and `replicaV2`), with the checkpoint's
   I/O (`checkpointRename`, `checkpointSync`, `checkpointSyncDir`).
2. **The primary's stream**: protocol 1's dirty keys and history
   (`replication`), protocol 2's history and snapshot (`replicationV2`) and
   what its replicas have acknowledged (`replicaAck`).
3. **Failover**: the term (`failover`) and its file's I/O (`termRename`,
   `termSync`, `termSyncDir`).

The order follows what reads what. Every command reads the replica's state:
`evalAndResponse` asks whether its engine is applying a primary's decision
before framing a reply, and the replica's refusal asks whether it has recent
state before running a read. And applying a frame is where the step 2.2
leftover was, a received transaction run through `defaultEngine`. So the
replica moves first. The checkpoint's I/O moves with it, although the census
listed it beside the term's: only the replica's apply writes a checkpoint, and
what it writes is the replica's position. The primary's side reads the replica
only for whether its engine is applying, which it already asks of the engine
it publishes from. Failover goes last because both sides read it, a replica
observing each frame's term and a primary stamping its frames with its own and
observing its replicas', so once both are methods it moves in one PR, with
neither side reaching it through `defaultEngine`. Where the plan above leaves a
choice open, step 2.4 settles it this way:

- **Guardrails first, captured on develop.** The first commit of the first PR
  adds them, on develop's code at 9736d8d, before anything moves:
  - `replication_golden_test.go` runs three scenarios under every fsync
    policy, with synchronous and worker appends, and compares a transcript of
    each with `testdata/replication-9736d8d`. A protocol 2 primary's stream
    from its start, a snapshot and the deltas after it (every operation the
    stream carries, the records written in a command's place, opaque images,
    a key reaped lazily, transactions of writes, reads, a failed command and
    expiries), and the refusals of its pulls; then the same engine as a
    replica applying the snapshot and the deltas, writing its checkpoint,
    restarting from it and catching up, and refusing a gap, a bad checksum
    and a protocol 1 frame. A protocol 1 primary's full frame and deltas, and
    a replica applying them. And promotion, fencing and observed terms, the
    term file each writes, a restart onto it, the terms frames carry, and a
    replica learning a term from a frame and refusing an older one. The
    transcript holds every frame as sent, with its body record by record;
    every reply to a replication, failover, refused or KEEL.DUMP command;
    INFO replication field by field; what each applied frame added to the
    replica's log; the checkpoint; the term file; and the keyspace, which
    the replica must share with its primary. Random epochs and snapshot
    identities are named by the order they first appear in, a checksum is
    checked and written as "ok", ages are left out, a checkpoint's digest is
    checked against the replica's log, and records are normalized as the
    persistence golden test normalizes a log. Protocol 1 seals keys from a
    map, so each key's records in one of its frames are kept together and the
    keys put in order; so is the one protocol 2 frame that carries PFMERGE's
    images of several keys, which also come from a map.
  - `scripts/check-replication-compatibility.py`, run by the Replication
    compatibility workflow on Linux and macOS, builds the base and the change
    and, over protocols 1 and 2: pulls from each build by hand and requires
    the same frames, snapshot, KEEL.DUMP images and INFO replication fields
    for the same writes; runs every pairing of a base or changed primary with
    a base or changed replica through a full sync, a delta stream with
    MULTI/EXEC blocks, acknowledgements that reach the stream's end, a
    dropped connection resumed from the replica's cursor, a replica crash
    restarted on the other build (under protocol 2, from the checkpoint the
    first build wrote) and a primary restart, which starts a new epoch, with
    the replica's keyspace its primary's after each and the same in every
    pairing; requires every protocol 2 replica to write the same log,
    normalized as its frames are, and each build to replay every replica's
    log; and moves the term file each
    build writes to the other, requiring the same replies, INFO fields and
    file bytes. It reuses `check-log-compatibility.py`'s workload and
    normalization.
  - `BenchmarkCommandPathWithReplica` is `BenchmarkCommandPathWithLog`'s
    families and cycle with a protocol 2 feed on and a replica attached
    in-process; its file uses only what 65ebdbc had, and `command-path.yml`
    borrows it into an older baseline as it borrows the log-on benchmark. It
    needs no network: a pull is a command like any other. The timed loop has
    no pull in it, because a primary publishes every write the same way
    whether a replica pulls or not, and a pull's cost, a checksummed JSON
    frame of up to 256 KiB, is per pull and would drown the per-write cost
    the step touches. After the timer, the stream must have grown by every
    write measured, in the epoch the replica attached in.
  - **Negative controls**: three builds broken on purpose. One publishes INCR
    in lower case, which changes no keyspace and no replica log: the golden
    test fails (`want "INCR" "c"`, `got "incr" "c"`), and so does the
    script's wire check (`body 0 (protocol2) differs: record 12`), while its
    pairings pass, which is why the wire check is there. One writes a
    version 3 checkpoint: the golden test fails on the checkpoint, and the
    script on `the candidate did not resume from the baseline's checkpoint`.
    One ends the term file with a newline, which still parses: the golden
    test fails on `term file promoted: "3"`, and the script on the replica's
    term file.
- **Fields keep their names**, as in steps 2.2 and 2.3: `e.replicaApplying`,
  `e.replicaReady` and so on, and `replicaV2` is a field of a named type,
  `replicaV2State`. The checkpoint's I/O hooks are fields too,
  `e.checkpointSync`, `e.checkpointRename` and `e.checkpointSyncDir`, which
  `engineIn` sets to `syncFile`, `os.Rename` and `syncDir`.
- **Methods, and the package's functions on the default engine.**
  `ApplyReplication` and `ReplicaResumeCursor` keep their signatures and run
  on the default engine, and are Engine methods of the same name, so the
  server is unchanged. The rest become methods. `InitReplication` stays a
  package function until its other half, the primary's stream, moves in the
  second PR: it starts the default engine's stream and calls the two halves
  of the replica's start on the default engine, `resetReplica` and
  `followPrimary`, in the order it always ran them.
- **A replica applies on its own engine.** A frame's commands run through
  the applying engine's `evalAndResponse`, and a received transaction
  through its `runTransaction`, so they are logged in that engine's log and
  the block is framed there; step 2.2 left this on the default engine.
- **The role is the engine's, from the second PR.** Whether an engine is a
  replica, feeds a stream, and in which protocol were flags read from
  `config` wherever they were needed, so every engine in a process was a
  replica or none was, and the first PR's isolation test made them all
  replicas. The second PR's needs a primary and a replica in one process, so
  each engine reads its role, a `replicationRole` (`ReplicaOf`, `Feed`,
  `Protocol`), through three pointers, as a space reads its limits: the
  default engine's point at the config variables, read live, so the flags and
  the tests that assign them work unchanged until step 2.5 replaces both
  with options; any other engine's point at its own `ownRole`, which starts
  as neither a replica nor a feed, in protocol 1, as the flags do. Everything
  that asked config is a replica, feeds, or speaks protocol 2 now asks the
  engine: the replica's refusal, the stream's publication, the pulls, the
  apply path, the checkpoint (whose `primary` field is the engine's
  `ReplicaOf`), the log's replay and digest, the expiry cycle and INFO.
- **The primary's stream** moves in the second PR as three fields of named
  types: `replication` (`replicationState`: protocol 1's batches and what
  both protocols share, the epoch and the keys the running command changed),
  `replicationV2` (`replicationV2State`: the history and the snapshot) and
  `replicaAck` (`replicaAckState`). `InitReplication` and
  `ReplicationAcknowledged` keep their signatures on the default engine and
  are methods of the same name; the rest become methods. So the step 2.3
  leftovers go: `sealReplication` seals protocol 1's images from the sealing
  engine's stores, and refuses on its space's size; a protocol 2 primary's
  opaque images come from its own stores; `InitReplication` starts the
  engine it is called on; and `CloseAOF` closes the snapshot of the engine
  whose log it closes, not the default engine's.
- **No deferred closure on a write.** With a protocol 2 feed on, every write
  ended in `recordReplicationV2Commit`, which cleared the command's dirty
  keys in a deferred closure. That is the pattern step 2.3 found costing an
  indirect call per command (#112, #113), and moved onto the engine the
  closure would capture it. The function now publishes and then clears, as
  the deferred call did, since no panic is recovered on the way out of a
  command.
- **The inliner's budget.** `noteReplicationDirty` runs for every key every
  write changes, and was inlined at each call, at a cost of 74 against the
  budget of 80. Reading the role and the stream through the engine cost 91,
  and it stopped being inlined, which with the feed off is a call per key
  where there was none. It now reads the feed flag directly and the stream
  through one pointer, and the dirty set is never nil (`engineIn` makes it),
  which brings it back to 74. `replicationV2Enabled` and `writable` inline as
  before.
- **Failover read the default engine's role until the third PR.** While the
  term was still the server's, `LoadTerm` and `observeTerm` read `config`,
  the default engine's role, and `writable`, which a primary's pulls and
  every write ask, read the engine's own role and the server's term.
- **The term** moves in the third PR: `failover` (`failoverState`, the term,
  the term held, whether the engine is fenced, and its file) and the file's
  I/O hooks, `e.termSync`, `e.termRename` and `e.termSyncDir`, which
  `engineIn` sets to `syncFile`, `os.Rename` and `syncDir`. `LoadTerm`,
  `CurrentTerm`, `HeldTerm`, `Fenced` and `Writable` keep their signatures on
  the default engine; `LoadTerm` and `CurrentTerm` are methods of the same
  name, `writable` is `Writable`'s, and `observeTerm` and `persistTerm`
  become methods. An engine's term file is beside its own log; a term it
  learns from a peer, its replicas' pulls or its primary's frames fences it
  alone, as its own role says; and its frames carry its own term.
- **The term stays atomic.** The replica transport reads the term from a
  goroutine of its own, to send it with each pull, so `e.CurrentTerm` loads
  it atomically and every store to it is atomic, as the package's were; the
  server's transport reads the default engine's.
- **Isolation.** `TestEnginesShareNoReplica` gives two replicas a log each
  and the streams of two primaries in epochs of their own, each with a
  transaction larger than one frame. Applied interleaved, one engine stops
  inside its transaction while the other catches up, and each holds its own
  open block, readiness, position and checkpoint. A third replica of the
  first primary whose disk will not sync its checkpoint stops, through its
  own I/O alone. Then two fresh replicas apply side by side on goroutines of
  their own, reading as they go; each ends with its own primary's keyspace,
  writes its own checkpoint, and restarted on its own log resumes from it.
  Protocol 1 does the same side by side. The default engine, a replica as
  well, applies none of it. The race job runs it under `-race`.
- **Isolation, the primary.** `TestEnginesShareNoReplication` runs three
  primaries, two over protocol 2 and one over protocol 1, each with a
  replica of its own that pulls from it in-process, as the transport does,
  and applies what it is sent: a snapshot taken by its primary's rewrite,
  then rounds of writes with transactions, a key reaped lazily, and the same
  names held as different types on each primary, one a filter with an
  expiry whose image is published. Side by side on goroutines of their own,
  each replica ends with its own primary's keyspace, at its primary's
  offset, in its primary's epoch, which no other engine has; each primary
  hears its own replica's acknowledgement and served a snapshot of its own
  log. Then one primary's epoch starts again and its replica takes a new
  snapshot while another primary's stream, epoch and replica carry on, and
  closing one primary's log closes its snapshot and leaves the other's open.
  The default engine feeds none of it and applies none of it. Reverting the
  opaque images to the default engine's stores fails it.
- **Isolation, failover.** `TestEnginesShareNoFailover` gives two primaries a
  log and a term file each. One is promoted and the other fenced at a higher
  term, and each writes, refuses, reports and keeps only its own term; the
  first's frames carry its term to a replica of its own, which keeps it
  beside its own log, while a replica that has moved on deposes the second
  alone. The first's disk then fails its term file's sync: its promotion
  fails and fences it, through its own I/O, while the second's promotion goes
  through the second's. Side by side, each takes fifty promotions, writes and
  fences on a goroutine of its own while another goroutine reads its term as
  the transport does; each ends with its own term in its own file, and a
  restart on each log reads it back, fenced. The default engine's term and
  file are untouched.
- **Census.** The first PR removes the nine entries it moves, the second the
  three of the primary's stream, and the third the four of failover, and
  with them the census's `replicated` reason. What remains is the default
  engine (step 2.7), the server's INFO hook (phase 6), and the tables and
  sentinels, computed once.
- **Measured per PR**, as in steps 2.1 to 2.3, now including the replica-on
  benchmark: the paired command-path job runs at least twice against develop
  and once against `65ebdbc`. The second PR's runs against develop came out at
  1.009 to 1.013, inside the budget, with two rows over 1.04 on repeat on EPYC
  7763: the log-on LPUSH-RPOP and SISMEMBER, a read whose path the PR changed
  by one load. Its hot functions were the same instructions as develop's but
  for that load and one per key written, while the functions whose sizes it
  changed had flipped the 64-byte alignment of the hot functions after them.
  A layout control, the PR plus a never-taken branch in the cold `OpenAOF`
  that restored develop's alignment for nine of twelve hot functions, ran at
  0.993 and 0.996 with no row over 1.04, so the excess was placement and not
  cost, and the control was not merged.

### Step 2.5: options

Step 2.5 replaces the package variables of `internal/config`, which every
engine in a process read, with options: `core.Options` for what an engine is
held to, and `server.Options` for the listener and the transport. `cmd/keel`
maps its flags onto both. Nothing the server does may change: every flag
keeps its name, its default and its meaning, and the server's effective
settings, the 5,000,000-key cap and LRU eviction among them, are the ones it
had. It takes three PRs, in this order:

1. **The keyspace's limits**: `MaxMemory`, `KeyNumberLimit`, `EvictStrategy`,
   `LRUSamples`, `LFULogFactor`, `LFUDecayPeriod` and `LCSMaxCells` become
   `core.Options`, which an engine resolves into a `data_structure.Limits`
   that its space holds by value.
2. **Expiry, persistence and replication**: the active-expiry parameters,
   the log's settings (`AOFEnabled`, `AOFFileName`, `AOFFsync`,
   `AOFAsyncAppend` and the automatic rewrite's) and the role
   (`ReplicaOf`, `ReplicationFeed`, `ReplicationProtocol`) become
   `core.Options` too.
3. **The listener and the transport**: `Host`, `Port`, `MaxConnection`,
   `IOThreads`, `CronIntervalMs`, `RequirePass`, the replica's password and
   TLS, and `AOFConcurrentAppend` become `server.Options`, and what is left of
   `internal/config` is its build identity.

The limits go first because every access reads one: they are the step's hot
path, and the indirection that let the default space read config live goes
with them. Each PR moves its settings end to end - the code that reads them,
`cmd/keel`'s mapping and every test that sets them - and removes their
variables from `config`, rather than moving the code first and the tests in a
last PR. While a test assigns a variable, the engine has to read it live,
which is what the pointers of phase 1 and step 2.4 were for; moving the tests last
would have kept those pointers, and two places to set one setting, for two
more PRs. So at every commit each setting is in exactly one place. Where the
plan above leaves a choice open, step 2.5 settles it this way:

- **Zero is the default.** Every field of `core.Options` takes its default
  when it is zero, so `Options{}` is an engine for embedded use: no bound on
  its keys or its memory and LRU eviction, as the Decisions below say, and
  Redis's figures for the rest. A setting that can be turned off where off is
  not its default takes a negative value for off, `core.Off`, as go-redis
  does for its timeouts and retries: `LFUDecayPeriod` then never decays,
  `LCSMaxCells` has no bound, and `LFULogFactor` is a factor of zero, which
  counts every access. `Options` validate on the way in, and a refused one
  leaves the engine as it was.
- **As given, and resolved.** An engine keeps its options as they were given,
  which `Configuration()` reads back, so a test can put back exactly what it
  found; and it resolves them into what the code reads, every default filled
  in and every setting that is off spelled as the space spells it, zero. The
  space's `Limits` are taken as written, with no defaults of their own beyond
  `DefaultLimits()`, which is what `Options{}` resolves to.
- **No key bound unless one is set.** `MaxKeys` zero is no bound, as in Redis,
  and the space tests the bound before it counts, so an engine without one no
  longer counts its keys on every write. The server's cap was `-maxkeys`'s
  default, `defaultMaxKeys` in `cmd/keel`, and nowhere else, with a check
  beside it that the rewrite's key ceiling stayed in a sane relation to it;
  the ceiling is exported, as `core.RewriteKeyCeiling`. Since October 5,
  2026 the server has no key bound by default either (see "Decisions"), and
  that check became a startup warning naming the ceiling.
- **LRU is the zero policy.** `config.EvictStrategy` defaulted to random,
  which only tests ever ran under, since the flag defaults to lru. The
  policies are a typed `EvictionPolicy` in `data_structure` (`EvictLRU`,
  `EvictLFU`, `EvictRandom`), which `core` re-exports.
- **The server's settings are its flags'.** `engineOptions` in `cmd/keel` maps
  every flag onto the engine explicitly, so none of the server's settings is
  an engine default; a flag whose zero turns its setting off is passed as
  `core.Off`. The flags are checked first, with the messages the server has
  always given. `TestDefaultFlagsKeepTheServerSettings` pins what the default
  flags hold the engine to, `TestFlagsReachTheEngine` that each flag, its
  zeros included, reaches it, and `TestServerReportsItsDefaultEviction` reads
  INFO from a server started without flags.
- **The default engine** starts with `Options{}`. `core.Configure` holds it to
  the options it is given, and `cmd/keel` calls it once, after checking its
  flags and before the log is replayed, where it once assigned config before
  anything read it. Another engine is given its options when it is made:
  `newEngine(Options)` until `core.Open` (phase 3).
- **Limits by value.** A space holds its `Limits` by value. It held a pointer
  per limit so that the default space could read config live, and every
  access read the policy through one; it now reads it from the space it
  already has in hand. `SetLimits` replaces them; code outside the package
  reads them through `Limits`, `MaxKeys` and `MaxMemory`, the last two being
  what append admission and the reserve commands' budget check ask on their
  paths.
- **Tests set options on the engine they run on.** `withOptions(t, change)`
  changes the default engine's and puts back what it found when the test
  ends; a test on an engine of its own passes them to `newEngine`. This is
  the groundwork for step 2.6, which gives every test an engine of its own.
- **Resolved once, read where they were.** An engine resolves expiry's
  parameters and the log's into a `settings` value when it is given its
  options, and its role (`ReplicaOf`, `Feed`, `Protocol`) into the
  `replicationRole` value it holds; `WithDefaults()` is that resolution as
  `Options`, for a caller outside the package. The role used to be three
  pointers, so that the default engine could read config live; it is a value
  now, and `noteReplicationDirty`, which runs for every key every write
  changes, reads the feed flag with one load where it read a pointer and then
  the flag (inline cost 73, from 74). `replicaOf`, `feedsReplicas` and
  `replicationProtocol` inline at cost 4, `writable` at 22 and
  `replicationV2Enabled` at 25.
- **The log's file is the engine's.** `AppendOnly` and `AppendFilename` are
  options of the engine whose log they are, which the server's startup
  sequence (`server.StartAOF`) reads until `core.Open` runs it (phase 3).
  `AppendFilename` has no default, so `AppendOnly` needs one; the server's
  `./keel-master.aof` is `-appendfilename`'s default, and the name the log
  had before the rename is a constant of the server, which only its startup
  reads. The fsync policies are a typed `core.FsyncPolicy`, by Redis's names.
  The event loop reads whether its engine appends on a worker once, when it
  starts.
- **Each engine's settings are its own.** A test that assigned
  `config.AOFFsync` while two engines of its own ran changed both, and the
  default engine's; it now gives each engine its policy (`reconfigure`).
  `TestEnginesShareNoOptions` holds two engines and the default engine to
  different options: each expires, syncs its log and reports in INFO only as
  its own options say.
- **The server's options are the server's.** `server.Options` (`Host`,
  `Port`, `MaxClients`, `IOThreads`, `CronInterval`, `RequirePass`,
  `ConcurrentAppend`, `PrimaryPassword`, `PrimaryTLS`) are handed to
  `RunAsyncTCPServer` and `RunNetTCPServer`, which resolve the defaults once
  and pass on what each part needs: the listener its address and backlog,
  the multiplexer how many descriptors one wait reports
  (`CreateIOMultiplexer(maxDescriptors)`), the I/O pool its threads, the
  replica transport its password and TLS. A connection holds the password it
  authenticates with, set when it is accepted, so AUTH, HELLO and the check
  before every command read the connection rather than a global, and a test
  builds a client with the password it needs. `WriteUnbuffered` and
  `ActiveNetVariant`, which `-mode` sets for the benchmark modes, were never
  config; they stay with the rest of the server's package state until the
  server struct (phase 6).
- **Every flag as it was.** `TestFlagsKeepTheirNamesAndDefaults` requires
  exactly the flags the server had before the step, each with the type and
  the default `-h` lists at d56088a, so a flag cannot change or go without a
  test changing too; the tests that map the default flags and each flag onto
  the engine's and the server's options check what they do.
  `TestServerStartsWithItsListenerAndTransportFlags` starts the server
  through `main` with every listener and transport flag set: the password
  and a two-connection limit are refused and enforced as the flags say, and
  with two I/O threads, a 10 ms cron and concurrent appends on a worker the
  server serves, and reaps a key nobody reads.
- **What is left of `internal/config`** is the build's identity: `Version`,
  which the linker stamps (`-X .../internal/config.Version`), and
  `BuildVersion` and `BuildRevision`, which read it. The package keeps its
  name because the release workflow, the Dockerfile and `build-alpha.sh`
  stamp that path, and no PR check builds a release. `config_test.go` is its
  census: it fails on any package variable but `Version`, and on any code in
  the module that assigns `Version` or takes its address, as a flag would.
- **Identical benchmark workloads.** A baseline from before step 2.5 holds its
  engine to config's 5,000,000-key cap, so every one of its writes counts the
  keyspace. Every command-path benchmark gives the candidate's engine the
  same cap; without it the candidate would skip that count and look faster
  for that alone. The log-on and replica-on benchmarks, which
  `command-path.yml` builds into a baseline that predates them, take their
  settings from `command_path_settings_test.go`, and the job gives such a
  baseline `testdata/command-path/command_path_settings_test.go` in its place,
  which sets the same through config. `holdServerKeyCap` is a function of its
  own rather than a closure in `BenchmarkCommandPath`, so that the
  benchmark's closures, its timed loops, keep the names and places they have
  in a baseline's build.
- **Measured per PR**, as in steps 2.1 to 2.4: the paired command-path job at
  least twice against develop and once against `65ebdbc`. Part 1 ran at 1.003
  to 1.005 against develop on EPYC 7763, and at 0.908 to 0.913 against
  `65ebdbc`, with no more allocations anywhere; ZADD ran at 1.037 to 1.051
  on EPYC 7763, over 1.04 in six of eight runs, with ZSCORE and SADD at 1.02
  to 1.04.
  On those paths its instructions are develop's but for one load fewer in
  `Keyed.Get` (the policy, now read from the space) and one test in
  `overLimit`; the functions around them had moved. A layout control, the PR
  plus never-taken branches in nine cold functions that restored develop's
  64-byte phase for 32 of 35 hot functions, ran at 1.007 and 1.010 on EPYC
  7763 and 1.010 on EPYC 9V74 with no row over 1.04 (ZADD 1.027 and 1.035),
  and was not merged. A second experiment moved the options to the end of
  `Engine`, giving every field a command reads develop's offset and the hot
  instructions develop's: SISMEMBER then ran at 1.068 and 1.071 and SADD at
  1.054 and 1.059, with nothing on their paths reading the options, so it was
  reverted. Rows of 2 to 7% follow where the code lands, not what it does.
  Part 2 ran at 0.990 and 0.992 against develop on EPYC 7763 and 0.990 on
  Xeon 8573C, with no row over 1.04, and at 0.902 against `65ebdbc`.

### Step 2.6: parallel tests

Step 2.6 runs the suite in parallel. Every test that can run on an engine of
its own now does: core's tests on engines of their own, and cmd/keel's on
server processes of their own. What stays serial says why in a comment. The
race job, and a new job that repeats the converted packages in a shuffled
order under the race detector, then show that the engines share nothing a
test can see. No product code changes. It took four PRs:

1. **The command families** (#122): the helpers, the census and the shuffled
   job, and the order dependences that job found among tests still on the
   default engine.
2. **Persistence** (#123): the log, its admission and transcript, the
   rewrite and its failures, the persistence goldens and the allocation
   admission tests.
3. **Replication, failover, transactions and expiry** (#124), and the
   replication goldens.
4. **cmd/keel** (#125), and this section.

In core, 433 of 482 tests run in parallel, up from 5. In cmd/keel, 37 of 49
do. Measured by running each package alone, alternating the builds before
and after on the same runner (run 37519886774, 0ab9855 against the step's last
commit), the step's wall time per package was:

| Package | Linux | Linux, `-race` | macOS | macOS, `-race` |
| --- | --- | --- | --- | --- |
| internal/core | 12.3 → 6.5 s | 32.4 → 16.5 s | 16.4 → 11.5 s | 48.9 → 25.5 s |
| cmd/keel | 15.3 → 5.9 s | 153.8 → 55.9 s | 18.6 → 10.0 s | 173.3 → 70.4 s |
| internal/server | 0.1 → 0.1 s | 1.3 → 1.3 s | 0.2 → 0.2 s | 1.5 → 1.7 s |

These are medians of three rounds, or the mean of two under `-race`. In
the race job, where packages run side by side, cmd/keel was its longest
package.

Where the plan above leaves a choice open, step 2.6 settles it this way:

- **An engine per test, built by the test.** `newTestEngine(t, Options)`
  builds an engine in a space of its own, held to the options given, and
  closes it when the test ends: its log, workers, rewrite and replication
  snapshot. It takes the test's temporary directory before registering that
  cleanup, so that a directory holding the engine's files is removed only
  once the engine has let go of them. Options a test changes mid-way it
  sets with `reconfigure`.
- **Helpers take the engine.** `runOn`, `rawReplyOn`, `withAOFOn`,
  `restartOn`, `setupReplicationV2On` and the rest act on the engine they
  are given. A helper that may be handed the default engine puts its
  options back (`withOptionsOn`). Each default-engine form went once
  nothing called it.
- **Serial tests run first.** Go runs a package's serial tests before it
  releases its parallel ones, and never beside them. A test that needs the
  process to itself therefore stays serial, and says why in a comment:
  - those that read heap statistics or call `runtime.GC` (25 in core);
  - those that capture the process's log output (nine rewrite-failure
    tests in core, one shutdown test in cmd/keel);
  - those that assert on wall-clock time: two reliability tests that
    require a call to return within 500 ms, and the rewrite stall profile.
    A watchdog deadline on work that takes microseconds, and a sleep past a
    TTL, are not timing tests: a busy machine only lengthens a lower bound;
  - those that write the package's state: the test that replaces
    `ClientBuffers`, and the one that adds a command to `commandTable`;
  - the one that reads every goroutine's stack;
  - and in cmd/keel, the five that fill the kernel's socket buffers while
    other clients must be answered within deadlines, the five flag tests,
    which parse the process's command line into the default engine's
    options, and `TestServerProcess`, the entry point of a test server's
    own process.
- **The census.** `TestParallelTestsLeaveTheDefaultEngineAlone` reads core's
  source and fails if a test that calls `t.Parallel` reaches what the
  process shares:
  - the default engine, by name, through a package function that acts on
    it, through `data_structure`'s functions over `DefaultSpace`, or
    through a test helper that does any of these, followed transitively;
  - a write to a package variable, by name or through an index, a field or
    a pointer, or `delete` or `clear` on one;
  - a process-wide call: log output, heap statistics, `runtime.GC`,
    `testing.AllocsPerRun`, `runtime.Stack`, the environment, the working
    directory, rlimits and signals.

  It also fails if a method of the package reaches the default engine
  through a package function. It reads syntax, not types: package-level
  names through the parser's scopes, and test types' methods by name where
  they are called. Breaking it on purpose fails it every way it was
  broken. While the step was written it caught the replication goldens'
  pulls sending the default engine's term, and the command-table test
  marked parallel.
- **Paused tests are goroutines.** A parallel test waits as a parked
  goroutine until the serial ones finish, so a serial test now runs beside
  some 250 of them. One test looked for its own drain in a dump of every
  goroutine's stack, read into a fixed 128 KiB buffer. On CI, whose
  toolchain path is longer, the paused tests filled the buffer and cut the
  frame off. It now reads the whole dump.
- **The default engine is left as found.** The order dependences the
  shuffled job found were latent on develop. Tests still on the default
  engine left state that `ResetStores` does not clear: a failed rewrite's
  outcome, a ready replica, a stream and its dirty keys. The isolation
  tests then asserted that state's absolute values. Develop's file order
  had always run them first. Failing shuffled orders, bisected, named three
  leaking tests; a sweep found nine more.
  - Each leaking test now puts back what it changed: `CloseAOF` cancels a
    rewrite without recording a failure, `keepRewriteHistory` puts back
    what outlives a rewrite, and replication is started afresh once a
    test's options are back.
  - The isolation tests compare the default engine with what they found.
  - A sweep ran every serial test alone, followed by a probe of everything
    a test could leave on the default engine, and found nothing left.
- **Shuffled, repeated and hunted.** The race detector job's sibling, `race
  detector, shuffled and repeated`, runs core and cmd/keel three times in
  a shuffled order under `-race`. A failure is named with its package, its
  seed and the command that repeats it. Go shuffles the whole test list
  before `-run` filters it, so a seed keeps its relative order over any
  subset, which is how each failing order was bisected to the test that
  caused it. Throwaway seed hunts, never merged, gave each part:
  - in core, eight legs of ten shuffled runs and two legs of three
    shuffled `-race -count=3` runs, every run passing on each part's final
    code;
  - in cmd/keel, on Linux and on macOS, two legs of six shuffled runs and
    two of two shuffled `-race` runs: 32 runs, all passing;
  - the two failures, the goroutine dump twice, were fixed before #122
    merged.
- **cmd/keel's servers are processes of their own.** Each test server gets
  a port from below the range the kernel hands out by itself, tried once
  per process and kept only if a listener can bind it (`freePort`). A port
  the kernel picks for a listener on `:0` and closes for the server to bind
  is free only for that moment. The kernel handed such ports to other
  tests' probes while race-built servers were still starting, 7 to 14
  times in each three-pass race run. A readiness check then reached the
  probe instead of the server, and two tests failed before this changed:
  one dial was refused and one connection was reset. A server's
  file-size limit is in its own environment (`startLimitedTestServer`)
  rather than the test process's, which `t.Setenv` would change and Go
  refuses in a parallel test. Subtests that each start a server run in
  parallel, but for one test whose subtests each write and replay about
  65 MiB of logs. Side by side they held 380 MiB of files at once, more
  than the local validation wrapper's 512 MiB budget allows beside the Go
  build cache. One at a time, the package's peak is 372 MiB.
- **The largest writers are kept apart across packages.** `go test ./...`
  runs packages side by side. With core's tests faster, its serial
  `TestRewriteStallProfile` (106 MiB of logs) came to overlap one of those
  subtests (65 MiB): CI's file footprint peaked at 162 MiB, against 108 on
  develop and a warning line of 160 MiB. The worst case was the same before
  the step; develop's timing had kept them apart. The two now take one lock,
  `internal/testlock`, an flock on a file in the shared temporary directory,
  before their directories, so that one is released only once its files are
  gone.
  - It is imported only by tests, so the server's binary does not contain
    it.
  - A waiter gives up after three minutes, naming the lock and why it
    exists.
  - The kernel releases a crashed holder's lock.
  - Where there is no flock, it does nothing.
- **internal/server stays serial.** Its tests drive the server's package
  state: the client registry, the queued reads, the waker and shutdown,
  which become a `server.Server` in phase 6. They also drive the default
  engine the server runs on, until step 2.7. Nearly every test touches one
  or the other. The whole suite takes under two seconds under `-race`, so
  running its few pure tests in parallel would save nothing measurable,
  and would need a census of its own.
- **No benchmarks owed.** No product code changed. The server built with
  `go build -trimpath -buildvcs=false ./cmd/keel` is the same bytes at
  0ab9855 and after each of the step's PRs. Each PR ran the paired command-path job against develop as a
  sanity check. Each came out at a median of 0.999 to 1.007, with
  allocations unchanged on every row. Single rows moved by up to 11% on one
  host: ZADD at 1.084 and 1.109 on EPYC 9V45, and at 1.042 when repeated on
  a Xeon. The benchmarks are in the test binary, which the changed test
  files rearrange.
- **What step 2.7 inherits.** What still runs on the default engine is what
  tests it, or what cannot change yet:
  - the isolation tests `TestEnginesShareNoKeys`, `...NoCommandScope`,
    `...NoLog`, `...NoRewrite`, `...NoReplica`, `...NoReplication`,
    `...NoFailover` and `...NoOptions`, and
    `TestConfigureHoldsTheDefaultEngine`, with the helpers they use: `run`,
    `captureStreamV2`, and the default forms of `setupReplicationV2`,
    `pullV2` and `snapshotV2`;
  - the benchmarks `BenchmarkCommandPath`,
    `BenchmarkCommandPathUnderEviction`, `BenchmarkCommandPathWithLog` and
    `BenchmarkCommandPathWithReplica`, two of whose files `command-path.yml`
    copies into older baselines, so they keep the helpers those baselines
    have, `withOptions` among them; and `BenchmarkRewrite` and
    `BenchmarkSketchRewriteStart`;
  - the fuzz target `FuzzRestoreValidation`;
  - internal/server and cmd/keel, which drive the default engine through the
    package's functions (`EvalAndResponse`, `ExpireCycle`, `OpenAOF` and the
    rest), and the flag tests in cmd/keel.

### Step 2.7: the default engine

Step 2.7 removed `defaultEngine`, the engine that every caller which named
none ran on, with the 42 package functions in `internal/core` that acted on
it, and `data_structure.DefaultSpace`, its space, with the 14 package
functions over that. A command now runs on the engine its caller holds, and
`cmd/keel` holds the server's. Nothing a client, a log or a replica can see
changed: not a reply, a record or a frame, not a flag or its default, and
not the order of startup and shutdown. It took five PRs, in this order, and
a sixth (#129) fixed a flake in a guardrail that the first one met:

- **Part 1, core's tests** (#127): the census of the default engine's users
  (below), the isolation tests, `FuzzRestoreValidation`, and the helpers
  that served them on the default engine.
- **Part 2, the benchmarks** (#128), and what lets the two that
  `command-path.yml` borrows build in a baseline from before there were
  engines.
- **Part 3, the server and `cmd/keel`** (#130): `cmd/keel` makes the
  server's engine and hands it to the server, which runs every command and
  every cycle's work on it, and their tests make engines of their own.
- **Part 3b, `ClientBuffers`** (#131), the server's INFO hook, installed on
  the engine the server drives rather than in a package variable.
- **Part 4, the default engine itself** (#135): `defaultEngine`, the
  package functions over it and the package's `init`, then `DefaultSpace`
  and the package functions over it, and this write-up.

The order follows who uses what. On develop, 52 functions use the default
engine directly: 27 in core's tests, 21 in internal/server and 4 in
cmd/keel. core's tests go first because they need nothing new: each
isolation test already runs on engines of its own and uses the default
engine as one more. The benchmarks go next, and alone, because every later
part is measured with them: what they run on has to change, and be shown to
measure the same, before anything they measure does. The server goes third,
with the constructor and methods it needs, and leaves nothing that uses the
default engine. `ClientBuffers` follows it, as a part of its own that can be
reverted alone, and the last part deletes what nothing calls. Where the plan
above leaves a choice open, step 2.7 settles it this way:

- **`cmd/keel` owns the server's engine** until phase 6 gives it to
  `server.Server`. `runServer` makes it with `core.NewEngine(engineOptions())`
  where it called `core.Configure`, and then runs `server.StartAOF(e)`,
  `e.InitReplication()`, the server and `e.CloseAOF()`, in the order it runs
  them on the default engine now. The server is handed the engine:
  `RunAsyncTCPServer(wg, e, o)`, `RunNetTCPServer(wg, e, o)` and
  `StartAOF(e)`.
  - The loop runs its own work on that engine, at the points it runs it
    now: expiry, the flush, the rewrite's slices, memory maintenance,
    replica apply and the ordered-append gates; and it installs its
    allocation budget and rewrite waker on it.
  - Each connection holds the engine its commands run on, set when it is
    accepted, as a Redis client holds its `db`. `respond`, `transact`,
    HELLO's role and `executeRun`'s budget read the connection's. The field
    goes last in `client`, so no field the loop reads moves, and the struct
    grows from 312 to 320 bytes, the same allocator size class.
  - The replica transport reads that engine's role, resume cursor and term;
    the term is still read atomically.
- **`core.NewEngine(Options) (*Engine, error)`** returns an engine with empty
  stores in a space of its own, held to the options given, or the error
  `Configure` gives for options no engine can be held to. It replaces
  `Configure` on the default engine, and the package's `init`, which built
  the default engine's stores before `main` ran. `newEngine`, which panics
  on refused options, becomes a test helper over it. Phase 3's
  `core.Open` builds on it, adding the log's replay and the sidecar lock;
  the names the public package gives any of this are phase 5's to choose.
- **Each package function becomes its engine's method.** 32 of the 42 are
  already an Engine method of the same name, which the server calls on its
  engine. Of the other ten:
  - `EvalAndResponse` and `Transact` are `evalAndResponse` and `transact`,
    exported under the function's name, so that one thing has one name;
  - `SetCommandAllocations`, `CommandAllocations` and `Configuration` become
    methods of the same name;
  - `Configure` becomes `NewEngine`. Changing a live engine's options stays
    unexported (`configure`): nothing outside core does it;
  - `ResetStores` goes: a caller that needs an empty keyspace makes an
    engine;
  - `Writable`, `HeldTerm` and `Fenced` go: only tests called them, and
    those read the engine's own.

  One method is new: `Limits`, the limits an engine's space is held to,
  which cmd/keel's flag tests read from `DefaultSpace` now.
- **`DefaultSpace` goes too.** Only the default engine lives in it, so the
  last part removes it, with the 14 package functions over it, `OwnerOf`'s
  copy of the space's method among them. Their other callers (a server
  test, the eviction benchmark and the flag tests, which move with their
  parts, and an LCS test in data_structure) ask the engine or the space they
  hold. No benchmark in data_structure uses them. data_structure's census, and
  the test that checks it, then list nothing mutable; the `GOOS=windows`
  build of core and data_structure stays as it is.
- **The hot path.** No instruction a command runs in core changes.
  `EvalAndResponse` was inlined at its callers as a load of `defaultEngine`
  and a direct call of `evalAndResponse`; the server loads the connection's
  engine instead and makes the same direct call, to the same code under its
  exported name. `Transact` and the budget's reads change the
  same way. A field added to Engine goes after `settings`, its last, so
  every field a command reads keeps its offset and the hot instructions stay
  the same bytes; what is left to move a row is where code lands, which
  moved single rows by 2 to 7% either way in step 2.5. No engine method is
  deferred on the server's path (step 2.3: a deferred method is wrapped in a
  closure and called through it). And the linker drops functions nothing
  calls, so once part 3 has merged, the package functions are already absent
  from the server's binary, and part 4 removes only the initializers of
  `defaultEngine` and `DefaultSpace` from it. Each of these is checked, not
  assumed:
  - the server's dispatch (`(*client).respond`, `responseRw`, `executeRun`)
    with `go tool objdump` before and after part 3, and the connection's
    layout, `c.engine`'s offset and every other field's;
  - every new call on the path inlined or direct, with `-gcflags=-m`;
  - the addresses of the hot functions in the server's and the benchmarks'
    binaries, with `go tool nm`, before and after parts 3 and 4.
- **Measured per PR**, as in steps 2.1 to 2.6. The paired command-path job
  runs at least twice against develop and once against `65ebdbc`, where the
  step starts at 0.88 to 0.92; part 1, which changes neither the product nor
  a benchmark, runs it once as a sanity check, as step 2.6 did. A part
  passes at a median paired ratio of at most 1/0.98 (1.0204) with no row
  allocating more. A row over 1.04 is run again, and a row put down to code
  layout is shown to be layout with a control build: the part plus a
  never-taken branch in a cold function. Parts 1 and 2 change no product
  code: the server built with `go build -trimpath -buildvcs=false ./cmd/keel`
  must be the same bytes as develop's. The job measures core's command path,
  not the server's, so part 3 also runs the server end to end against
  develop: General validation's matched job, memtier against both builds on
  one runner with disjoint CPU masks, with its memory matrix.
- **The benchmarks run on engines of their own, where a baseline resets.**
  Each side builds its own `BenchmarkCommandPath` and `...UnderEviction`, so
  a baseline goes on running them on its default engine. The candidate makes
  an engine wherever the baseline calls `ResetStores`, held to the options
  set there: the server's former key cap (`serverKeyCap`) and, under
  eviction, the budget and policy. Both sides then run the same commands on
  an empty keyspace held to the same options, counting the same keys.
  `BenchmarkRewrite` and `BenchmarkSketchRewriteStart`, which the job does
  not run, move the same way. Part 2's runs against develop compare the same
  product code, so they show by themselves whether a benchmark on an engine
  of its own measures what it measured on the default engine.
- **The borrowed benchmarks build in any baseline.** `command-path.yml`
  builds the candidate's `command_path_log_bench_test.go` and
  `command_path_replica_bench_test.go` into a baseline that lacks them. For
  the cumulative comparison that is `65ebdbc`, which has no `Engine` at all,
  so from part 2 the two files name no engine API:
  - each family takes its engine from `command_path_settings_test.go`
    (`logBenchmarkEngine(b)`, `replicaBenchmarkEngine(b)`), a value of type
    `benchEngine`, and calls only `EvalAndResponse`, `OpenAOF`, `FlushAOF`,
    `CloseAOF`, `AOFStats` and `InitReplication` on it, and `mustSucceedOn`;
  - the candidate's settings file makes `benchEngine` an `*Engine` (in part
    2 a struct holding one, whose `EvalAndResponse` calls `evalAndResponse`
    until part 3 exports it);
  - `testdata/command-path/command_path_settings_test.go`, which the job
    already gives such a baseline in place of that file, makes it an empty
    struct: making one sets the baseline's config and resets its stores, as
    the borrowed files did, and each method calls the baseline's package
    function of the same name;
  - `mustSucceedOn` fails setup that errors or answers nothing, and the
    legacy file implements it the same way rather than calling a baseline's
    more lenient `mustSucceed`, so both sides hold setup to one check;
  - every method of either inlines, so in both binaries the timed loop calls
    the dispatch directly, as it does now (`-gcflags=-m`).

  The job's copy step does not change, and its provenance still names what
  a baseline borrowed. Part 2's run against `65ebdbc` is the one that shows
  the old-baseline shim builds and measures there. A baseline at or after
  step 2.4, develop included, has both files and builds its own.
- **The census of users.** `TestDefaultEngineUsersAreCensused` lists, with
  the part that moves it, every function and method in core's tests, in
  internal/server and in cmd/keel that uses the default engine directly:
  that names `defaultEngine` or `DefaultSpace`, or calls a package function
  over either. It fails on one that does and is not listed, and on a listed
  one that no longer does or is gone, so a use added while the step is under
  way fails, and so does an entry its part forgot. It reads syntax, as the
  parallel census does. A caller of a listed helper is not listed: removing
  the helper breaks it. A file in an external test package (`core_test`) is
  read as another package's, and a selector is resolved through the file's
  imports, aliases included. Part 1 added it with develop's 52 and left 38;
  part 2 left 26, all in internal/server and cmd/keel but
  `TestConfigureHoldsTheDefaultEngine`; part 3 left none; and part 4
  deleted it with the default engine, after which the compiler refuses any
  use.
- **What step 2.6 left on the default engine:**
  - **The isolation tests** run on engines of their own. Where the default
    engine stood by, a bystander engine made first takes its place, and each
    test compares on the bystander what it compared on the default engine:
    its keys and expired count, budget, log positions and file, rewrite and
    rewrite outcome, replica state, stream and epoch, term and term file,
    and options. Where the default engine took part (the primary whose
    streams `TestEnginesShareNoReplica` captures, the engine that writes
    while another rewrites in `TestEnginesShareNoLog`, the third set of
    options in `TestEnginesShareNoOptions`) an engine of the test's own does.
    They then run in parallel. `captureStreamV2` takes the engine it
    captures from, and `run` and the default forms of `setupReplicationV2`,
    `pullV2` and `snapshotV2` go.
  - **`TestConfigureHoldsTheDefaultEngine`** becomes
    `TestNewEngineHoldsItsOptions` in part 3: an engine is held to the
    options it was made with, reports them as given and in INFO, and options
    no engine can be held to make no engine.
  - **`FuzzRestoreValidation`** restores into an engine of its own, made
    once for the target and emptied before each input by `resetStores`, the
    unexported method an engine's stores are built with, as `ResetStores`
    emptied the default engine. Nothing new is exported for it, and go.yml's
    fuzz smoke is unchanged.
  - **The benchmarks**, as above; `withOptions`, `mustSucceed` and `run2`
    go with them.
  - **internal/server's tests** each make an engine of their own, whose log
    is closed when the test ends, and hand it to the connection or loop they
    drive; `withEngineOptions` goes. They stay serial: they drive the
    server's package state (the client registry, queued reads, the waker and
    shutdown) until phase 6. The package joins the shuffled race job, which
    shuffles serial tests too, so a test that depends on what another left
    fails there.
  - **cmd/keel's flag tests** build the engine `runServer` would build from
    the flags they parse, and read its options and limits. They stay serial,
    because they parse the process's command line (`flag.CommandLine`,
    `os.Args`).
- **`ClientBuffers`** is the hook through which INFO reports the server's
  connections. The plan had it go in phase 6, when the server renders INFO;
  it moves in step 2.7 instead (see "Decisions"). Part 3b gives Engine a
  field for it, after `settings`, which the server sets on the engine it
  drives (`SetClientBuffers`) as it sets its allocation budget; INFO's
  clients section and `total_connections_received` read it, and nothing
  else does, no write or eviction path among them. The server's INFO stayed
  byte-identical, which 3b showed by comparing it between develop's build
  and its own after the same connections.
  `TestINFOClientBuffersHasExplicitScopeAndStableValues`, serial because it
  replaced the package variable, then runs in parallel.
- **The censuses at the end.** core's `packageVars` loses `ClientBuffers` in
  3b and `defaultEngine` in part 4, with their reasons (`transport`,
  `defaultInstance`), leaving the 26 tables and the 57 sentinels.
  data_structure's loses `DefaultSpace`.
  `TestParallelTestsLeaveTheDefaultEngineAlone` keeps its checks of package
  variables and process-wide calls, and loses those of the default engine
  and `DefaultSpace`, with its check that no method reaches the default
  engine through a package function, a check the compiler then makes. It
  becomes `TestParallelTestsShareNoPackageState`.
- **Nothing user-visible moves.** Flags, defaults, replies, logs, frames,
  and the order of startup and shutdown, log lines included, stay as they
  are. Where a part could move a log line or a startup or shutdown step, its
  PR says how it checked that it did not; for part 3, both builds' startup
  and shutdown output is compared under the same flags.
- **Guardrails.** Every part keeps green the persistence and replication
  goldens (`testdata/persistence-40eb2f6`, `testdata/replication-9736d8d`);
  the Log compatibility and Replication compatibility workflows, which build
  the base's server and the change's and pair them both ways, so that from
  part 3 they start each through `cmd/keel`'s changed startup; native
  recovery; `BenchmarkCommandPathWithLog` and `...WithReplica` in the paired
  job; the race job and the shuffled race job; and the footprint step, under
  160 MiB. No part adds a test that writes tens of MiB or starts a server
  process; one that did would take `internal/testlock` and start its server
  through cmd/keel's port helpers.

What the step measured, part by part. The paired command-path job's medians
are against develop unless named, on GitHub's hosted runners, whose CPU
varies from run to run; no row allocated more in any run.

- **Part 1** changed no product code: the server was the same bytes as
  develop's. In core, 442 of 483 tests ran in parallel, from 433 of 482. The
  job ran 0.985 and 0.995 as a sanity check. Its Replication compatibility
  job failed once on macOS with the same server binary on both sides: a
  snapshot frame's `snapshot_bytes` differed by 28 bytes, one record header,
  because a rewrite cuts a large collection at a millisecond as well as at
  256 elements or 64 KiB. #129 now checks each run's snapshot frames against
  the snapshot they carried, and compares the snapshot as one header sized
  by its normalized body.
- **Part 2** changed no product code either. The job ran 1.000 to 1.006,
  and 0.903 against `65ebdbc`, with the legacy settings file built into it.
  SET-EX ran 1.044 to 1.051 on EPYC 7763 in four runs. Develop's benchmarks
  had used `CloseAOF` and `ResetStores` as function values, which kept those
  wrappers linked; without them the linker dropped them, and every product
  function after moved 64 or 128 bytes. A control, the part plus a test that
  is never run and keeps both linked, put every product function at
  develop's address and ran 1.000 and 0.993, SET-EX 1.007 and 0.992.
- **Part 3** ran 0.999 to 1.007, and 0.910 against `65ebdbc`. On the
  connection's path, the only instructions that changed were each load of
  `defaultEngine` becoming a load of `c.engine`; `RunAsyncTCPServer` grew 257
  bytes, all of it per cycle. End to end, memtier ran pipeline-16 at 0.970 to
  0.984 against develop in seven runs on EPYC 7763, and about 1.00 elsewhere.
  The field was the cause, though not through any instruction: a client is
  passed to `Transact` as a `core.Connection`, so its type is used in an
  interface, and its `*core.Engine` field made the linker keep `*Engine`'s
  exported methods and four `abi.(*MapType)` methods. That added 128 bytes at
  the start of the text and moved every function in the binary. Develop
  with the field added and never set put every core, data_structure and
  runtime function where part 3 has it. Against it, part 3 ran pipeline-16
  at a median of 0.995 and pipeline-64 at 0.993 over five runs on EPYC 7763,
  while the field alone, against develop, ran pipeline-16 at 0.979 and
  0.988.
- **Part 3b** ran 0.991 to 0.999, and 0.899 against `65ebdbc`; INFO was
  byte-identical to develop's in five configurations.
- **Part 4** ran 1.018 to 1.030 against develop in five runs, four of them
  on EPYC 7763 and one on EPYC 9V45, and 0.930 and 0.931 against `65ebdbc`
  on EPYC 7763; every row over 1.04 was in the test binary's placement,
  below. In the server, only the initializers of
  `defaultEngine` and `DefaultSpace` went: no other function changed size,
  and the hot ones moved 192 to 320 bytes, keeping their 64-byte phase. End
  to end, memtier ran at about 1.00 against develop on EPYC 7763, one client
  at 0.988 to 0.996, and against part 3b on EPYC 9V74. In core's test
  binary, every hot function was instruction-identical, but deleting the
  census's test code and the inits moved them all, the generic stores'
  methods, which are emitted after the package's tests, furthest; against
  part 3b the job ran 1.030 to 1.032 on Xeon 8573C and EPYC 7763. A control
  that restored the 64-byte phase of 543 of 544 hot functions ran 1.014 and
  1.021. A control that kept the deleted test code and the init-time
  allocations as code that never runs, so that every function in the binary
  was at 3b's address, ran 0.999 and 0.998; rebuilt on the rebased part, with
  every function at develop's address, it ran 1.000 and 1.002 on EPYC 7763,
  and 0.909 against `65ebdbc` on EPYC 9V74.

So every row the step ran over budget was code placement: the instructions
were the same, and only controls that restored exact addresses brought the
rows back, where restoring the 64-byte phase recovered about half. Two of the
three causes were the linker's: functions kept or dropped because something
unrelated stopped or started using them as values or through an interface.

Two results were over the gate itself, not only a row. Part 3 ran
pipeline-16 end to end at 0.970 to 0.984 on EPYC 7763, against a floor of
0.98. Part 4's paired job ran at medians of 1.018 to 1.030, against a ceiling
of 1.0204. Neither passed on the gate. Each was accepted in review as code
placement, on its control build: part 3 on develop with the field added,
and part 4 on the exact-address control, which ran 1.000 and 1.002 on EPYC
7763, where the part ran 1.025 to 1.030. The rule above was applied to the median as well as
to a row: a result put down to code layout is shown to be layout with a
control build. A later step that reads over the gate needs a control of its
own; these do not carry over.

### Phase 3: the instance contract

Phase 3 gives an engine a lifecycle, and makes it safe to share. `core.Open`
starts an engine from its options and its log, and `Close` ends it. A lock on
a file beside the log keeps a second instance off that log. A mutex in each
engine lets more than one goroutine call it. An engine that `Open` makes is
driven by a goroutine of its own, and a write an embedded caller makes is as
durable when its call returns as a server's write is when the server replies.
The server keeps driving its own engine. Nothing it shows a client, a log, a
replica or an operator changes, except what the sidecar lock adds (see "The
owner's decisions" below). All of it stays in `internal/`: the public package
and its names are phase 5's. It takes five parts, one PR each, in this order:

1. **The instance lock**: a `sync.Mutex` in each engine. The server's loop
   holds it for every cycle and releases it only while it waits in the
   multiplexer.
2. **`Open` and `Close`**: the server's startup sequence moves from
   `server.StartAOF` into core, and its replay takes a context that can stop
   it. `Open` runs the sequence on a new engine, and `Close` ends one.
3. **The sidecar lock**: `path.lock`, held from before the log is read until
   `Close`.
4. **The maintenance goroutine**: the driver of an engine `Open` makes.
5. **Calls and their durability**: the call an embedded caller makes, the wait
   for the published offset, and the `ErrPersistence` latch. This part also
   adds the phase's closing record to this section.

The order follows risk, and what each part needs. The instance lock goes
first because it is the one change on the server's hot path. Measured alone,
nothing else moves the code it is measured against, and every later part runs
on it. `Open` and `Close` come next; they change only startup and shutdown.
The sidecar lock is a part of its own because it is the one change a server's
operator can see, so it can be reverted alone. The maintenance goroutine is
the first code that touches an engine from a goroutine of its own, so it
needs the instance lock. Calls need the goroutine, which flushes for them, and
`Close`, which stops it. Where the plan above leaves a choice open, phase 3
settles it this way:

- **The instance lock.** `Engine` gets a `sync.Mutex`, which `Lock` and
  `Unlock` take and release. It goes after `clientBuffers`, Engine's last
  field, so every field a command reads keeps its offset. The contract is the
  one this plan states under "Concurrency": whoever holds the lock may touch
  the engine's state, and nobody else may.
  - **Before a driver has it**, an engine's maker is its only user and takes
    no lock. That is how `Open` replays a log, and how cmd/keel starts the
    server's log before the loop runs.
  - **No command takes it.** `EvalAndResponse` runs under its caller's lock,
    so not one instruction of a command's path changes.
  - **The server takes it once per cycle.** The loop takes the lock before it
    starts and holds it for its whole life, except for its wait: it calls
    `Unlock` just before `ioMultiplexer.Check()` and `Lock` as soon as `Check`
    returns. So every `continue` and `return` in the cycle runs under the
    lock, with no unlock of its own to forget. The loop's deferred cleanup
    (closing clients, `CancelRewrite`, removing the hooks it installed) and
    its `CloseAOF` run under the lock too. That is one uncontended lock pair
    per cycle: nothing else in the server takes it, so no cycle waits for it.
  - **io-thread workers never touch the engine.** In the read phase they parse
    a connection's bytes into commands (`readCommandsReserved`, which reads no
    engine), against the server's request budget. In the write phase they
    write a connection's reply bytes to its socket. Neither phase overlaps the
    loop's own use of the engine, because `pool.run` returns only once every
    worker has finished.
  - **The disk workers** (the append worker, the everysec sync and the
    rewrite's I/O) own only the bytes and the descriptor they were handed, and
    the atomic counters that time them. The loop polls their results under the
    lock, as it does now. The replica transport reads only the engine's term,
    atomically, as it does now.
  - **The `net` benchmark modes** take the engine's lock where they took
    `evalMu`, which the table at the top of this plan has become the engine
    lock. `EvalUnlocked` still turns it off, for the benchmark that measures
    without it.
  - **The cost** is two atomic operations per cycle, against a cycle of one or
    more system calls. Part 1 measures it, under "Measured per part".
- **`Open`.** `core.Open(ctx, o) (*Engine, error)` builds on `NewEngine`,
  which stays as it is. `NewEngine` makes the engine: its options validated,
  its stores empty, in a space of its own. `Open` then runs what the
  server's startup runs now, in the same order, and starts the engine's
  driver:
  1. If `o.AppendOnly`, the log's startup. It moves from `server.startAOF`
     into core, as the method `(*Engine).StartAOF(ctx, legacy)`, and runs:
     1. the sidecar lock (part 3);
     2. the term file beside the log (`LoadTerm`), before the log is read;
     3. the replay of the log at `AppendFilename`; or, when there is no log
        there but there is one at the legacy name the server passes, the
        replay of that one. `Open` passes no legacy name;
     4. the repair of a torn tail: the suffix is saved to a
        `.keel-torn-tail-*` file beside the log and synced, then the log is
        truncated and synced;
     5. opening the log for appending (`OpenAOF`);
     6. for a legacy log, rewriting the replayed keyspace into the new log.
  2. Replication's start (`InitReplication`): its epoch and, for a replica,
     its checkpoint.
  3. From part 4, the maintenance goroutine.

  Each step logs what the server's startup logs now, in the same words and
  order, because core logs them where `server.startAOF` did: the replay
  count, the torn tail, the legacy log and its migration, and `appendonly:
  on, …`. A step that fails undoes the steps before it: the log is closed if
  it was opened, the sidecar lock is released and the engine is discarded.
  `Open` wraps an error of the log's startup as cmd/keel does,
  `appendonly: …`, and returns the others as they come.
- **cmd/keel's startup maps onto it, unchanged.** `runServer` keeps its
  order: `core.NewEngine`, the rewrite-ceiling warning, `server.StartAOF(e)`,
  `e.InitReplication()`, the server, and the close, which becomes
  `e.Close()`.
  - `server.StartAOF(e)` becomes
    `e.StartAOF(context.Background(), legacyAOFFileName)`. The legacy name
    stays the server's constant, which only its startup reads.
  - So `Open` is `NewEngine`, `StartAOF` and `InitReplication`, in that order,
    plus the maintenance goroutine. The server runs the same three, with its
    warning between the first two, as it does now.
  - The server does not call `Open`, because its loop is its driver. Phase 6
    may fold the two.
  - The SIGTERM handler is still installed after the log is open, so a signal
    during replay still ends the process, as it does now.
- **Context-aware replay.** The replay checks its context every 1,024
  records. It never checks inside a transaction's block, which is replayed
  whole or not at all, as now. If the context is cancelled, the replay stops,
  and `Open` returns the context's error, wrapped with the log's path, so that
  `errors.Is` finds `context.Canceled` or `context.DeadlineExceeded`. That
  leaves:
  - the files as they were, byte for byte, because a replay only reads, and
    nothing has written yet. No torn tail is repaired, and no log is opened or
    created;
  - the sidecar lock released;
  - no engine, because the partly replayed one is discarded.

  A later `Open` replays from the start. The context is checked once more
  before a torn tail is repaired, and not after it. From the repair on,
  startup runs to completion, for one reason. Stopping after the new log is
  opened, before a legacy log has been rewritten into it, is the one
  interruption that loses data: the next start would prefer the new, empty
  log. Nothing else in startup takes long. The engine does not keep the
  context.
- **`Close`** ends an engine, in this order:
  1. It marks the engine closed, under the lock, so a call already running
     finishes first, and every later call gets `ErrClosed`.
  2. It stops the maintenance goroutine, and waits for it to exit.
  3. It closes the log with `CloseAOF`. That closes the replication snapshot,
     cancels a rewrite, joins the append and sync workers, writes what is
     buffered, syncs it whatever the fsync policy, and closes the file.
  4. It wakes every call waiting for durability. Each has been either covered
     or failed by the final sync.
  5. It releases the sidecar lock, last, so that no other instance can open
     the log while this one may still write to it.

  When `Close` returns nil, every write a call has returned for is on disk
  and synced, the log is closed, the sidecar lock is free, and none of the
  engine's goroutines is left. If the final flush fails, `Close` still closes
  the file and releases the lock, and returns the error. A second `Close`
  returns `ErrClosed`, as a second `os.File.Close` does. The server calls
  `Close` where cmd/keel calls `CloseAOF` now. The loop has closed the log by
  then, so `Close` only marks the engine closed and releases the sidecar lock,
  and prints nothing.
- **The sidecar lock** is an advisory lock on `path + ".lock"`, beside the
  log, as the term file is `path + ".term"`. It cannot be on the log's own
  descriptor, because a rewrite renames a new file over the path.
  - **`flock`, not `fcntl`.** An `fcntl` lock belongs to the process. A second
    `Open` of the same log in the same process would get it again, and
    closing any descriptor of the file, a test's read of it included, would
    drop it. A `flock` lock belongs to the open file. Two engines in one
    process conflict as two processes do, and only closing the descriptor
    `Open` took releases it. On Linux, `flock` over NFS is carried out with
    `fcntl` locks, so there two engines in one process are not kept apart,
    though two processes still are.
  - **Platforms.**
    - Linux, macOS, FreeBSD, NetBSD, OpenBSD and DragonFly use `flock`.
    - Windows opens the lock file with no sharing (`CreateFile` with share
      mode 0). Any second open then fails with a sharing violation, in the
      process or outside it, until the handle is closed or its process ends.
    - Elsewhere (AIX, Solaris, illumos, Plan 9, js and wasip1) there is no
      lock, and the documentation says so. None of them runs the server.

    core and data_structure keep their `GOOS=windows` and `GOOS=freebsd`
    builds. The tests run on Linux and macOS in CI. Part 3 also runs them
    once on a Windows runner, in a throwaway job, so that the Windows lock is
    shown to work and not only to build.
  - **The error** is `ErrLocked`, whose text is `log in use by another
    instance`. It is returned as `fmt.Errorf("%w: %s", ErrLocked, lockPath)`.
    The server prints it as it prints any startup error, and exits with
    status 1: `appendonly: log in use by another instance:
    ./keel-master.aof.lock`. A lock file that cannot be created or opened is
    an error too, naming its path. Where the filesystem does not support locks
    at all (`ENOTSUP`, `EOPNOTSUPP` or `ENOLCK`), the engine opens without
    one, and logs `appendonly: <path>.lock: <error>; nothing keeps a second
    instance off this log`.
  - **After a crash** there is no stale lock to clear. The kernel releases a
    dead process's locks, so the file is left behind, empty and unlocked, and
    the next `Open` takes it. The file is never removed, because removing a
    lock file races with whoever opens it next: one instance would hold a lock
    on the removed file, and another on its replacement.
  - **The server takes it too**, through the startup it shares with `Open`;
    this is the owner's decision, below.
- **The maintenance goroutine** (part 4) is the driver of an engine `Open`
  makes, as the event loop is the server's. It does the loop's per-cycle
  work, in the loop's order, under the lock: the expiry cycle; the flush
  (`FlushAOF`, which writes the buffer, syncs as the policy says, and advances
  a rewrite by a slice); and memory maintenance, once a second.
  - **When it runs a cycle.** Every 100 ms, which is the server's default
    `-cron-interval-ms` and Redis's default `hz` of 10. Also whenever a call is
    waiting for buffered log records, and whenever a disk worker or a
    rewrite's I/O finishes; it installs its wake with `SetRewriteWaker`.
    While a rewrite has slices left, it runs the next cycle at once, as the
    loop wakes itself. It takes the lock afresh for each cycle, so calls run
    between slices.
  - **Everysec** is due within a tick of the second, as on the server.
  - **The flush runs under the lock**, as the loop's does. A call that
    arrives while the log is being written or synced waits for it, as a
    client of the server does. Disk I/O stays on this goroutine and the
    workers that exist; no caller's goroutine writes the log.
  - **`Close` stops it.** It closes a stop channel and waits for the
    goroutine to exit before the log is closed, so the goroutine never
    flushes a closed log.
  - **Options only the server can drive** are refused by `Open`.
    `AsyncAppend` means something only with the loop's back-pressure: while a
    batch is out, no command runs. Here, the maintenance goroutine already
    keeps the log off the callers' goroutines. A replication role
    (`ReplicaOf`, `ReplicationFeed`) needs the server's transport, and this
    plan keeps replication server-only.
- **Calls** (part 5). `(*Engine).Do(ctx, cmd, w) error` runs the command
  language for an embedded caller, until phase 4's typed operations replace
  it. It checks its context, takes the lock, and refuses with `ErrClosed` or
  the persistence latch. Otherwise it runs the command in the scope
  `EvalAndResponse` gives it, notes the log's end (`AppendOffset`), and
  releases the lock. It unlocks at each return rather than with `defer`, as
  step 2.3 did for `aofEnd`. A panic in a command is not recovered, as on the
  server, which it ends; it leaves the engine locked.
- **Durability.** A call returns once the published offset,
  `appendCompleted`, covers the log's end as it was when the call released
  the lock. Under `always` that offset means synced, and under `everysec`
  and `no` it means written: the gate the server holds its replies to.
  - **Group commit.** If its records are still buffered, a call wakes the
    maintenance goroutine and waits, outside the lock, for the next flush to
    be published. That flush covers every call that buffered records since
    the last one: one write and, under `always`, one sync, for all of them.
    This is the group commit the plan takes from the existing append offsets.
  - **Reads wait too**, when anything is buffered. A read may have seen a
    write that is not on disk yet, and no client of the server is told what a
    crash could still lose. A call that neither buffered records nor saw any
    buffered does not wait. With no log, nothing is ever buffered, and a call
    is the lock, the command and the unlock.
  - **Cancelled while waiting**, a call returns the context's error. Its
    command has run and its record will be written, as a Redis client that
    times out has its command run.
- **The latch.** A failed flush or sync latches. The maintenance goroutine
  records the error and wakes every waiting call. From then on, every call,
  read or write, returns `ErrPersistence` wrapping the cause, without running.
  A waiting call whose records were published before the failure still
  returns nil. Nothing clears the latch: close the engine and open it again,
  which replays what reached the disk. The server never latches: it stops at
  the first failure, as it does now.
- **Error sentinels.** Phase 3 adds three to core:
  - `ErrClosed`, `instance is closed`;
  - `ErrLocked`, `log in use by another instance`;
  - `ErrPersistence`, whose text is Redis's wire error for a failed AOF
    write, `MISCONF Errors writing to the AOF file`. `Do` returns it as
    `MISCONF Errors writing to the AOF file: <cause>`, and `errors.Is`
    matches both the sentinel and the cause.

  They are internal: nothing outside the module can name them, and no reply
  or log line the server writes changes, except `ErrLocked`'s refusal.
  Phase 5 exports them from package keel as the same values, so that
  `errors.Is` holds across the layers. With them it exports the command
  errors that phase 4 sorts into categories (`ErrWrongType`, `ErrNotFound`,
  `ErrNotInteger`, `ErrOverflow`, `ErrOutOfMemory`, `ErrReadOnly`), whose wire
  text does not change.
- **Census.** The three sentinels join core's `packageVars` as sentinels.
  Nothing mutable joins it. The instance lock, the lock file, the closed
  state, the maintenance goroutine and the latch are fields of the engine,
  after `clientBuffers`.
- **Nothing user-visible moves** for the server, except what the sidecar lock
  adds: not a flag or its default, a reply, a log line, a frame, or the order
  of startup and shutdown. Each part that changes the server says how it
  checked. Parts 1 to 3 compare both builds' startup and shutdown output
  under the same flags, as step 2.7's part 3 did. They add a legacy log, a
  torn tail, a damaged log and a damaged term file, each of which must give
  the same lines and the same refusal on both builds.
- **Tests, by part.**
  - **Part 1.** A test drives the server's loop while another goroutine holds
    its engine's lock, and the loop serves nothing until the lock is
    released. The race job runs it, and every server test, under `-race`.
  - **Part 2.**
    - `Open` replays a log, repairs a torn tail, and refuses a damaged log
      and refused options, with nothing left open.
    - A context that cancels after a given number of records, counted by the
      test rather than timed, stops a replay. Every file is then the same
      bytes as before; there is no torn-tail backup, no log is created, and a
      later `Open` replays all of it.
    - `Close` then `Open` gives back the keyspace under each fsync policy.
    - The server's legacy-log tests keep running through
      `server.StartAOF`.
  - **Part 3.**
    - A second `Open` of a log in the same process gets `ErrLocked`. After
      the first engine's `Close`, a new `Open` succeeds.
    - A child process holding the lock makes `Open` fail. Killed with
      SIGKILL, it leaves the lock free.
    - `Close` releases the lock, and the lock file survives.
    - A second server on a log another server holds refuses to start, with
      the line above.
  - **Part 4.**
    - Keys expire with nobody reading them.
    - An everysec log is synced with no calls after the write.
    - A `BGREWRITEAOF` finishes with no further calls.
    - `Close` leaves none of the engine's goroutines behind.
  - **Part 5.**
    - **Kill tests.** A child process opens an engine under each fsync
      policy, writes from several goroutines, and reports every write as its
      call returns. The parent kills it with SIGKILL at an arbitrary point,
      once while a rewrite runs, and reopens the log. Every reported write must
      be there, and a torn tail must have been repaired.
    - **The latch,** through failing write and sync hooks.
    - **Group commit:** one sync covering several waiting calls, counted
      through the sync hook.
    - **Cancellation** while a call waits.
    - **Contention under `-race`:** two engines, eight goroutines on each,
      each goroutine on keys of its own. Both engines are reopened, and each
      goroutine's keys must match its own model.
- **Measured per part.**
  - **The paired command-path job**, as in step 2.7: at least twice against
    develop, and once against `65ebdbc`. A part passes at a median of at most
    1/0.98 (1.0204), with no row allocating more. A row over 1.04 is run
    again. A row put down to code layout is shown to be layout with a control
    build of its own; step 2.7's controls do not carry over.
  - **End to end.** Parts 1 to 3 change the server's binary, so each also
    runs General validation's matched job against develop: memtier against
    both builds on one runner, with its memory matrix.
  - **Part 1's evidence:**
    - the loop and the per-command functions (`executeRun`,
      `(*client).respond`, `responseRw`, `EvalAndResponse`) compared with
      `go tool objdump` against develop's. Only the lock pair around `Check`
      may differ;
    - `Lock` and `Unlock` inlined at the loop (`-gcflags=-m`);
    - the hot functions' addresses compared with `go tool nm`, as step 2.7
      did.
  - **Part 5** adds a benchmark of `Do`: with no log, under everysec and under
    always; on one goroutine and on eight; against `EvalAndResponse` with no
    lock. It is reported but not gated, because there is nothing to pair it
    with. Its name keeps it out of the paired job's `^BenchmarkCommandPath`,
    which a baseline without it would fail.
- **Guardrails.** Every part keeps green:
  - the persistence and replication goldens (`testdata/persistence-40eb2f6`,
    `testdata/replication-9736d8d`);
  - the Log compatibility and Replication compatibility workflows. From part
    2 they start each build through the changed startup, and from part 3
    they restart servers on logs that a killed server held locked;
  - native recovery;
  - `BenchmarkCommandPathWithLog` and `...WithReplica` in the paired job;
  - the race job and the shuffled race job;
  - the footprint step, under 160 MiB.

  The kill tests write a few KiB. A test that writes tens of MiB takes
  `internal/testlock`'s `HoldDiskHeavy`, and a server test starts its server
  only through cmd/keel's `startTestServer` and its siblings.

- **The owner's decisions.** These are the owner's to make, and each part
  follows whichever way they are made:
  - **Whether the server takes the sidecar lock.** This plan recommends that
    it does: one startup for both, and a second server on one log is silent
    corruption today. Two things become visible: a `keel-master.aof.lock`
    file beside the log, and a refusal to start, with the line above, for a
    second server on a log another holds. Redis takes no such lock. The
    alternative is that the server skips it until phase 6.
  - **The latch, against Redis.** Under `everysec`, Redis refuses only writes
    after a failed AOF write, with `MISCONF`, keeps serving reads, retries
    the write, and clears the refusal once a retry succeeds. Under `always`,
    Redis exits. This plan latches every call, reads included, until the
    engine is reopened, as the accepted "Concurrency" section says. That is
    stricter than Redis, and matches the server, which stops.
  - **`Close` without a context.** core's `Close` takes no context, and a
    second call returns `ErrClosed`. The API sketch has `db.Close(ctx)`;
    phase 5 chooses the public signature.
  - **The sentinels' texts**, above, which phase 5 exports.

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
  defaulting to random is the server-side inconsistency to remove in step 2.5
  (removed: LRU is the zero `core.Options.Eviction`).
- **Key limit default:** none, as in Redis, where only `maxmemory` bounds the
  keyspace. `MaxKeys` defaults to zero, meaning unlimited. Step 2.5 first made
  the server's 5,000,000 cap an explicit `-maxkeys` default rather than a hidden
  one. On October 5, 2026 the owner chose Redis's default for the server too:
  `-maxkeys` now defaults to 0. The rewrite's four-million-key ceiling and the
  snapshot's one-million-key refusal stay documented limits of persistence, not
  of the keyspace, and a server started with `-appendonly` and no `-maxkeys`
  at or below the rewrite ceiling logs a startup warning naming it. (`-maxmemory`
  bounds bytes rather than keys, so it does not hold the count down.)
- **`Atomic` semantics:** as Redis EXEC. Queued work is isolated and logged as
  one frame; a command that fails inside it does not undo the others, and there
  is no rollback.
- **Release target:** the first embeddable release is `v0.2.0-alpha.1`.
- **`ClientBuffers` moves in step 2.7, not phase 6** (October 6, 2026). Once
  the server holds its engine, the hook is a field and a setter on it, as the
  allocation budget already is; as a package variable, it has every engine in
  a process report the server's connections in INFO, and it is the last
  mutable entry in core's census. It moves in a PR of its own after the
  server's (3b), so that the server's measurements stay clean and the move can
  be reverted alone. Phase 6 still moves INFO's rendering into the server.
