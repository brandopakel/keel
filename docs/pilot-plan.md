# Pilot and release evidence

Use the cache/analytics example as a starting point with two or three outside projects.
Recruitment and sending messages require the owner's chosen contacts; no outreach has been performed.

For each pilot, record:

1. Application and why its current cache/analytics implementation is insufficient.
2. Redis client/version and all initialization and application commands used.
3. Working-set size, key types/value sizes, number of connections, request mix and pipeline depth.
4. Required p50/p95/p99 latency, tolerated counting error, memory/process budget and expiry window.
5. Acceptable loss on crash and required recovery time; whether data can be rebuilt.
6. Installation time, unsupported commands encountered, operational pain, and whether they would keep using Keel.

Run Keel and the existing alternative with identical datasets, client placement, connection counts,
pipeline depth, persistence policy, memory limits and warmup. Record failed requests, timeouts,
RSS, GC pauses, CPU, eviction/expiry, and client latency during normal traffic, rewrite and restart.
Retain raw results, exact binary hashes and machine/tool versions. Never discard failed runs.

Acceptance should be agreed with each pilot before measuring. A useful candidate is a repeatable
memory reduction while meeting that application's p99 and correctness/error budget. The local
idle-connection result motivates that experiment; it is not a substitute for it.

Before public alpha promotion:

- Native Linux and macOS CI passes for the exact source revision.
- Fresh installation and upgrade/restart from a prior AOF succeed.
- Release archives contain license/notices, version information and checksums.
- Known limits and command subset match the released revision.
- A rollback copy of persistent data exists and compatibility changes are documented.

Before a stable release, require application pilots, longer adversarial/soak tests, storage-failure
coverage on deployment filesystems, and explicit support/versioning commitments.

External execution is part of the next evaluation: compare controlled separate-host
RESP benchmarks with a candidate managed service before selecting a provider.
Keep workload scripts, raw results and correctness assertions portable. See the
[external testing decision](async-scaling.md#external-testing-and-benchmarking-decision).
