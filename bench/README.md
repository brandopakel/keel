
## Four-way comparison (added later)

Four servers, one session, same box, same methodology:

| label | implementation |
|---|---|
| `redis-server` | real Redis, as a calibration reference |
| A1 `-mode kqueue` | event loop, one write syscall per reply (current upstream design + framing fix) |
| A2 `-mode kqueue-wbuf` | event loop, replies coalesced per read |
| B `-mode net` | `net.Listener`, goroutine per connection, `bufio` both ways |

A1 vs A2 isolates write buffering. **A2 vs B isolates the I/O mechanism, which is
the comparison this issue actually asks for** — same framing, same write policy,
same execution semantics (B serialises behind one mutex to match the event loop's
semantics; sharding would be faster and would make the comparison meaningless).

Scripts: `run-matrix.sh` (redis-benchmark, 460 runs), `run-memtier.sh`
(randomised keys/payloads), `run-hyperfine.sh` (per-config noise floors),
`run-memory.sh` (RSS vs connection count). `summarise.py` reduces to medians.
