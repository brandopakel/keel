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
| `run-memory.sh` | RSS against connection count |
| `run-offloopback.sh` | the same, across a veth pair between network namespaces (Linux) |
| `summarise.py` | reduces a matrix CSV to medians and comparison tables |

`SERVERS=`, `BIN=`, `OUT=`, `REPS=`, `PORT_BASE=` are all overridable.

## Four ways this harness produced confident, wrong numbers

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

**Single samples lie.** A one-shot comparison of the framing fix showed a 14.5%
*regression*; five reps per side reversed the sign, with a spread of
150k–222k rps. Separately, a P=64 run at `-n 100000` finishes in ~70ms, short
enough that startup dominates — it reported 1.45M where a longer run reported
5.4M. Always at least 5 reps, always compare medians, always check the run is
long enough to measure anything.

**Noise floors are not uniform.** At c=50 the event loop needs a 10.6% delta to
be significant while `net.Conn` needs 0.8%. A single global figure would have
been wrong in both directions. `run-hyperfine.sh` measures each configuration
separately.

**A long session poisons its own later runs.** A run at c=1000 opens 1000
sockets and TIME_WAIT holds each for ~15s against an ephemeral range of ~16k.
Back-to-back suites exhaust it, `redis-benchmark` fails with "Cannot assign
requested address", and hyperfine writes an empty file and moves on. The scripts
now drain before each configuration and scale run counts to the socket budget.

A fifth, in `run-offloopback.sh`: a server that failed to bind was skipped with a
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
