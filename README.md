![img.png](memkv.png)
# MemKV — a Redis-inspired in-memory database in Go

MemKV is an in-memory key-value database written from scratch in Go. It speaks
**RESP**, the Redis serialization protocol, so `redis-cli` and ordinary Redis
clients talk to it unmodified.

It was built to explore how a high-performance network server actually works:
event loops, socket handling, custom and probabilistic data structures, and the
low-level design decisions underneath a real database.

> **This is a fork of [quangh33/memkv](https://github.com/quangh33/memkv).**
> Upstream is the original design and the origin of everything below the network
> layer. This fork rewrote the I/O path — see [What this fork
> changes](#what-this-fork-changes) — and carries a benchmark harness that
> upstream does not. `main` tracks upstream unchanged; the work lives on
> `develop`.

## Key features

- **RESP compliant.** Works with `redis-cli` and standard Redis clients.
- **Single-threaded event loop.** `epoll` on Linux, `kqueue` on macOS, selected
  by build tag. A single goroutine serves all connections, so per-connection
  memory stays near zero and commands need no locking.
- **Custom data structures**, implemented from scratch:
  - Skip list, for sorted sets (`ZADD`, `ZRANK`, …)
  - Geohash, for geospatial indexing (`GEOADD`, `GEODIST`, …)
- **Probabilistic data structures:**
  - Scalable Bloom filter — set membership in little memory (`BF.MADD`, `BF.EXISTS`)
  - Count-Min sketch — frequency estimation over a stream (`CMS.INCRBY`, `CMS.QUERY`)
  - HyperLogLog — distinct-element counting in a fixed 12KB, whatever the
    cardinality, within about 0.8% (`PFADD`, `PFCOUNT`, `PFMERGE`)
  - Cuckoo filter — set membership that supports deletion, which a Bloom filter
    cannot, in less space for the same accuracy (`CF.ADD`, `CF.EXISTS`, `CF.DEL`)
  - Morris counter — counting to four billion in a single byte rather than
    four, within 20% (`MORRIS.INCRBY`, `MORRIS.QUERY`)
- **Bounded by memory, not just by key count.** `-maxmemory 512mb` holds the
  keyspace to a byte budget, so the number of keys adapts to how big they are.
  It covers **every** type, not only strings: one 1MB budget holds 1733 string
  keys, or 2826 sets, or 76 HyperLogLogs, and eviction can free a sketch to make
  room for a string. Go gives no way to ask the allocator what a value cost, so
  the accounting is estimated and calibrated against real heap growth — within
  6% for strings and 2.3% for the collection types — with tests comparing the
  estimates against `HeapAlloc` so they cannot drift.
- **Approximate LRU and LFU eviction.** A bounded keyspace: past `-maxkeys`,
  each new key evicts an old one. Rather than ordering every key by access time
  or frequency — which would cost a structure threaded through the keyspace and
  turn reads into writes — both policies sample a few keys at random and evict
  the worst, keeping the best candidates between passes. LRU retains 83% of
  recently-used keys against 49% for random. LFU is the one to reach for when a
  scan would otherwise flush the cache: streaming 2000 read-once keys past a
  1000-key cache leaves LRU holding 1 of its 500 hot keys, and LFU holding 496.
- **Graceful shutdown.** SIGINT or SIGTERM unwinds the event loop so its deferred
  cleanup actually runs, rather than exiting the process from under it.

## Getting started

```sh
go run ./cmd                             # replies are coalesced per read (the default)
go run ./cmd -maxkeys 1000000            # bound the keyspace; evicts approximately-LRU
go run ./cmd -maxkeys 1000000 -evict lfu # ...or by frequency, which resists scans
go run ./cmd -maxmemory 512mb            # bound by bytes instead of key count
redis-cli -p 8081                        # from another terminal
```

Other I/O modes exist for benchmarking; `-mode` selects them and
[`bench/README.md`](bench/README.md) explains what each one is for.

## What this fork changes

Upstream's event loop answered one command per read and treated every write as
having succeeded. That is invisible against `redis-cli` typing one command at a
time, and comes apart under pipelining or values larger than a single read.

**Correctness**

| | |
| :--- | :--- |
| Partial and pipelined frames | A read is not a message. Bytes are buffered per connection and only whole frames are parsed, so a split command is no longer truncated and a pipelined batch no longer loses every command after the first. |
| Incomplete replies | A short write left a truncated frame on the wire and the client misparsed everything after it; `EAGAIN` on a full send buffer dropped the reply outright. Writes now complete or report why they could not. |
| Frame and buffer bounds | A length header is checked before it is trusted, and unparsed bytes per connection are bounded, so a client cannot open a frame it never finishes and make the server buffer without limit. |
| Spurious readability | `EAGAIN` on a read no longer closes the connection. |
| Per-connection failures | A socket that fails to configure costs that client its connection instead of unwinding the server and disconnecting everyone. |
| Shutdown | The loop is woken and unwound so its deferred `Close` calls run. Previously `os.Exit` skipped them all — the "graceful shutdown" above was not true before this fork. |

**Performance** — platform is noted per row, because it matters: the Nagle
stall is a Linux delayed-ACK timer and does not reproduce the same way on
darwin. Figures are medians of repeated runs. Method and raw data are in
[`bench/`](bench/README.md).

| | |
| :--- | :--- |
| Nagle vs. delayed ACK *(Linux)* | `TCP_NODELAY` was never set, so a pipelined client was pinned at ~1220 batches/second regardless of batch size — a 40ms delayed-ACK timer, not a throughput limit. |
| Reply coalescing *(Linux)* | Replies from one read are sent as one write instead of one write per command: **4.1x / 8.1x / 13.9x** at pipeline depth 8 / 16 / 64, and 3.0x at P=64 on darwin. Now the default. |
| Large values *(darwin)* | The read path re-copied its buffer on every read, quadratic in value size. Removing that, sizing each read to what the frame still needs, and reading straight into the destination: **11.5x** at 256KB values, 336 → 3866 MB/s. |
| Reply encoding *(darwin)* | Replies were built with `fmt.Sprintf` and then converted, copying every payload twice. Appending instead: **2.7–3.4x** faster encoding, allocations per reply down from 8–9 to 2. |

## Supported commands

| Category | Commands |
| :--- | :--- |
| **General** | `PING` |
| **String** | `SET`, `GET`, `DEL`, `TTL`, `EXPIRE`, `INCR` |
| **Keyspace** | `DBSIZE` |
| **Server** | `INFO`, `MEMORY USAGE` |
| **Sorted Set**| `ZADD`, `ZRANK`, `ZREM`, `ZSCORE`, `ZCARD` |
| **Set** | `SADD`, `SREM`, `SCARD`, `SMEMBERS`, `SISMEMBER`, `SMISMEMBER`, `SRAND`, `SPOP` |
| **Geospatial** | `GEOADD`, `GEODIST`, `GEOHASH`, `GEOSEARCH`, `GEOPOS` |
| **Bloom Filter**| `BF.RESERVE`, `BF.INFO`, `BF.MADD`, `BF.EXISTS`, `BF.MEXISTS` |
| **Count-Min** | `CMS.INITBYDIM`, `CMS.INITBYPROB`, `CMS.INCRBY`, `CMS.QUERY` |
| **Morris Counter** | `MORRIS.INITBYDIM`, `MORRIS.INITBYPROB`, `MORRIS.INCRBY`, `MORRIS.QUERY`, `MORRIS.INFO` |
| **HyperLogLog** | `PFADD`, `PFCOUNT`, `PFMERGE` |
| **Cuckoo Filter** | `CF.RESERVE`, `CF.ADD`, `CF.ADDNX`, `CF.EXISTS`, `CF.MEXISTS`, `CF.DEL`, `CF.COUNT`, `CF.INFO` |

## Benchmarks

[`bench/`](bench/README.md) holds the harness and every recorded run, including
the wrong ones, with notes on how the harness was mistaken before it was right.
It exists because of [quangh33/memkv#2](https://github.com/quangh33/memkv/issues/2),
which asks whether the hand-rolled event loop should be replaced with Go's `net`
package. The short answer measured here: the netpoller is 8–19% slower for small
values and uses far more memory per connection, while winning at large payloads
and deep pipelining.

## Future work

- [x] HyperLogLog — dense encoding, 16384 six-bit registers, Ertl's estimator.
      Estimates track real Redis to within the error bound of both. Redis's
      sparse encoding, which costs a few hundred bytes instead of 12KB for keys
      with few members, is not implemented.
- [x] Morris counter — approximate counting in one byte where an exact counter
      needs four. The cell holds an exponent and raises it only with
      probability (1+a)^-c, which makes the estimate ((1+a)^c - 1)/a unbiased
      for any number of increments; a = 0.08 puts eight bits within 3% of the
      range of a Count-Min sketch's 32-bit cell, for 20% relative error per
      counter. Exposed as the cells of a hashed counting table, since that is
      the only place the saving is real — one counter per key would be swamped
      by the ~100 bytes a key costs anyway. Rows are combined by median, not
      the minimum a Count-Min sketch takes: a minimum is right only for exact
      cells, and over noisy ones it finds the unluckiest row rather than the
      cleanest. Measured, the minimum reads 21% low at depth 5 and 26% low at
      depth 9 — worse the more rows are added, which is backwards — while the
      median stays within 2%.
- [x] Cuckoo filter — 16-bit fingerprints, four to a bucket, partial-key cuckoo
      hashing. Measured: 96–98% load factor, 16.3–16.7 bits per item at a
      0.009–0.013% false positive rate, against 17.7 bits per item for a Bloom
      filter at a comparable rate — and unlike Bloom it can delete. Fixed
      capacity, so it refuses inserts when full rather than growing.
- [x] Approx LRU eviction — five random samples per eviction, with a 16-entry
      pool carried between passes. Measured hot-key retention: 83% against 49%
      for random eviction, where true LRU would score 100%. The pool is worth
      most of that: plain sampling scores 70% at five samples and needs 20 to
      reach 87%. Bounded by key count rather than by memory.
- [x] Approx LFU eviction — Redis's logarithmic counter, incremented with
      probability 1/(base*factor+1) so eight bits span millions of accesses, and
      decayed lazily so a key popular yesterday does not outrank one in use
      today. Measured: 99% of a working set survives a 2x scan that leaves LRU
      with 0%, while a working set that moves is still followed. Shares one
      64-bit field with LRU rather than adding its own, which is 76MB at the
      default key limit.
- [x] Memory-based bounding — `-maxmemory`, with per-key accounting calibrated
      against real heap growth, reported through `INFO` and `MEMORY USAGE`.
      Covers every keyspace: strings, sets, sorted sets, Bloom and cuckoo
      filters, Count-Min sketches and HyperLogLogs share one budget, and
      eviction chooses between them on a common scale.
- [ ] Longest Common Subsequence
- [ ] Threaded socket I/O, in the style of Redis `io-threads` — parallelise
      reads, writes and parsing while command execution stays single-threaded.
      The large-value path is now within range of what one core can copy, which
      is the wall that motivated the same change in Redis 6.
