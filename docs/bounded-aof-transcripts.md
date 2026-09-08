# Bounded primary AOF transcripts

Status: candidate, September 7, 2026. No frozen-soak or release result applies.

An 8 MiB SET previously allocated about 8.40 MB to construct its canonical log
record. Evicting 64 keys with 128 KiB names allocated about 40.46 MB while first
collecting removal arrays and then growing the encoded log. Both retained more
than eight MiB of idle log capacity. Local regression checks reproduce those
allocations before the change.

The primary now coalesces ordinary commands in a buffer whose length and backing
capacity cannot exceed four MiB plus 64 KiB of framing headroom. Large fields borrow the command's existing
strings and drain fragments directly in order. Eviction and expiry write their
DEL records directly instead of accumulating a separate removal array. The same
local fixtures allocate 4.27 MB for the eight-MiB SET and 7.96 MB for mass
eviction, with 4.26 MB retained log capacity. Two exact restarts
preserve the resulting state. These are allocation diagnostics, not controlled
latency or process-RSS measurements.

Lazy-expiry DELs precede the command's canonical record, so an expired key can be
recreated without deleting the replacement on replay. Eviction runs after the
canonical record, so it can remove even the key that triggered the eviction.
A command scope tracks the unpublished protocol-2 prefix across buffer drains.
Previously committed commands are not published again. Opaque filter/sketch
updates publish their final exact image after eviction decisions, preserving the
existing bounded-image/snapshot-fallback contract.

Fragment drains join any older append worker before writing. They do not sync,
advance the acknowledgement position, walk or hand off a rewrite, or switch file
descriptors. The normal completed-command flush applies the durability policy.
Short/failed writes set the existing fatal persistence state; the server's flush
barrier withholds the affected replies. The torn final record remains repairable
without removing the preceding acknowledged prefix. Concurrent append admission
requires the entire admitted run to fit within the remaining transcript space;
otherwise it waits at the existing server barrier before execution.

This bounds encoded primary log construction and removes the eviction-record
array. It does **not** make large commands asynchronous or impose a filesystem
latency limit: a four-MiB drain can still block, and large commands issue more write
calls. Startup's recovered-key list, parsed input, staged destructive member
arrays, replication history/images and rewrite state have separate contracts.
The final rewrite handoff remains synchronous. This is not a complete
process-wide allocation pool or a proven append-overlap speedup.

Tests cover bounded large values and mass eviction, canonical byte equality,
expiry/recreation, two restart passes, streamed SPOP/TTL records, exact CMS image
replication over many frames, checkpoint digests, an older deliberately blocked
append, short writes/torn-tail repair, and no sync/acknowledgement/rewrite advance
mid-record. After integrating PRs 53/54 and the rewrite-ceiling change, the full
local suite, vet, and three focused transcript/ordered-append/replication/rewrite
race repetitions pass on Apple Silicon Go 1.26.6. Initial test compile and null
reply-expectation errors are retained separately from runtime results.

The I/O counters now cover direct transcript drains as well as regular flushes.
The earlier review request on slow-call test timing is addressed with a synthetic
start one minute before the cutoff; the blocked-worker assertion needs no sleep.

Hosted validation compares the exact baseline and candidate with identical fsync
and append modes. Four policies (off/no/everysec/always) include ordinary reads,
small writes, pipelined writes and one-MiB writes; enabled persistence runs in
synchronous, worker-barrier and concurrent modes. Five alternating ten-second
pairs per cell use disjoint exposed CPU groups on each public VM. Host tenancy
and physical storage remain uncontrolled. Successful disposable AOFs are hashed
with bounded reads and removed after shutdown to bound disk use between arms;
failed logs are streamed into compressed artifacts for diagnosis. Raw tool logs, telemetry,
results, checksums and host/source metadata are archived. Neither a completed job
nor a client-CPU-limited cell establishes an adoption result by itself.

The initial one-MiB ceiling passed correctness and reduced the allocation
fixtures to 1.06/1.59 MB, but a one-second local worker/concurrent probe showed
one-MiB write throughput around 150 to 95 operations/s. Each one-MiB value plus
framing crosses that ceiling and forces a direct write. This probe shares the
Mac with frozen soaks and is not a controlled performance result, but identifies
a structural boundary cost. The candidate raises the ceiling to four MiB before
hosted adoption testing. Both the one-MiB raw probe and subsequent results remain
archived; the initial probe is not silently replaced with a later successful run.

