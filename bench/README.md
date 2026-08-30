# MemKV benchmarks

Working notes for the upstream discussion in
[quangh33/memkv#2](https://github.com/quangh33/memkv/issues/2) — whether the
hand-rolled `epoll`/`kqueue` event loop should be replaced with Go's `net`
package. This directory lives only on the work branches and is deliberately kept
out of the upstream fix PR.

The posted conclusion is in the issue comment. This file records how the numbers
were produced and, more usefully, the ways the harness was wrong before it was
right.

## The servers

One binary, selected with `-mode`:

| mode | implementation |
|---|---|
| `kqueue` | the event loop, replies from one read coalesced into one write — **the default** |
| `kqueue-nobuf` | the event loop, one write syscall per reply (upstream's design) |
| `net` | `net.Listener`, goroutine per connection, 4KB `bufio` both ways, mutex |
| `net-small` | as `net`, 512-byte buffers — how much per-connection memory is tunable |
| `net-direct` | as `net`, without `bufio.Reader` (the read path already buffers) |
| `net-chan` | goroutine per connection for I/O, one executor goroutine for commands |
| `net-nolock` | diagnostic only. PING-safe, races on anything touching a store |

`redis-server` is measured alongside as a reference column, not a competitor.

On Linux the `kqueue` modes run on `epoll` — the name is historical, the
`io_multiplexing` package resolves it by build tag.

**`-mode` defaults to `kqueue`.** Coalescing is a ceiling removed rather than a
trade-off, so the fork serves buffered unless told otherwise, and `-mode
kqueue-nobuf` is how you ask for one write per reply. Every script here passes
`-mode` explicitly, so none of them depend on the default.

### readChunkSize changed after these runs

Everything under `results/` was measured with `readChunkSize = 4096`. It is now
65536, which changes the large-value numbers substantially - measured on darwin,
SET at d=262144 went from 8.5k to 14.4k ops/second on that change alone, and
11.5x against the tree before any of the large-value work.

It moves **both** arms of the comparison: the event loop reads it directly, and
`bufSizeFor` in `server_net.go` hands it to the net variants as their `bufio`
size, so `net-small` is still the only variant deliberately reading small. The
event-loop-versus-netpoller ratio is therefore still measured like for like -
but absolute large-value figures in `results/` are now historical, and the
conclusions drawn from the size sweep specifically would need a re-run to be
restated. The concurrency, pipeline and command-type sweeps are unaffected in
kind, since they run at d=3.

### The 2026-08-27 relabelling

The mode names were swapped when the default changed, and **everything under
`results/` was rewritten in the same commit** so that a label means one thing
across the whole directory:

| meaning | name before | name now |
|---|---|---|
| event loop, one write per reply (A1, upstream's design) | `kqueue` | `kqueue-nobuf` |
| event loop, replies coalesced per read (A2, the default) | `kqueue-wbuf` | `kqueue` |

Nothing was re-measured. The CSV `server` column and the `hyperfine*/` filenames
were renamed in place, row for row and file for file, and the numbers attached
to each arm are the ones that arm originally produced.

Two consequences worth knowing. Reading a `results/` file at a commit **before**
that migration gives the old meaning, so `git show` output and the working tree
disagree about what `kqueue` denotes — the migration commit is the boundary. And
the [issue #2](https://github.com/quangh33/memkv/issues/2) comment was written
under the old names; its `kqueue` is this directory's `kqueue-nobuf`. The A1/A2
identities in `summarise.py` did not move and are the stable way to refer to the
two arms across both.

**`kqueue` vs `net*` is the comparison the issue asks for.** Same framing,
same write policy, same execution semantics, all with `TCP_NODELAY`. The `net`
variants serialise execution because `core/storage.go` holds unsynchronised
package-level maps; sharding them would be faster and would measure a different
program.

## Scripts

| script | what it does |
|---|---|
| `run-matrix.sh` | throughput: concurrency, pipeline depth, value size, command type |
| `run-memtier.sh` | randomised keys and payload sizes, latency percentiles |
| `run-hyperfine.sh` | per-configuration noise floors |
| `run-iothreads.sh` | throughput against `-io-threads`: concurrency, value size, pipeline depth |
| `run-memory.sh` | RSS against connection count |
| `run-offloopback.sh` | the same, across a veth pair between network namespaces (Linux) |
| `summarise.py` | reduces a matrix CSV to medians and comparison tables |
| `summarise-iothreads.py` | the same for an `-io-threads` sweep, with ratios against 1 thread |
| `run-ab.sh` | two binaries over the same suites, arms alternating rep by rep |
| `summarise-ab.py` | paired per-rep ratios from an A/B run |

`SERVERS=`, `BIN=`, `OUT=`, `REPS=`, `PORT_BASE=` are all overridable.

## I/O threads

`run-iothreads.sh` sweeps `-io-threads`, which moves the read syscall, RESP
parsing and the write syscall onto more than one thread while command execution
stays on the loop's own. Every arm is the same `-mode kqueue` server; only the
thread count differs, and `-io-threads 1` is the configuration every other
result in this directory was measured under. `summarise-iothreads.py` prints the
full table from `results/iothreads-darwin.csv`; the cells worth drawing
conclusions from are these.

darwin/arm64, medians of five runs, against `-io-threads 1`. Only rows whose
spread stayed inside the noise floor at every thread count are listed, which is
why the small-payload concurrency rows are not here:

| workload | 2 | 4 | 8 |
|---|---|---|---|
| `SET` d=262144 | 1.32x | **1.41x** | 1.40x |
| `SET` d=65536 | 1.08x | 1.04x | 1.06x |
| `SET` d=8192 | 0.90x | **0.89x** | 0.91x |
| `GET` d=8192 | 0.90x | **0.88x** | 0.88x |
| `GET` d=65536 | 0.96x | 0.94x | 0.97x |
| `GET` d=262144 | 0.98x | 0.96x | 0.97x |
| `PING` c=1 | 0.96x | 0.99x | 1.01x |
| `PING` c=10 | 0.95x | 1.10x | 1.12x |

Three things are solid. The large-value `SET` path gains **1.41x** and saturates
at four threads. A band around 8KB *loses* 10–12%, consistently and for both
`GET` and `SET`. And a single connection is unchanged, which is the threshold
working: one ready connection is never worth distributing, so it never is.

Everything with small payloads at c=50 and above measured 1.05x–1.25x, but with
spreads of 15–32% around it. That is above the noise floor, so those cells are
recorded and not relied on. If the gain there is real it is the interesting one,
and it wants a quieter machine than this to establish.

`GET` at large sizes not moving while `SET` does is the asymmetry worth naming:
at d=262144 the one-thread server is already pushing 8.1 GB/s to a client on the
same box, against 4.2 GB/s for `SET`. That is the cell most likely to be
measuring `redis-benchmark` rather than memkv.

### The barrier is not what costs, and Redis's answer is worse here

The 8KB regression looked like barrier overhead, so the barrier was measured on
its own. A channel-and-WaitGroup rendezvous costs **688ns** for three workers and
2.5µs for seven — at these rates a fraction of a percent, nowhere near 11%.

Worth recording is what the alternative measured. Redis does not use a channel
here: its I/O threads busy-wait on an atomic, because in C a spin beats parking
a thread. The same spin in Go measured **11.4µs**, sixteen times worse than the
channel, because goroutines are multiplexed onto threads rather than pinned to
cores, so one that spins fights the scheduler instead of sidestepping it.
Translating that part of Redis's design faithfully would have made this slower.

What is left as the likely cost at 8KB is moving each connection's bytes between
cores for a phase that is over in a microsecond — 50 connections times 8KB is
past what a core keeps close, while at 256KB the copying is large enough to be
worth splitting anyway. That is a reading the numbers are consistent with rather
than one they establish.

**These are loopback numbers and the client shares the machine.** There are 12
cores between `redis-benchmark` and the server, so an arm that takes eight
threads is not measured against an idle client. `run-offloopback.sh` exists for
this and is Linux-only; the small-payload rows deserve a run there before
anything about them is called a property of the design.

### Does the restructuring cost anything when threading is off?

Phases were introduced so that I/O threads could exist, but they replaced the
loop for everyone: `-io-threads 1` is the default and no longer runs the code
that every other result in this directory was measured against. Three things
changed for a server that never sets the flag. Every ready connection is now
read before any of them executes, where the old loop finished one connection
before looking at the next. A batch of several commands is coalesced into one
arena shared by the whole server rather than a buffer allocated per batch. And a
batch of one keeps its reply by reference, which is the large-value bypass the
old `respondBatch` had and which had to be rebuilt rather than inherited.

`run-ab.sh` measures it: the tree at the commit before phases against the
current tree, both at their defaults. Both servers run at once and the arms
alternate rep by rep, so each pair of readings is a second apart rather than
half an hour, and `summarise-ab.py` divides them before taking a median.
Comparing two medians taken thirty minutes apart would mostly measure the
thirty minutes.

Paired ratios, current over pre-phases, darwin/arm64, five reps
(`results/phases-vs-inline-darwin.csv`). Only the cells whose spread stayed
under 6% are listed; the rest disagreed rep to rep by more than they differed:

| scenario | paired | spread |
|---|---|---|
| `PING` c=1 | 1.000x | ±2% |
| `PING` c=200 | 1.007x | ±5% |
| `SET` d=65536 | 1.005x | ±4% |
| `GET` d=65536 | 1.037x | ±6% |
| `SET` d=262144 | **1.100x** | ±5% |
| `GET` d=262144 | **1.129x** | ±3% |

Not a cost. Flat on small values and 10–13% *faster* at 256KB, which is where
reading every ready connection before executing any of them has the most to
batch. The 256KB rows are also the check that the single-reply bypass survived
the rewrite: a large value copied through the arena rather than kept by
reference would show there and nowhere else, and as a loss rather than a gain.

The B arm is the current tree, so it carries everything added since the phases,
not the phases alone. What that amounts to is one thing — cross-keyspace type
checking, measured separately below at roughly 1–2% — and the append-only file,
which is off by default and does nothing when it is. So the phases themselves
account for the gain and a little more.

### What does cross-keyspace type checking cost?

Every command now resolves its key against all the keyspaces before running, so
a name means one thing and `DEL` deletes it. That is a lookup per keyspace on
the way into every command, and the alternative — a directory mapping names to
owners — was rejected on memory: a second map entry and a second copy of every
key, which showed up immediately as the accounting estimate falling to 70% of
real heap.

Paired ratios, checking over not checking (`results/typecheck-darwin.csv`),
cells with spread under 6%:

| scenario | paired | spread |
|---|---|---|
| `PING` c=1 | 0.998x | ±4% |
| `pipe` `PING` P=1 | 1.011x | ±5% |
| `GET` d=65536 | 1.000x | ±4% |
| `GET` d=262144 | 0.989x | ±4% |
| `SET` d=8192 | 1.014x | ±3% |
| `SET` d=65536 | 0.975x | ±3% |
| `SET` d=262144 | 0.986x | ±4% |

Between 0.975x and 1.014x, so on the order of one to two percent, at the edge of
what five reps resolve. That is the price of a name meaning one thing.

### A three-rep pass got this backwards

The first sweep here used three reps and reported `-io-threads 4` at 0.74x–0.90x
across half the size sweep — a clear, consistent-looking regression. Five reps
over the same configurations returned the table above, in which the same
workloads are flat or ahead. Nothing about the server changed between the two.

The second run had a fault of its own: it shared the machine with a `go test`
loop being used to wait for it. Both runs were discarded and the recorded CSV is
from a third, with nothing else running. The lesson already in this file was the
right one, and it is not enough on its own — reps fix sampling noise, and only
an otherwise idle machine fixes the rest.

## Ways this harness produced confident, wrong numbers

Recorded because each one looked like a result at the time.

**Ports were never advancing.** `p=$(next_port)` runs in a subshell, so the
parent's counter never moved and every server tried to bind the same port. The
redis teardown compounded it: `pkill -f "redis-server --port 1"` never matched,
because redis rewrites its process title to `redis-server *:PORT`. Each memkv
server then failed to bind and — because a failed bind returns from
`RunAsyncTCPServer` while `main` stays blocked on `wg.Wait()` — stayed alive with
no listener. The readiness probe passed anyway, because redis was still
answering. **Every throughput number from that harness, on both platforms, was
measured against one surviving redis-server.** The tell was four different
implementations agreeing to within 2% on every row, which read as a finding
rather than as a fault. `start_server` now asserts via `lsof` that the process
owning the port is the one it just launched, and exits rather than measuring the
wrong server.

**A subshell ate the port counter, twice.** The A/B script
assigned ports with `PORT_A=$(start_one ...)`. Command substitution runs in a
subshell, so `PORT=$((PORT+1))` advanced a copy and the parent's counter never
moved: both arms were handed the same port, the second server failed to bind,
and every one of its readings was zero. This is the *first* failure in this
list, written down in this very file, reproduced verbatim in a new script
written after reading it.

What made it worse than a repeat is that the guard did not catch it. The
ownership assertion compared process names by substring, so `memkv` matched a
running `memkv-base` and the arm that had failed to bind was reported as
verified. The run completed, exited zero, and produced a full table of paired
ratios between 0.967x and 1.085x — a reassuringly null result, because it was
one binary compared against itself. It was published as evidence that the
read/execute/write phases cost nothing when threading is off, and that claim was
withdrawn and re-measured.

The guard now asserts that the pid owning the port is the pid just launched. A
name can be coincidentally right; a pid cannot.

**Single samples lie.** A one-shot comparison of the framing fix showed a 14.5%
*regression*; five reps per side reversed the sign, with a spread of
150k–222k rps. Separately, a P=64 run at `-n 100000` finishes in ~70ms, short
enough that startup dominates — it reported 1.45M where a longer run reported
5.4M. Always at least 5 reps, always compare medians, always check the run is
long enough to measure anything.

This one recurred while measuring I/O threads: a
three-rep sweep put `-io-threads 4` between 0.74x and 0.90x across half the
value sizes, which read as a clear and consistent regression. Five reps and
medians over the same configurations returned 0.91–1.08x. The regression was
noise, and the rule above was written down precisely so that it would not be
believed a second time.

**Noise floors are not uniform.** At c=50 the event loop needs a 10.6% delta to
be significant while `net.Conn` needs 0.8%. A single global figure would have
been wrong in both directions. `run-hyperfine.sh` measures each configuration
separately.

**A long session poisons its own later runs.** A run at c=1000 opens 1000
sockets and TIME_WAIT holds each for ~15s against an ephemeral range of ~16k.
Back-to-back suites exhaust it, `redis-benchmark` fails with "Cannot assign
requested address", and hyperfine writes an empty file and moves on. The scripts
now drain before each configuration and scale run counts to the socket budget.

One more, in `run-offloopback.sh`: a server that failed to bind was skipped with a
message while the script still exited zero, so CI reported success with a hole in
the results. Missing servers now fail the run.

## Which results are authoritative

Everything under `results/` is kept, including the wrong runs, so the record is
auditable. Read them in this order:

| path | status |
|---|---|
| `results/ci/`, `results/ci2/` | **authoritative.** Linux/epoll, 7 servers, corrected harness. Two independent full runs; headline ratios agree within 1.7% |
| `results/matrix-fair.csv` | **authoritative.** darwin/kqueue, corrected harness, all servers with `TCP_NODELAY` |
| `results/memory-fair.csv`, `results/latency-fair.csv` | **authoritative.** darwin memory and latency percentiles |
| `results/hyperfine3/` | **authoritative.** per-config noise floors, 12/12 cells, drained between configs |
| `results/iothreads-darwin.csv` | **authoritative.** darwin, `-io-threads` 1/2/4/8, 5 reps. Loopback, so see the caveat above |
| `results/phases-vs-inline-darwin.csv` | **authoritative.** darwin, the loop before and after phases, interleaved arms, 5 reps. Re-measured after the port bug above invalidated the first run |
| `results/typecheck-darwin.csv` | **authoritative.** darwin, cross-keyspace type checking on and off, interleaved arms, 5 reps |
| `results/linux3/` | superseded. Linux post-`TCP_NODELAY`, 4 servers only |
| `results/linux2/` | superseded. Linux pre-`TCP_NODELAY` — the run where the Nagle stall was found |
| `results/matrix.csv`, `results/memory.csv`, `results/memtier.csv` | superseded. Corrected harness, but before `TCP_NODELAY`, so unfair to the event loop |
| `results/hyperfine/`, `results/hyperfine2/` | superseded. Port exhaustion left cells empty |
| `results/*-INVALID-port-collision.csv` | **wrong.** Kept as the record of the bug above. Do not cite |

Fairness note: Go's `net` package sets `TCP_NODELAY` by default, so any run where
the event loop lacked it was comparing against a handicapped opponent. Only the
files marked authoritative have it on both sides.

## Environment

darwin/arm64, 12 CPU, 24 GB, Go 1.26.6, and `ubuntu-latest` (2 vCPU) via
`.github/workflows/bench.yml`. `redis-benchmark` 8.10.1, `memtier_benchmark`
2.5.1, `hyperfine` 1.20.0. Absolute numbers are not comparable between the two
machines; the ratios are.
