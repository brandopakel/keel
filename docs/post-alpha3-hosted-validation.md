# Hosted validation after alpha.3

This record extends the [alpha.3 release closeout](alpha3-closeout.md). It does not
change the published release or expand the alpha durability/replication contract.
Application pilots still require an explicitly selected workload and acceptance
criteria; an existing integration alone does not establish a pilot result.

## Bencher execution

The public [Keel Bencher project](https://bencher.dev/perf/keel) now executes the
paired harness on the provider's `intel-v1` Firecracker runner. This is separate
from the earlier reports that imported GitHub measurements. The account uses the
Free plan: jobs are submitted sequentially with a 240-second timeout, within its
one-concurrent-job and five-minute limits. No paid runner or cloud deployment was
provisioned. [Plan limits](https://bencher.dev/pricing/).

The [image build and offline smoke](https://github.com/brandopakel/keel/actions/runs/33995869890)
passed at harness revision `ddedb29c68259b9c2e8891d3190c52708f4eeb13`. The image is:

```text
registry.bencher.dev/keel@sha256:b5b9d94d26431a4469e92d98a53d0d7210b99c975f82ad4e2b0314c2fd0ae71d
```

Both release source revisions were rebuilt with Go 1.27.1, `CGO_ENABLED=0` and
`-trimpath` in clean worktrees. These benchmark executables are identified by
their own checksums; they are not the downloadable release archives.

| Arm | Source revision | Binary SHA-256 |
| --- | --- | --- |
| Alpha.2 sync | `3596d92d192dd1f7bfceba77303212b514c84b7d` | `c8354ae52c903753175a790e01cfe8498b08210d6333d5a657ce5498ff7f69e3` |
| Alpha.3 sync and worker | `a9de9156aeb4d3ef18c06cff8784350f05867045` | `da3ee08123df44f5208696572964c6851882ec98feef99dd405da1d78b6ba383` |

Each job contains one matched triplet: alpha.2 synchronous appends, alpha.3
synchronous appends and alpha.3 worker appends. Five repetitions rotate/reverse
the arm order under each of `off`, `everysec` and `always`. Each arm uses a fresh
authenticated process, five seconds of closed-loop mixed cache/list traffic and
a separately scheduled 100 Hz PING probe. Persistence-enabled arms also perform
a rewrite. The `off` policy disables AOF and the worker in every arm; its
worker-labelled arm is an identical-configuration repeat control.

All arms retain raw requests, errors/drops, 0.5-second RSS/CPU samples, GC trace
logs, source/build metadata and filesystem details. CPU samples are lifetime
averages, and GC summaries include setup/shutdown. Server and Python generator
share one four-vCPU guest over loopback; this does not establish separate-host
capacity, production latency or application memory savings. Five-second arms
are a short comparison, not a sustained-load test.

All **15 jobs / 45 arms passed**, with **1,416,217 attempts, zero request errors
and zero dropped probes**. The 630 BMF measurements were published to Bencher.
Every load/probe summary and exported metric was recomputed from the raw CSV,
and all binary hashes, clean source revisions, arm orders and telemetry were
checked. The [full summary](../bench/results/alpha3-bencher-hosted-summary.json)
retains all 90 role measurements, raw-file/archive hashes and report/job IDs.

All jobs used runner `997069de-45de-45c7-bd9c-21045e5607f0`. Guest evidence records
Linux 6.1.177, Python 3.12.14, ext4, four vCPUs and an Intel Xeon CPU model.
The provider Spec assigns 48 GiB memory and 128 GiB disk. These are guest/Spec
observations, not a claim about deployment storage's physical durability.
[Spec contract](https://bencher.dev/docs/explanation/testbeds/).

Median load throughput across five repetitions:

| AOF policy | Alpha.2 sync (ops/s) | Alpha.3 sync (ops/s) | Alpha.3 worker (ops/s) |
| --- | ---: | ---: | ---: |
| off | 6,880 | 6,824 | 6,820 |
| everysec | 6,720 | 6,700 | 6,460 |
| always | 5,100 | 5,140 | 4,900 |

The median within-repetition worker/sync throughput ratios are **0.958** for
`everysec` and **0.962** for `always`; all five ratios in each policy are below
1. These results establish no worker-append speedup. Probe tails vary even
between the identical AOF-disabled controls. Worker appends remain experimental
and disabled by default. The short guest comparisons do not justify broader
latency, memory-saving or production-capacity claims.

The first image build failed its offline smoke because the copied Go metadata
reader needed an explicit `GOROOT`; the corrected image passed. The first hosted
job, `01a073aa-2aa0-73f1-95c7-bc24acee2dbf`, then failed before collecting successful
requests because the offline guest's loopback interface was down. The harness
now explicitly enables only `lo` and records its before/after flags. Subsequent
guest evidence confirms that transition. The failed job remains in the evidence
and is excluded from performance summaries.

One successful `always` job lost its local CLI polling connection. Its processed
report and complete raw archive were recovered through the authenticated API;
the job was not resubmitted. Transport failure was checked against provider state
before continuing the series.

Raw archives are retained locally and in each job's authenticated API output.
The [integration instructions](../bench/external/README.md#free-hosted-comparisons)
explain how to verify and extract them. The decoder rejects checksum/length
mismatches, traversal paths, links, special files, duplicate envelopes and
oversized member lists before extraction. Credentials remain in the local secret
store and temporary registry configuration; none is stored in GitHub or the image.

The code at `c911739` passed the [Linux/macOS Go 1.22 and stable matrix, race
detector and Docker checks](https://github.com/brandopakel/keel/actions/runs/33997105395),
plus [RESP framing, seven Python tests, authenticated k6/Locust smokes and AWS
package validation](https://github.com/brandopakel/keel/actions/runs/33997105380).
A local CLI check also confirmed that duplicate policies are rejected before
creating output files. That guard prevents an accidental repeated policy from
overwriting its earlier raw CSV. The benchmark image remains identified by the
earlier, recorded harness revision above.

## Longer local recovery testing

A one-hour APFS soak of the published macOS ARM64 alpha.3 executable **passed**
over 3,601.0 seconds. It verified **463,751 acknowledged writes**, **19 primary
crash recoveries**, **38 replica crash recoveries**, **115 rewrite checkpoints**,
stale-read rejection, expiry, canonical readback and a fenced manual promotion.
The promoted instance also passed restart/readback. Both synchronous and worker
append modes passed `RLIMIT_FSIZE` write-failure checks and preserved prior
acknowledgements through two restarts.

The [operational evidence](post-alpha3-operational-evidence.json) retains host
details, checkpoint results, raw-file hashes and the local evidence-archive hash.
Maximum combined primary/replica RSS sampled at the 30-second checkpoints was
53,920 KiB; this is not a continuous peak-memory measurement. The main soak uses
`always` fsync and worker appends on both instances. The file-size-limit checks
exercise write errors; they do not establish APFS disk-full or power-loss behavior.

The primary, replica and synthetic load share the local Apple M4 Pro workstation.
This supplements the completed 15-minute Linux ext4 run; it is not a
deployment-filesystem or multi-day soak. The published executable SHA-256 is
`4c65f65d1ade25cc29e670788884d3b2d0fe3e9d4f27d9c86f28516f69267088`.

## Grafana compatibility and remaining targets

The authenticated k6 adapter passed its local smoke using xk6-tcp v0.2.0, the
extension version listed for Grafana Cloud when checked on September 5, 2026.
The run completed 11 batches / 44 requests over two seconds with zero request
errors, connection errors or dropped iterations.
This verifies compatibility with the supported extension API. It is not a
Grafana Cloud execution. [Supported extensions](https://grafana.com/docs/grafana-cloud/observe-and-act/testing/k6/author-run/use-k6-extensions/).

The remaining external work needs:

- An owner-selected application workload and explicit compatibility, latency,
  memory and loss/recovery acceptance criteria for a real pilot.
- Selected deployment hosts/filesystems and separate load-generator placement
  for sustained performance, network outages and longer operational tests.
- AWS account/region/network access and a cost ceiling for AWS execution.
- Grafana account/stack access and a reachable test instance for Cloud execution.
- An owner-selected KVM host and runner credentials if self-hosted Bencher
  execution is desired in addition to the completed provider-hosted path.

Automatic failover, stronger durability guarantees, concurrent command execution
during appends, embedding and partitioning remain future engineering scope.