With the four-MiB ceiling, the full suite and three focused race repetitions pass
again. A second one-second local probe records one-MiB writes at roughly 150 to
128 operations/s and pipelined small writes at 125k to 144k operations/s. These
short, contended results remain diagnostic; they do not qualify either a gain or
an acceptable regression. That candidate was held until a controlled policy/mode matrix and review could
assess the memory/throughput tradeoff.

## First hosted matrix and admission correction

Run 34165127082 compares candidate `44b2a58` with baseline `ad18e15`. The
off-policy matrix and all three always-policy matrices pass. Small-command
throughput is mostly unchanged. Concurrent one-MiB writes have a median paired
throughput ratio of 0.852; synchronous pipelined writes are 0.966. Off-policy
one-MiB writes are 0.952, despite the transcript path being disabled. No completed
cell reports a generator CPU warning, but public-runner variability still applies.
These results do not support adopting the original candidate as performance-neutral.

The no/everysec matrices fail on the baseline's first one-MiB write arm in all
three append modes. Requests complete without reported command errors, followed
by `shutdown exceeded five seconds` on SIGTERM. These six failures remain failed;
they do not establish a candidate regression or an acceptable shutdown. A separate
hosted diagnostic captures the blocked shutdown phase before changing its policy.
Compressed original artifacts are in `transcript-first-hosted-2026-09-07.tar.gz`.

Inspection found that append admission reserved three copies of each SET value,
although the canonical log contains it once; only the key can also appear in lazy
DEL and expiry records. The corrected bound covers the value once, the key three
times and generous framing/expiry slack for SET/SETEX/PSETEX. Other command bounds
remain conservative. A small allowance above four MiB accommodates four one-MiB
values plus RESP framing. The final geometric buffer growth includes this
allowance, avoiding an extra full backing allocation just for the headers.

A deterministic check holds an older append worker, admits and executes four
one-MiB expiring SETs without joining that worker, verifies each actual transcript
fits its reservation and all replies remain gated, then performs two exact
replays. Existing random-run admission, expiry, replication and torn-tail checks
also pass. The bounded local check takes approximately 5.5 seconds and prunes its
compilation cache. The initially reproduced extra-growth allocation failure and
the corrected result are both archived. This correction requires a fresh matched
hosted matrix; no throughput gain or neutral result is inferred from the unit test.

## Corrected off/always comparison

