# Scheduled arrivals and larger recovery

`bench/arrival` is an independent Go RESP load generator. It schedules arrivals
without waiting for earlier replies and admits them to a bounded queue feeding
independent connections. It reports scheduled, issued, completed, failed,
queue-dropped and queue-expired counts; every scheduled arrival is accounted for.
Latency from the scheduled arrival includes scheduler and queue delay. Service
latency and scheduler lag are reported separately, along with generator CPU
time. Histogram buckets have at most 1/64 relative width (plus nanosecond
rounding), and the reported quantile is the bucket's upper edge.

Dropped requests are not hidden from throughput or interpreted as server
rejections. A late/saturated generator limits the experiment: inspect generator
CPU, scheduler lag and queue drops before attributing a capacity plateau to
Keel. Failed protocol exchanges fail the harness; expected overload drops remain
recorded measurements. Value correctness is covered separately by the Redis
differential suite. No per-deployment SLO is assumed by this tool.

`bench/run-capacity.py` starts a fresh server for every rate, repetition and arm,
preloads its dataset, then runs the scheduled measurement without a separate
timed warmup. Baseline/candidate order rotates. Both arms use the same AOF policy,
append mode, workload and connection counts. Cases cover small reads, balanced
1 KiB traffic, whole-second expiry cohorts, 1 MiB values, 4,096-member hashes and
lists, and three tenant mixtures. Tenants have separate generator processes,
connections and queues with a shared scheduled start, avoiding interference
caused solely by one shared generator queue. AOF-off reports append mode disabled.

The smoke run completed all seven cases. A separate four-second expiry case
verified that the expiry counter actually increased. A deliberately slow test
server verifies overload drops and accounting without slowing the arrival
schedule to match completed replies. RESP framing and histogram boundary tests
also pass. Local smoke timings run alongside existing soaks and are not
comparative performance evidence.

`scripts/check-large-recovery.py` verifies two replicas together through
interrupted initial snapshots, a 32 MiB write outage overflowing retained
history, primary rewrite, checkpoint restart without duplicate increments, and
primary crash/epoch recovery. Full dataset digests and the acknowledged counter
are checked after each phase. Catch-up observation times precede digest checks;
phase totals include verification. The local 10 MiB smoke passes all phases.
The default hosted dataset is 128 MiB, with both replicas independently verified.

The manual `capacity-validation.yml` workflow builds exact candidate/baseline
runtime commits with Go 1.27.1 and records generator/runtime provenance. It uses
standard free Linux runners and disjoint exposed physical-core groups. The
larger recovery test runs on another standard runner. These are public-VM
measurements; dedicated deployment hosts, underlying tenancy and deployment
latency guarantees remain outside their evidence.

Example local smoke (owned disposable servers only):

```sh
go build -o /tmp/keel-arrival ./bench/arrival
go build -o /tmp/keel-candidate ./cmd/keel
python3 bench/run-capacity.py --candidate /tmp/keel-candidate --load /tmp/keel-arrival --out dist/capacity-smoke --rates 1000 --seconds 1 --reps 1
python3 scripts/check-large-recovery.py --bin /tmp/keel-candidate --out dist/recovery-smoke --keys 160
```
