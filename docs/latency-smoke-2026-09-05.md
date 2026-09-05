# Local latency smoke — September 5, 2026

Three fresh-process repetitions per policy, five seconds each, 10,000 list elements.
These sequential local runs are exploratory: the machine was not isolated and some development/test activity overlapped.
No causal performance improvement is established. Python scheduling and large-response parsing affect probe latency.

| Binary | Policy | Median load requests/s | Median probe p99 (ms) | Request errors |
| --- | --- | ---: | ---: | ---: |
| alpha2 | off | 19440 | 3.808 | 0 |
| alpha2 | everysec | 19190 | 3.952 | 0 |
| alpha2 | always | 1214 | 6.038 | 0 |
| async | off | 19002 | 3.972 | 0 |
| async | everysec | 18623 | 3.946 | 0 |
| async | always | 1142 | 6.064 | 0 |

Everysec probe p99 is essentially unchanged. Always shows variable throughput/tails; further matched runs are needed.
The worker fault test, rather than this workload, verifies that a deliberately blocked Sync does not block FlushAOF.
List mutation/replay tests verify correctness of chunking, not a universal large-key latency bound.

Raw attempts: [alpha.2](../bench/results/tail-alpha2.csv.gz), [candidate](../bench/results/tail-async.csv.gz).
Executable/environment metadata: [alpha.2](../bench/results/tail-alpha2.metadata.json), [candidate](../bench/results/tail-async.csv.metadata.json).

Candidate results correspond to the working-tree executable hash in metadata, not to a published release.
The expanded Linux workflow has been edited locally but has not been executed for this revision.