[Run 34169298573](https://github.com/brandopakel/keel/actions/runs/34169298573)
compares corrected `3bced0b` against `f1cbab2`, with five alternating pairs per
case/mode, ten seconds of load, matched durability and disjoint exposed CPU groups.
All 160 arms pass with no generator CPU warnings. This repeat uses a newer baseline
than the first matrix, so it is a fresh candidate assessment rather than an isolated
measurement of the framing fix. The original failures and slower cells remain above.

| Policy / mode | 64B read ratio | 64B write ratio | Pipeline write ratio | 1 MiB write ratio |
| --- | ---: | ---: | ---: | ---: |
| off/sync | 0.994 | 0.998 | 0.998 | 0.993 |
| always/sync | 0.999 | 1.006 | 0.994 | 1.003 |
| always/barrier | 0.994 | 1.014 | 1.043 | 1.008 |
| always/concurrent | 0.998 | 1.013 | 1.033 | 1.005 |

Ratios are medians of paired candidate/baseline throughput. Most cells remain near
parity; this does not establish a general async append speedup. Always/concurrent
1 MiB write p99 is 45.823 → 46.079 ms; always/barrier 1 MiB p99 is 33.791 → 34.559 ms.
Always/barrier and concurrent pipeline throughput ratios are 1.043 and 1.033 in this
run. Shared public VM scheduling and physical storage are uncontrolled. Full compact
artifacts are `bench/results/transcript-corrected-off-always-2026-09-07.tar.gz`.

The remaining no/everysec comparison must use the same explicit supported shutdown
grace on both binaries, after integrating the shutdown fix. The harness records the
grace and preserves failures; it does not silently alter an older baseline binary.

## Completed four-policy comparison

[Run 34171209014](https://github.com/brandopakel/keel/actions/runs/34171209014)
completes **400 passing arms**: 40 case/policy/mode cells, five alternating
baseline/candidate pairs each. Candidate `b823ba52baf848b2b5052d71e986a18546c1e71d`
and control `a1d550c1e3e73d2633e4097e45d1816cfe8e9e8c` share the merged reservation
and shutdown runtime changes. Both receive an explicit 30-second shutdown grace.
All completed cells have zero generator CPU warnings. Reports, tool logs,
telemetry, host metadata and source identities are preserved in
`bench/results/transcript-final-policy-matrix-2026-09-07.tar.gz` (about 7 MiB).
The 563.65 GiB of cumulative disposable AOF output was hashed/reported and removed
between arms on the hosted runners; it was never copied to the laptop.

| Policy / mode | 64B read ratio | 64B write ratio | Pipeline write ratio | 1 MiB write ratio |
| --- | ---: | ---: | ---: | ---: |
| off/sync | 1.010 | 0.996 | 1.002 | 1.008 |
| no/sync | 1.010 | 0.995 | 0.970 | 0.989 |
| no/barrier | 0.995 | 1.002 | 1.013 | 0.999 |
| no/concurrent | 1.002 | 1.004 | 1.038 | 0.997 |
| everysec/sync | 1.002 | 0.996 | 0.965 | 0.992 |
| everysec/barrier | 0.992 | 1.000 | 1.022 | 1.012 |
| everysec/concurrent | 0.999 | 0.997 | 1.039 | 1.000 |
| always/sync | 1.005 | 1.007 | 0.987 | 1.005 |
| always/barrier | 0.996 | 1.012 | 1.026 | 1.007 |
| always/concurrent | 1.008 | 0.992 | 0.996 | 1.005 |

This supports a bounded-memory change with a measured throughput tradeoff; it
does **not** establish performance neutrality or a general append-overlap speedup.
The synchronous pipeline is about 3.0% slower with no periodic fsync and 3.5%
slower with every-second fsync. Its every-second p99 moves from 1.967 to 2.039 ms.
Always/synchronous pipeline p99 moves from 2.991 to 3.343 ms (about 11.8% higher),
while its throughput is 1.3% lower. Every-second/synchronous 1 MiB write p99 rises
from 39.167 to 42.239 ms. Other cells and append modes differ; per-cell evidence
must accompany any performance claim. The shared public VMs do not establish
dedicated deployment capacity or tails under every workload.

The allocation fixtures reduce 8 MiB SET allocations by about 49% and long-key
mass-eviction allocations by about 80%, with a bounded retained transcript. The
remaining optimization is the ordinary synchronous serialization path: profile
its CPU and allocation costs on matched hosted runs before selecting a change.
Review remains required before adoption; no new release is implied by this data.


## Integrated Intel rewrite diagnostic failure

The integrated runtime `d547a3ce4840201aae9c0bbfd104602888629f91` failed
[its Intel native job](https://github.com/brandopakel/keel/actions/runs/34173728823/job/101898906222)
in `TestRewriteStallProfile`: one worker wait reached 3.000139 seconds and hit
its three-second diagnostic gate. The next moving-keyspace test then reported
an old keyspace-registry traversal. The profile test lacked cleanup on its fatal
exit, leaving the prior rewrite active; that source path can contaminate the next
test. It does not identify why the first worker wait was slow.

The profile fixture now always closes/cancels its AOF during test cleanup and
records whether its longest wait was a replacement write, replacement sync or
original-file sync. The three-second gate remains unchanged. A hosted Intel
workflow runs five alternating control/candidate pairs with this identical
fixture and preserves every failure. Its local short check passed in 5.47 seconds
and pruned both Go cache and intermediate files; the million-key diagnostic is
skipped locally. The original job log and short-check report are in
`bench/results/transcript-intel-rewrite-wait-2026-09-07.json.gz`.

These repetitions are diagnostic evidence. A successful repeat cannot explain
the earlier wait or establish a fixed filesystem latency bound. The complete
400-arm workload matrix remains separate evidence for its recorded runtime.
