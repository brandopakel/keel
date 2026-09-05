> Follow-up implementation and current status: [engineering delivery](engineering-delivery.md).
> Findings below describe the reviewed baseline.

# Engineering and product review — 2026-09-04

Keel has implemented most of the mechanisms described in its README. It has not finished its documented roadmap, and several implemented mechanisms still fail in ways that prevent a dependable release for other projects. The next milestone should be a reliable, narrowly defined service that people can install, integrate, operate, and upgrade.

The strongest product opportunity is a compact service for memory-conscious caching and approximate counting. That is a product hypothesis to validate with users and workloads, not an established competitive advantage.

## Scope and evidence

- Working tree: `rewrite/event-loop-and-entrypoint`, commit `f7c8e522a0c5`. The tree was clean before this review. No implementation changes were made; this report is the retained repository change.
- GitHub default branch: `develop`, commit `d27846a334d680aa78cfe268c3a9d35e0c88cd1b`. Its README blob matches local `origin/develop`; the working branch only removes the image reference from that README.
- [PR #11](https://github.com/brandopakel/keel/pull/11) was open during review. Its Linux Go 1.22/stable, Docker, race, and CodeRabbit checks were successful. The host-binding fix and socket rewrite in this checkout are therefore not yet on the default branch.
- GitHub reported no tags or releases; issues are disabled. `chore/licence` exists and contains an MIT LICENSE, but neither this checkout nor the default branch has that file. This is an unfinished distribution task; this review does not independently establish provenance or make a legal determination.
- Reviewed the repository's implementation packages, entrypoint, command registrations, persistence and serialization, data structures, tests and coverage patterns, Dockerfile, CI/release workflows, benchmark scripts and summaries, README/design log/roadmap, benchmark documentation, and third-party notices. Inventory: 111 Go files, including 50 test files. This is a codebase-wide engineering review, not a formal proof of every algorithm or a sustained fuzzing audit.
- Intent is inferred from those checked-in docs and the open PR. Undocumented plans or external project documents were not available.
- Local environment: Go 1.26.6, Darwin/arm64. `go vet ./...` passed. A fresh `go test -count=1 -json ./...` with socket access passed all 480 test cases/subtests. The corresponding race run passed 479 and skipped `TestRewriteStallProfile`, whose timing assertions deliberately exclude race instrumentation. Packages without tests were reported separately.
- Additional isolated loopback probes used temporary AOF files and disposable server processes. Temporary in-package probes checked accounting and fsync scheduling and were removed afterward. The findings below distinguish reproduced failures from control-flow findings.
- Historical throughput benchmarks, Linux runtime behavior, physical power-loss recovery, and production-scale soak tests were not remeasured locally. Linux/Docker evidence comes from the PR's CI, not a new local run.

## README and roadmap matched to implementation

| Intended capability | Implementation evidence | Assessment |
| --- | --- | --- |
| RESP and ordinary Redis clients | `core/resp.go`, `eval.go`, `server/server.go`, protocol and pipeline tests | Useful RESP2 command subset. Malformed bulk terminators are accepted; protocol support does not establish full Redis command compatibility. |
| epoll/kqueue event loop and reply batching | `core/io_multiplexing/`, `server/server.go`, `iothreads.go` | Built on Linux/macOS. Writes still fall back to blocking sockets, so client isolation is incomplete. |
| Optional parallel socket I/O | `server/iothreads.go` | Built. Command execution remains serial, and the phase barrier still waits for slow writers. This does not provide parallel execution or sharding. |
| Strings, hashes, lists, sets, sorted sets, geo | `core/commands_*.go`, `data_structure/` | Core types implemented and tested. Supported options and useful command sequences remain narrower than a Redis client API suggests. |
| Bloom, CMS, HLL, cuckoo, Morris | Corresponding command and data-structure files | Built. Morris/cuckoo allocation and restore validation need hardening. HLL is dense only; cuckoo capacity is fixed, as documented. |
| Shared memory budget and LRU/LFU | `data_structure/evictor.go`, `dict.go`, `keyed.go`, `memory.go` | Built across ten registered keyspaces. Accounting and allocation-order bugs remain. Budget estimates cover the keyspace, not total process RSS. |
| One key owner; keyspace commands | `core/keytype.go`, `storage.go`, `commands_keyspace.go` | Built. LCS remains the documented type-check exception. Some manually maintained type lists have drifted. |
| TTL and active expiry | String dictionary expiry table; `core/expire.go` | Built for strings only, in the default event-loop server. Collections and sketches cannot expire. |
| Linear-memory LCS | `data_structure/lcs.go`, `commands_lcs.go` | Built with a work limit and tests. It still runs synchronously on the execution thread. |
| Optional AOF with fsync policies | `core/aof.go`, event-loop flush hook | Built, but recovery, idle sync, error handling, and alternate-mode integration do not yet meet the documented guarantees. |
| Incremental AOF rewrite | `core/aof_rewrite.go` | Built, including dirty-key reconciliation and tests. Initial key collection, final dirty flush, large individual values, and file I/O can still cause pauses. |
| Graceful shutdown | `server/shutdown.go`, `cmd/keel/main.go` | Signal-driven path exists. Startup/internal server failures are not propagated through main; alternate modes omit AOF close. |
| Installation and distribution | Go command/module, Dockerfile, release workflow | Packaging machinery exists. No tagged release or downloadable release assets existed during review; no published image workflow is present. |
| Embedding in another Go project | All implementation packages under `internal/`; globals throughout | Not built, accurately disclosed. Socket integration is the current supported shape. |
| SCAN, MULTI, pub/sub, list operations | Absent from dispatch | Documented backlog remains: SCAN, transactions, pub/sub, LTRIM/LREM/LINSERT, blocking list commands. |
| Authentication, TLS, replication | No handlers/configuration/replication subsystem | Not built, accurately disclosed. Default bind is all IPv4 interfaces. |
| Further measurements | Benchmark harness and historical results | AOF overhead and stronger I/O-thread measurements remain explicitly outstanding. The harness also needs repairs before new claims. |

The dispatcher has **88 registered names**, or **85 excluding `MEMKV.DUMP`, `MEMKV.RESTORE`, and `SRAND` aliases**. The README says 82. `PTTL`, `BF.ADD`, and canonical `SRANDMEMBER` are missing from its command table. A command count should state its counting convention and preferably be generated.

## Findings to fix before a dependable release

Priorities below describe engineering order. P0 means stop release work until resolved; P1 means required for the reliability/integration milestone; P2 means subsequent product development.

### 1. P0 — Small requests can crash the server

**Reproduced:** `MORRIS.INITBYDIM m 4294967295 4294967295` closes the connection and crashes with `makeslice: len out of range`. `cmdMORRISINITBYDIM` accepts dimensions and calls `CreateMorris` without the preallocation checks used by CMS. The failure is before any attempted enormous allocation in this reproduction.

**Reproduced:** a 49-byte cuckoo restore payload also crashes with `makeslice: len out of range`. `UnmarshalCuckoo` checks the bucket/slot relationship, then allocates the declared slot count before validating that the payload contains those slots. The test payload is type byte 8 followed by six little-endian uint64 values: `[2^61, 0, 0, 0, 1, 2^63]`.

Locations: `internal/core/commands_morris.go:23`, `internal/data_structure/morris.go:127`, `internal/data_structure/serialize.go:209`, `internal/core/commands_dump.go:171`.

Related code-inspection concerns: `CF.RESERVE` also lacks the CMS/Bloom preallocation budget check; HLL restore validates buffer length but not register values used as histogram indexes; Bloom restore does not validate all invariants its lookup/growth code assumes. Treat these as one validation audit, not two isolated patches.

**Completion criterion:** all reserve/init/restore routes validate dimensions, multiplication overflow, payload length, structure invariants, and allocation budget before allocation or mutation. Add parser/restore fuzz targets and bounded subprocess crash regressions. Malformed input must produce a bounded error response and leave existing data intact.

### 2. P0 — Crash-tail recovery breaks the following restart

**Reproduced:** create an AOF containing a complete SET followed by a partial SET; start with AOF enabled; a new SET receives `OK`; stop cleanly; start again. The second startup refuses the file as malformed.

`LoadAOF` detects a partial final frame, and `StartAOF` accepts it, but `OpenAOF` appends after that partial frame. The partial bytes are never removed. Newly acknowledged commands turn a recoverable tail into corruption inside the log.

Locations: `internal/core/aof.go:338`, `internal/server/server.go:688`, `internal/core/aof.go` `OpenAOF`.

**Completion criterion:** retain the last valid byte offset, repair the tail before accepting writes, and test crash → recovery → new writes → second recovery. Replay must also reject command execution errors: currently replies are discarded, so a recognized command returning an error can be counted as applied. Keep a recoverable original when repair itself fails.

### 3. P0 — Durability acknowledgements and fsync scheduling are incomplete

**Control-flow finding:** after `core.FlushAOF()` returns an error, the default loop sets a stop flag but still reaches `pool.run(writable, true)`. The successful command replies staged before the failed flush can therefore still be sent. In `kqueue-nobuf`, replies are sent before the flush in the first place.

**Reproduced with an in-package probe:** after a write under `everysec`, an idle `FlushAOF()` leaves an overdue `lastSync` unchanged. `flushAOF` returns early when the command buffer is empty, before checking the sync deadline. An acknowledged write can remain unsynced by Keel beyond the advertised interval until another write or close. OS writeback does not substitute for an explicit fsync guarantee.

Locations: `internal/server/server.go:624`, `internal/server/server.go:657`, `internal/core/aof.go:301`.

**Completion criterion:** no success response for a failed required write/sync; independently schedule syncing of dirty file data even when no commands arrive; make persistence mode guarantees explicit and enforce them uniformly. Inject write/sync errors in tests. Propagate directory-sync failures relevant to rewrite durability rather than discarding every `Sync` error in `syncDir`.

### 4. P0 — Advertised flags can silently lose all writes in alternate I/O modes

**Reproduced:** start with `-mode net -appendonly`, SET a key, receive `OK`, and shut down with SIGTERM. The AOF is **zero bytes** afterward.

`RunNetTCPServer`/`handleConn` execute commands that fill the global AOF buffer, but do not drive `FlushAOF`, active expiry, rewrite advancement, or `CloseAOF`. Other `net*` modes share this integration gap. These are described as benchmark modes, but the CLI allows combining them with persistence without rejecting the combination.

Location: `internal/server/server_net.go`.

**Completion criterion:** either implement the lifecycle hooks under the same execution synchronization, or reject unsupported combinations at startup. Exclude the deliberately unsafe `net-nolock` diagnostic mode from normal product entrypoints. Add a mode/flag integration matrix including restart tests.

### 5. P0 — Memory accounting can wrap to 18 exabytes

**Reproduced in an empty keyspace:** `SET n 9`, `INCR n`, `DEL n` leaves estimated memory at **18446744073709551615 bytes**. INCR mutates the string from one byte to two without updating `memUsed`; deletion subtracts the new size from the old charge. This can drive inappropriate eviction under a memory limit.

Two related probes also fail: with a 120-byte budget, `SET k v EX 60` retains 150 estimated bytes, because expiry is added after budget enforcement. With a 100-byte budget and an oversized SET with expiry, the value is evicted but an orphan expiry remains: zero keys, one expiry entry, 48 charged bytes.

Locations: `internal/core/commands_dict.go:242`, `internal/data_structure/dict.go:95`, `internal/data_structure/dict.go` `Put`/`Del`, `internal/data_structure/memory.go`.

**Completion criterion:** one mutation path updates value size, expiry, and budget consistently; no expiry for an absent key; no unsigned underflow. Exercise size-changing increments, overwrites, eviction during creation, expiry, deletion, and replay as command sequences. Test that an empty keyspace has zero logical charge.

### 6. P1 — One slow reader can stop service for other clients

**Reproduced:** store a 4MB value; have a client request it repeatedly while not reading; an independent client's PING times out after one second. Closing the slow client releases the stall.

`FDComm.Write` switches to a blocking descriptor after `EAGAIN`, without a deadline. I/O threads do not solve this: the loop waits at the phase barrier. The README's nonblocking/event-loop framing is incomplete without this qualification.

Locations: `internal/core/comm.go:64`, `internal/server/iothreads.go` `serve`/`run`.

**Completion criterion:** write-ready events, partial-write offsets, bounded per-client output queues, stalled-client timeouts, and a per-cycle work budget. The multiplexer API will need update/remove interest support: Linux currently uses only `EPOLL_CTL_ADD`. Test that slow readers do not prevent other clients, expiry, or shutdown from making progress. Bound aggregate input/output memory as well as keyspace bytes; the current 1GB per-client query cap is not a deployment memory limit.

### 7. P1 — A failed Morris batch mutates data that is not logged

**Reproduced:** initialize a Morris table, then run `MORRIS.INCRBY m a 1 b invalid`. The command returns an error, but querying `a` returns 1. After a clean AOF restart it returns 0.

The handler parses and mutates pair by pair. The AOF assumes a top-level error means no mutation and omits it. Successful earlier pairs therefore disappear on restart. Failed cuckoo inserts also advance RNG state that the normal AOF does not record; exact probabilistic replay deserves a broader audit, especially around failed operations.

Locations: `internal/core/commands_morris.go:74`, `internal/core/aof.go` `aofCommit`.

**Completion criterion:** validate every argument before mutation, or explicitly record all effects of partial failure. Verify serialized state across sequences containing successes, errors, evictions, rewrites, and restarts, not just successful commands on a fresh table.

### 8. P1 — Protocol decoder accepts malformed frames

**Reproduced:** the byte sequence `*1\r\n$4\r\nPINGxx` receives `+PONG\r\n`. The bulk decoder advances past two trailing bytes without verifying CRLF. Negative bulk lengths other than -1 are also treated as empty strings.

Location: `internal/core/resp.go:167`.

The array decoder also allocates based on the declared element count before receiving the elements, and partial frames are parsed again as they grow. Per-frame length limits alone do not establish a small allocation/CPU budget for incomplete input.

**Completion criterion:** strict framing and command element validation, adversarial fragmentation/fuzz tests, and limits on parser allocations and work per connection. Keep error responses valid even when user-provided names contain control bytes.

### 9. P1 — Startup/internal failures do not terminate main reliably

**Control-flow finding:** main launches a server function returning an error as a goroutine and ignores the return value. Its wait group also waits for `WaitForSignal`, which blocks on an external signal. Bind failure can leave a live process with no listener; internally requested shutdown has the same signal-wait problem.

Locations: `cmd/keel/main.go:164`, `internal/server/shutdown.go:82`.

This matters to service managers, container restart behavior, and benchmark identity. PR #11 fixes address parsing and reuse, but retains this lifecycle problem.

**Completion criterion:** main owns a shared cancellation/error path, returns a nonzero status on startup/runtime failure, and always closes AOF and clients. Add process tests for port conflicts, invalid addresses, failed persistence, SIGTERM, and a slow client during termination. Also validate size multiplication overflow in `parseSize` and reject nonsensical signed configuration values.

### 10. P1 — Common integration paths are incomplete

**Reproduced:** `MEMORY USAGE` returns nil for an existing hash or list, and `KEEL.DUMP` returns nil for an existing hash. The manually maintained lookup chains omit these types. Hash/list AOF rewrite itself does have explicit emitters, so this finding does not imply those types are universally absent from persistence.

**Reproduced:** `EXPIRE h 60` on a hash returns WRONGTYPE; `SET lock v NX` returns a syntax error. The code binds TTL commands to the string keyspace and SET only accepts basic EX/PX forms.

Locations: `internal/core/commands_server.go:43`, `internal/core/commands_dump.go` `dumpKey`, `internal/core/keytype.go`, `internal/core/commands_dict.go`.

Other important boundaries: SET/MSET refuse replacing another type by design; LCS does not check other keyspaces; HLL reports its own type; SELECT/AUTH/HELLO/CLIENT/COMMAND are absent; no transactions or scripting; sorted sets lack ZRANGE. Redis libraries may connect but application features built on these operations can fail.

**Completion criterion:** publish an option-level compatibility matrix and test real client libraries in CI. Prioritize cross-type TTL, SET NX/XX and required expiry forms, PERSIST/PEXPIRE, INCRBY, ZRANGE, and LTRIM according to chosen applications. Add SCAN when operational key enumeration is a requirement. Do not promise locks, job queues, or application rate limiters until their atomicity and retry behavior are actually supported.

## Scaling and operational engineering

The current architecture scales connection handling more readily than CPU-heavy command execution. All stores, configuration, eviction state, persistence state, and much server lifecycle state are global. Moving folders out of `internal/` would expose those globals without creating a safe reusable library.

For a service-first release, keep the socket boundary and provide tested clients, deployment examples, a readiness check, health reporting, versioned configuration, upgrades, and backup/restore instructions. If embedding is a concrete user requirement, first create an instance-owned `Engine` with explicit options, lifecycle, concurrency guarantees, and independent storage/AOF state. Verify that two engines can run without sharing keys or configuration. This refactor also prepares future partitioning.

There are three distinct scaling tasks:

| Scale dimension | Required work |
| --- | --- |
| More connections and bursty clients | Nonblocking output, admission limits, aggregate buffer budgets, deadlines, fair command scheduling; measure p99/p99.9 under mixed workloads. |
| More data and sustained operation on one node | Correct accounting, map/churn heap calibration, large-key limits, streaming AOF replay, rewrite byte/time budgets, bounded dirty tracking, GC/RSS headroom, observability. |
| More throughput/capacity or availability across nodes | Explicit partition ownership, routing and resharding, cross-partition command rules, snapshot/replication offsets, catch-up, failover and split-brain handling. None exists today. |

`LoadAOF` reads the whole history into memory while reconstructing the dataset and suspends eviction until the end. AOF startup can therefore require substantially more memory than the final dataset. Rewrite slices are 2,048 **keys**, not bytes or elapsed time: large collections can still allocate/write enormous batches, and the final dirty pass is unbounded. Fixing only the README's 8–13ms endpoint examples will not establish predictable latency for arbitrary values.

Operational gaps to fill for shared deployments: authentication and an explicit TLS story, conservative bind defaults, command restrictions for destructive/administrative operations, noeviction or workload-specific eviction choices, and observable failure states. Existing INFO covers memory estimates, eviction/expiry counts, basic persistence sizes, and key counts; it does not yet give client counts, request/error rates, hit/miss ratios, latency, RSS/GC, queued bytes, or sync/rewriting failure status. AOF is local recovery, not high availability or an independent backup.

For approximate structures, eviction needs particular care: dropping an entire membership filter changes application behavior, not just cache hit rate. Consumers need clear missing-key handling, expiry/window semantics, and separation between disposable caches and retained aggregates.

## Benchmark evidence needs another engineering pass

The docs preserve mistakes and qualify many results, which is valuable. The current scripts still contain discrepancies with that standard:

- `run-memory.sh` reads RSS **after** its Python client exits and closes its sockets. It divides by the requested number of connections even if fewer connected. This does not directly measure memory while the stated connections remain open.
- `run-ab.sh` verifies exact listener PID, but `run-matrix.sh`, `run-iothreads.sh`, `run-memtier.sh`, and `run-hyperfine.sh` still use process-name substring checks. The README's PID-safety lesson has not propagated through the harness.
- Several scripts use broad `pkill` patterns instead of cleaning up only their own children. Failure paths can continue with zero/missing results. Summary scripts filter out nonpositive samples, so missing runs may disappear from the comparison.
- `run-memtier.sh` reuses `/tmp/mt.json` and does not require each benchmark command to succeed; a failed run risks using stale results. The CI latency command also uses `|| true`, and its artifact upload only retains the memory CSV.
- `bench.yml` requests Go 1.21 despite `go.mod` requiring 1.22. Automatic toolchain selection may compensate, but that does not make 1.21 the tested runtime. Record and intentionally select the actual toolchain.
- Benchmark docs say noise floors differ by configuration, while `summarise.py` and `summarise-iothreads.py` still use a global approximate 13% threshold. The main summary only presents four of the seven configured server modes.
- The docs already qualify large-value results as historical after read-buffer changes. Later rewrites also mean old results should remain attached to their measured commit, not silently become performance claims for the current revision.

These findings do not retroactively prove every recorded run wrong. They mean the present harness is insufficient to independently substantiate all current marketing claims.

**Completion criterion:** record commit, binary checksum, Go/runtime/OS/CPU, flags, actual connections and workload shape; hold connections open during sampling; enforce exact process identity and owned-process cleanup; reject missing/error samples; archive raw latency and correctness evidence; compare matched durability and memory settings. Then rerun AOF-off/always/everysec, rewrite-under-load, churn, slow-client, long-run, and network-separated workloads. Keep before/after algorithm microbenchmarks separate from claims against other databases.

## Documentation and distribution corrections

The existing README is effective as a design journal but incomplete as an integration contract. Separate the landing README, command compatibility reference, architecture decisions, operations guide, and measured results.

Concrete corrections:

- Refresh the command count/table and remove the duplicated LCS warning.
- State string-only TTL prominently. Qualify Redis compatibility by command, options, response type, error behavior, and tested client/version.
- Clarify that memory limits estimate keyspace usage, not process RSS, and that independent connection/rewrite/parser allocations require headroom.
- Correct the broad persistence guarantee until the P0 findings are fixed, and reject/document incompatible benchmark mode combinations.
- `THIRD_PARTY_NOTICES.md` refers to a LICENSE missing from this branch and `develop`. Complete the pending provenance/licensing work and ensure both LICENSE and notices travel in archives/images before distributing a release.
- Refresh origin/licensing text after that work lands. Do not present release archives as currently downloadable while no release exists.
- Change current-write references from MEMKV.RESTORE to KEEL.RESTORE, retaining legacy names in historical examples only. Fix stale comments such as AOF rewrite being “not built yet” and LCS requiring a key directory when a registry already exists.
- Add reproducible client quickstarts, configuration defaults/limits, supported OS/architecture, migration/upgrade and recovery instructions, and a support/contribution route. With GitHub issues disabled, adopters currently lack that issue-tracking path.

## What could make Keel distinctive

The existence of probabilistic types alone is not a unique proposition: [Redis already documents Bloom, cuckoo, Count-Min and HLL](https://redis.io/docs/latest/develop/data-types/probabilistic/). Parallelism alone is also insufficient: [Dragonfly documents a multithreaded, shared-nothing architecture](https://www.dragonflydb.io/docs), while [Valkey documents cluster operation](https://valkey.io/topics/cluster-tutorial/). These are scope comparisons, not new performance measurements.

Keel's plausible distinction is the combination of simple deployment, measured memory behavior, and convenient approximate state for applications that want a small companion service. Turn that into user outcomes:

1. **Memory-conscious approximate analytics:** make an error/byte budget easy to choose, expose actual structure size and saturation, support expiry windows, and demonstrate event-frequency and distinct-count workflows. Morris is an interesting ingredient, but its 20% relative standard error is not a promise that every answer is within 20%.
2. **A compact application companion:** offer a pinned binary/image and a working Go/Python/Node example that users can integrate quickly. Show total deployment RSS and tail latency for a concrete workload, including persistence and recovery, against an appropriate baseline.
3. **Predictable behavior under a fixed budget:** correct accounting, small/large-cardinality representations where measurement justifies them, bounded requests/rewrite work, and clear eviction semantics. Sparse HLL could help when users maintain many small counters; measure that scenario before choosing the implementation.
4. **A reusable Go engine if users need embedding:** provide independent instances and typed APIs for sketches/caches. This is a separate supported product surface, with more lifecycle and compatibility obligations, so validate demand before committing to it.

Useful first demonstrations are an analytics event counter with time windows, distinct-user counting, and a cache with a membership prefilter. A filter can avoid an unnecessary backing-store lookup; its probabilistic answer should not be presented as exact idempotency or guaranteed deduplication.

Mergeable summaries could become useful for distributed analytics, but each structure needs justified merge semantics. HLL already supports union/merge. Do not assume that summing counters or combining arbitrary Morris/cuckoo state preserves the estimator's guarantees.

## Recommended milestones and acceptance criteria

| Order | Milestone | Done when |
| --- | --- | --- |
| 1 | Repair the release blockers | Allocation/restore crash inputs return errors; AOF recovers through two restarts; failed sync never earns a durable success; idle syncing works; mode/flag misuse is rejected; memory accounting survives mutation/expiry/eviction sequences. |
| 2 | Make the service dependable under load | Slow readers cannot stall others; input/output/rewrite work has explicit bounds; startup/runtime failure exits correctly; crash/error tests and parser/restore fuzz regressions run in CI. |
| 3 | Finish one integration path | Cross-type TTL and the commands needed by two selected example apps work; pinned client-library tests pass; authentication/deployment boundary is defined; diagnostics explain latency, memory and persistence problems. |
| 4 | Ship a reviewable alpha | Pending branch/licensing work is resolved, docs agree with behavior, a versioned release includes notices/checksums, install/image/restart/upgrade smoke tests pass, and users have a support route. |
| 5 | Prove one differentiated workload | A reproducible comparison shows an adoption reason in total RSS, latency at a stated load, accuracy per byte, or operational simplicity; publish limitations and have outside users reproduce it. |
| 6 | Add the scaling users actually need | Choose embedded instances, partitioned service capacity, or replicated availability from observed constraints. Define failure and multi-key semantics before implementation. |

The existing roadmap items SCAN, MULTI, pub/sub, blocking lists, and replication should remain visible, but they should not all precede the first useful release. Select them by the application workflows the product commits to supporting. Reliability, a clear compatibility boundary, and evidence for one useful advantage are the immediate engineering work.
