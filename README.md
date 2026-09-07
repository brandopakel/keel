# Keel

Keel is a Go in-memory service for caching and approximate analytics. It combines
strings, hashes, lists, sets, sorted sets, geospatial queries, and probabilistic
structures under one estimated memory budget, with expiry and optional persistence.

**Status: alpha.** See [Releases](https://github.com/brandopakel/keel/releases) for
published builds. Documentation on a development branch can include unreleased changes.
Keel implements a documented RESP2 command subset; Redis protocol support does
not imply that every Redis client feature or application works unchanged.

## Run locally

Requires Go 1.22 or newer; Linux and macOS are supported. Use current Go on
recent macOS: older internal linkers omit the LC_UUID load command required by
macOS 26 ([Go issue](https://github.com/golang/go/issues/68678)). CI tests the
Go 1.22 source floor on macOS using external linking; release builds use current Go.

```sh
go build -o keel ./cmd/keel
./keel -maxmemory 256mb
redis-cli -p 8081 SET greeting hello NX EX 60
redis-cli -p 8081 GET greeting
```

The default bind is `127.0.0.1:8081`. The production event loop uses epoll on
Linux and kqueue on macOS; its historical flag name is `-mode kqueue` on both.
Command execution is serial. `-io-threads 4` parallelizes socket I/O and parsing.
Other `-mode` variants are benchmark implementations; AOF is rejected on them.

For password authentication, pass the name of an environment variable containing
the secret, using `-requirepass-env KEEL_PASSWORD`. An empty or missing variable
is a startup error. Clients authenticate using `AUTH password` or
`AUTH default password`. Use a secret manager or an interactive shell read to
populate the variable; avoid putting secrets in process arguments or shell history.

## Integration contract

| Area | Supported behavior |
| --- | --- |
| Strings | `GET`, `SET`, `SETEX`, `PSETEX`, `MGET`, `MSET`, `INCR`, `INCRBY`, `DECR`, `DECRBY`, `LCS` |
| SET options | `NX`, `XX`, `GET`, `KEEPTTL`, `EX`, `PX`, `EXAT`, `PXAT`; conditional failures return null, or the old value with `GET` |
| Expiry, every type | `TTL`, `PTTL`, `EXPIRE`, `PEXPIRE`, `EXPIREAT`, `PEXPIREAT`, `PERSIST`; expiry setters accept `NX`, `XX`, `GT`, `LT` |
| Keys | `DEL`, `EXISTS`, `TYPE`, `KEYS`, `DBSIZE`, `FLUSHDB` |
| Hashes | `HSET`, `HSETNX`, `HGET`, `HMGET`, `HDEL`, `HEXISTS`, `HLEN`, `HKEYS`, `HVALS`, `HGETALL`, `HINCRBY` |
| Lists | `LPUSH`, `RPUSH`, `LPOP`, `RPOP`, `LLEN`, `LINDEX`, `LSET`, `LRANGE`, `LTRIM`; pops accept an optional count |
| Sets | `SADD`, `SREM`, `SCARD`, `SMEMBERS`, `SISMEMBER`, `SMISMEMBER`, `SPOP`, `SRANDMEMBER` |
| Sorted sets | `ZADD` with `NX`/`XX`/`CH`, `ZRANK`, `ZREM`, `ZSCORE`, `ZCARD`; `ZRANGE` rank ranges with `REV`/`WITHSCORES` |
| Geo | `GEOADD`, `GEODIST`, `GEOHASH`, `GEOSEARCH`, `GEOPOS` |
| Approximate analytics | Bloom `BF.*`, Count-Min `CMS.*`, Morris `MORRIS.*`, Cuckoo `CF.*`, and `PFADD`/`PFCOUNT`/`PFMERGE`; see the [command registry](internal/core/eval.go) for exact names |
| Operations | `PING`, `AUTH`, `INFO`, `MEMORY USAGE key`, `MEMORY STATS`, `BGREWRITEAOF`, `KEEL.DUMP`, `KEEL.RESTORE` |

Important boundaries:

- One database, RESP2, IPv4. No transactions, Lua, Pub/Sub, blocking list commands,
  ACL roles, native TLS, cluster routing, or supported embedding API.
  Opt-in [replication](docs/replication-alpha.md) is experimental and bounded.
  Clients must avoid RESP3 negotiation and unsupported initialization commands.
- `SET` and `MSET` refuse keys of another type with `WRONGTYPE`; unlike Redis SET,
  they do not overwrite collections. Delete explicitly when changing type.
- `ZRANGE` does not support `BYSCORE`, `BYLEX`, or `LIMIT`. ZADD `GT`/`LT`/`INCR`
  are not implemented. Options outside the documented subset return errors.
- Expiry belongs to the key, including hashes, lists, filters, and sketches.
  In-place mutations preserve it; replacement clears it unless explicitly kept.
  Past deadlines delete immediately or make a key inaccessible on its next lookup.
  Active expiry samples across types; it provides eventual reclamation, not a
  fixed deletion deadline. `TTL`/`PTTL` return `-2` for missing and `-1` for persistent keys.
- `MEMORY USAGE` includes every type, including hashes, lists, and TTL metadata.
  This estimates keyspace storage, **not process RSS**. Socket buffers, AOF and
  rewrite buffers, allocator overhead, and transient command allocations need headroom.
- `KEYS`, whole-collection reads, and expensive computations execute on the command
  thread. Use bounded ranges and lower `-lcs-max-cells` for latency-sensitive workloads.
- Pipeline requests are supported; pipelines are not transactions. If a connection
  closes after a write, its outcome may be unknown. Retrying increments can duplicate effects.

[Python cache and analytics example](examples/cache_analytics.py) uses explicit
RESP2 and a nontransactional pipeline. The command behavior follows the supported
portions of Redis's [SET](https://redis.io/docs/latest/commands/set/) and
[EXPIRE](https://redis.io/docs/latest/commands/expire/) contracts.

## Persistence and deployment

```sh
./keel -maxmemory 256mb -appendonly -appendfilename ./keel-master.aof \
  -appendfsync everysec -maxclients 1000
```

- `always`: append and sync before successful replies. A write or sync failure stops
  the server without sending staged success replies.
- `everysec`: append before replies; sync in a background worker on the clock, including idle periods.
  At most one sync runs at a time. A reported background failure stops the server at the next loop check.
  A crash can lose recent writes. Slow storage can stretch the nominal one-second window.
- `no`: append before replies; let the OS schedule durability, with a sync on clean shutdown.

Startup streams the AOF. A torn final command is copied to a `.keel-torn-tail-*`
file beside the log, then the log is truncated to the last complete command before
new appends. Other malformed records or failed replay commands prevent startup.
Back up the AOF before upgrading. Dumps use a Keel-specific version and checksum;
legacy unversioned dumps remain readable, but neither format is Redis RDB or includes TTL.
The old `MEMKV.*` command aliases and `memkv-master.aof` migration path remain supported.

Nonblocking replies have a 64 MiB per-client pending-output limit. Incomplete input
is limited to 16 MiB per client. A client holding incomplete input or pending output
without progress for 30 seconds is closed (checked approximately once a second).
`-maxclients` bounds connected clients. Retained user-space input/output buffers
also share a 256 MiB limit; excess clients are closed. These are resource limits, not an RSS guarantee;
a reply can allocate before the output limit is checked.

Rewrites advance in slices of at most 2048 keys, targeting 1 MiB or 1 ms between
keys. Large lists additionally yield every 256 elements or about 64 KiB; one large
element can exceed that byte target. Mutations restart the list copy. Dirty keys are also processed in slices. A rewrite is abandoned if it exceeds
30 seconds or 100,000 dirty keys; the original log remains authoritative. Snapshot
creation refuses more than one million keys. Snapshot enumeration, individual large
keys, disk writes, and final sync remain synchronous. There is no hard rewrite latency SLA.

Keep Keel on a private network. AUTH does not encrypt traffic and grants access to
all commands, including destructive ones. Use a TLS proxy for untrusted network hops,
restrict access, mount a persistent writable data directory, and supervise restarts.
SIGINT/SIGTERM requests shutdown with a five-second backstop; forced termination is
crash recovery, not a clean-shutdown durability guarantee.

For containers, build `docker build -t keel:local .`, then:

```sh
docker run --rm -p 127.0.0.1:8081:8081 -v keel-data:/data \
  keel:local -host 0.0.0.0 -appendonly -maxmemory 256mb
```

Container arguments replace the image's defaults, so retain `-host 0.0.0.0` when
passing custom arguments. The image runs as UID 65534; bind-mounted directories
must be writable by that UID. Leave memory above `-maxmemory` for process overhead.
Use `INFO` and `MEMORY STATS` to inspect keyspace memory, expiry, eviction, and AOF
state; also monitor process RSS, storage space, restart counts, and client latency.

## Why use it?

The useful hypothesis is a compact cache plus approximate analytics: cache a result,
expire a time-window sketch, and measure both under the same memory budget.
Bloom and Cuckoo filters trade memory for false positives; Count-Min sketches can
overestimate; Morris adds probabilistic counter error. These are unsuitable for exact
billing or authorization. Cuckoo deletion requires knowledge that an item was inserted.

The product advantage still needs measurement on real application traces and feedback
from outside users. Historical throughput and memory results in [bench/](bench/README.md)
are not release guarantees. The repaired memory harness measures live connections and
verifies process identity. Run it against the exact binary you plan to deploy.

## Development and roadmap

```sh
go test ./...
go test -race ./...
go vet ./...
```

See the [engineering review](docs/engineering-review-2026-09-04.md),
[delivery checklist](docs/engineering-delivery.md), and
[historical design narrative](docs/architecture-history.md). History records prior
measurements and behavior; this README defines the current integration contract.
The [general-purpose validation program](docs/general-purpose-validation.md)
provides native workload comparisons, live heap/CPU profiles, seeded Redis
differential tests and extended operational checks. Application pilots are one
part of this coverage; Keel is the optimization target.
Embedding, partitioning, and replication are separate future decisions driven by
application requirements, not features implied by I/O threads.

### What remains to build out

In order of distance, not size.

- **Validating the experiments.** Asynchronous appends and bounded primary/read-only
  replication are included in alpha.3 as opt-in experiments with their own contracts and
  limits. Command execution concurrent with disk appends, automatic failover, and
  fencing are not started.
- **Command surface outside the contract.** Transactions, Lua, Pub/Sub, blocking list
  commands, `SCAN`, RESP3, ACL roles, and cluster routing are absent. `ZRANGE` lacks
  `BYSCORE`, `BYLEX`, and `LIMIT`; `ZADD` lacks `GT`, `LT`, and `INCR`; `ZCOUNT` and
  `ZINCRBY` do not exist. The skip list already has the range walks these need.
  `LREM`, `LINSERT`, `GEOSEARCHSTORE`, the `GEORADIUS` family, `CMS.INFO`, `CMS.MERGE`,
  and `BF.CARD` are also missing.
- **Persistence without a latency bound.** The `everysec` window stretches on slow storage.
  A rewrite abandons itself past thirty seconds or 100,000 dirty keys, and snapshot
  enumeration, individual large keys, disk writes, and the final sync still run on the
  command thread. Dumps carry no TTL and are not Redis RDB.
- **Scaling, deliberately deferred.** Embedding, partitioning, and replication for failure
  recovery each wait on a pilot that shows which constraint is real; the
  [delivery checklist](docs/engineering-delivery.md#5-demand-led-scaling-decision) lists
  the evidence each needs first.
- **Evidence before a release past alpha.** Separate-host deployment benchmarks and
  an application pilot with stated RPO, RTO, and memory targets remain outstanding.
  Alpha.3 adds four native installation targets, upgrade/rollback checks, matched
  shared-runner benchmarks and bounded crash/disk-full tests; it remains a
  pre-release. The [closeout record](docs/alpha3-closeout.md) reports those results,
  and [hosted validation](docs/post-alpha3-hosted-validation.md) adds Bencher execution. The
  [delivery checklist](docs/engineering-delivery.md#4-alpha-preparation-and-evidence)
  says what counts.
- **Boundaries that stay.** No native TLS (use a proxy), `SET` refuses to overwrite a
  collection where Redis would, and `MEMORY USAGE` measures the keyspace, not RSS.

Keel was formerly named memkv and grew from [quangh33/memkv](https://github.com/quangh33/memkv).
The original lineage is retained in Git history. Project code is under the [MIT license](LICENSE);
Redis-derived code retains its BSD terms in [third-party notices](THIRD_PARTY_NOTICES.md).

### Latency and scaling work

Run `python3 bench/run-tail.py --bin ./keel --out bench/results/tail.csv.gz`
for repeated mixed cache traffic, TTL writes, large-list reads, rewrites, and an
independent scheduled PING probe under all three persistence settings. Raw attempts,
errors and binary metadata are retained. This is a local diagnostic workload, not
an open-loop saturation test or proof of application performance.

Background `everysec` syncing improves concurrency, but append writes, rewrite
writes/finalization, whole-key replies and other large-key serialization can still
stall the event loop. `INFO persistence` exposes `aof_pending_fsync`.
Optional worker appends (`-aof-async-append`) and bounded primary/read-only replication
are opt-in experiments introduced after v0.1.0-alpha.2; see [contracts and limits](docs/replication-alpha.md).
Automatic failover, partitioning and a public embedding API remain unimplemented.
See the [next-stage engineering contract](docs/async-scaling.md) for
acceptance criteria and the [pilot plan](docs/pilot-plan.md) for application evidence.

Runnable [Bencher, k6 and AWS DLT adapters](bench/external/README.md) are available for external testing.
