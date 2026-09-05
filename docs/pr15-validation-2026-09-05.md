# PR #15 candidate validation — September 5, 2026

This record resumes the alpha.3 candidate from
[PR #15](https://github.com/brandopakel/keel/pull/15). The candidate remains on its
feature branch; merge and release publication are separate steps.

## Linux race failure and corrections

[The failing Go run](https://github.com/brandopakel/keel/actions/runs/33955219871)
tested feature revision `a676912cdb46d97612fcd78b54596b6d3fb2144e`. Its race job
reported no data race: `TestAutomaticRewriteTriggersOnGrowth` observed a 68,884-byte
AOF against a 65,536-byte threshold. Other native Go jobs and adapters passed.

The test assumed all automatic compaction had finished after its last write.
Background everysec fsync can still own the old descriptor, delaying replacement.
A controlled blocked-sync reproduction also showed that repeated hot-key updates
were serialized on every rewrite turn while replacement waited, inflating the
temporary log to 207,792 bytes for a 126,890-byte original log.

The rewrite now retains changed key names after the snapshot walk while sync owns
the descriptor, and reconciles them once sync completes. Duration and dirty-key
budgets still apply. The test exercises both ordinary and deliberately blocked
sync, verifies bounded temporary-log growth, drains pending work before checking
compaction, and replays the final value. The regression runs under race detection.

Other review corrections:

- Failed readiness registration now skips accounting after closing a client.
  A socket-pair test reproduced a 20-byte retained-accounting leak for pending replies;
  it covers both read and write registration failures.
- Replication clears readiness before applying mutations and enables it only after
  a complete frame. The server already exits on apply failure; the core now also
  rejects reads and further deltas after a partial application. A malformed-arity
  delta test verifies rejection and recovery through a fresh full snapshot.
- Nonblocking worker tests use a ten-second emergency release with the existing
  500 ms return threshold. Workflow actions use the repository's existing immutable
  pins. The README describes experimental replication, and its deployment example
  connects through a verified TLS proxy.

Review items retained with their existing contracts:

- A batch above 64 MiB deliberately stops asynchronous append mode without success
  replies; it is a documented alpha admission limit, not a disk failure fallback.
- The tail CSV `error` column contains an empty string on success and error text on
  failure. Values such as `0` or `false` are nonempty error text, not boolean flags.
- TLS remains configurable for controlled/private transport and local smokes;
  remote deployment must follow the documented TLS/protected-network contract.
- The checksum input has only JSON-supported scalar and byte-slice fields; its
  marshal cannot fail for the present frame type. No checksum-format change is needed.

## Completed Linux benchmark review

[Benchmark run 33955229511](https://github.com/brandopakel/keel/actions/runs/33955229511)
completed both append configurations on revision `a676912`. Each ran three fresh
processes per durability policy for 30 seconds, with authenticated loopback traffic,
1,000 cache keys, a 10,000-element list, one closed-loop load client and an independent
100 Hz scheduled PING probe. Each persistent run issued one successful BGREWRITEAOF
request. Probe latency includes scheduling delay and client parsing overhead.
Here `off` means AOF is disabled (no `-appendonly` flag); it is not the
`-appendfsync no` policy. Neither `off` row enables worker appends.

| Append configuration | Policy | Median load requests/s | Median probe p99 (ms) | Probe p99 range (ms) |
| --- | --- | ---: | ---: | ---: |
| Synchronous | off | 6,820.0 | 2.362 | 2.287–2.927 |
| Worker | off | 6,930.0 | 2.851 | 2.291–3.466 |
| Synchronous | everysec | 6,746.9 | 2.385 | 2.287–2.617 |
| Worker | everysec | 6,460.1 | 2.236 | 2.172–3.569 |
| Synchronous | always | 4,850.1 | 2.306 | 2.133–2.439 |
| Worker | always | 3,851.9 | 3.350 | 2.933–3.910 |

All 18 runs had zero request errors and zero dropped probes. There were 3,000 or
3,001 probes per run because floating-point scheduling can include the endpoint.
The `off` configurations run the same server options: their variation is a useful
reminder that separate shared GitHub hosts are not a paired controlled experiment.
Worker `always` throughput was about 21% lower and probe p99 about 45% higher;
everysec differences are mixed. These measurements establish no speedup and support
keeping worker appends opt-in. They predate this validation's source corrections.

[Per-repetition measurements and original metadata](../bench/results/tail-linux-pr15-summary.json)
retain the binary/harness hashes and raw CSV SHA-256 hashes. The original CSV,
metadata and process logs are in the run's `linux-tail-latency-sync` and
`linux-tail-latency-async` artifacts; a local copy is retained under
`dist/validation/pr15/benchmarks/` (ignored build evidence).

## Release checks

Thirty local race repetitions passed for the rewrite, delayed-sync lifecycle,
large-list mutation, replication failure and connection-accounting regressions.
The full local Go suite, full race suite, vet, RESP framing, BMF export and AWS
package checks passed. Authenticated k6 and Locust smokes also passed against the
local packaged Darwin ARM64 candidate.

All native checks passed on corrected source revision
`8b7b7ef33cee64c98feb9b2a97c00fcdbedf0a36`:

| Gate | Evidence |
| --- | --- |
| Linux/macOS, Go 1.22/current, build/test/vet, client integration and restore fuzz smoke | [Go run 33991061470](https://github.com/brandopakel/keel/actions/runs/33991061470) |
| Linux full race suite plus 20 repetitions of the new regressions | Same Go run, race-detector job |
| Docker named-volume/bind-mount persistence and restart | Same Go run, Docker job |
| Linux authenticated k6/Locust, RESP, BMF and AWS packaging | [Adapters run 33991061481](https://github.com/brandopakel/keel/actions/runs/33991061481) |
| Native Linux/macOS current-Go full tests, race and vet, then four target archives | [Release dry run 33991071168](https://github.com/brandopakel/keel/actions/runs/33991071168) |

All four GitHub archives were downloaded and their SHA-256 sidecars, required
licenses/docs/examples/adapters, binary architectures and clean Go VCS revision
verified. Python cache files are absent. [Archive hashes and build metadata](pr15-candidate-archives.json)
identify these exact source-`8b7b7ef` artifacts. The downloaded Darwin ARM64 binary's
version invocation and authenticated k6/Locust adapter smokes passed.
Linux ARM64 and Darwin amd64 artifacts were cross-compiled and inspected, not
executed on native hardware in this session.

Archives and raw validation logs are retained in `dist/validation/pr15/`. The
release workflow's publication job was skipped because this was a branch dispatch.
Pre-publication validation is complete for this candidate; a future release tag
must build and verify its own artifacts. The evidence-only follow-up changes docs
and preserves the validated implementation.

Controlled Bencher/KVM execution, hosted dashboards, paid AWS/Grafana runs, and
real application pilots remain pending their documented host/account/target inputs.
