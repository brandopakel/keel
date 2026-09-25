# Separate-host RESP comparison, September 24, 2026

The first measurement with the client and server on different machines. It
answers one question: over a real network, does Keel keep up with Redis on
the same hardware, workload and persistence? It does. Neither server is
distinguishable from the other here, and the network sets the pace.

## Setup

| | |
|---|---|
| Server host | Windows 11 (10.0.26200) desktop, Intel i7-13700F (24 threads), 16 GB; WSL2 Ubuntu 24.04.5 (kernel 6.18.33.2-microsoft-standard-WSL2, 8 GB to WSL), mirrored networking |
| Client host | MacBook Pro, macOS 26.5.1, Wi-Fi (802.11ax) on the same home LAN |
| Path | Tailscale, direct over the LAN, about 3 ms idle round trip |
| Keel | `v0.1.0-alpha.4` (5852df813c9d), the published linux_amd64 archive (sha256 6a16b75c...b793d1, verified) |
| Redis | 7.0.15 (Ubuntu package, jemalloc 5.3.0) |
| Client | memtier_benchmark 2.5.1: 4 threads x 25 connections, SET:GET 1:10, 256-byte values, 100,000 random keys, 30 s per run |
| Persistence | `none` (no AOF) or `everysec` (AOF, fsync every second) on both servers; Redis RDB saving off; both require AUTH |

Each run starts a fresh server process with an empty data directory. Three
repeats per cell, alternating which server runs first. `summary.csv` holds
all 24 runs; none failed and none reported errors. The script is
[`bench/run-separate-host-wsl.sh`](../../run-separate-host-wsl.sh).

## Results (median of three, with range)

| Persistence | Pipeline | Keel ops/s | Redis ops/s | Keel / Redis | Keel p99 ms | Redis p99 ms |
|---|---|---|---|---|---|---|
| none | 1 | 12,655 (12,119-12,763) | 12,357 (11,767-12,536) | 1.02 | 16.3 | 16.4 |
| none | 16 | 148,443 (134,241-156,891) | 148,109 (142,291-149,486) | 1.00 | 22.5 | 23.3 |
| everysec | 1 | 12,304 (12,273-12,370) | 12,281 (12,180-12,351) | 1.00 | 15.4 | 15.4 |
| everysec | 16 | 151,582 (149,039-152,787) | 157,284 (144,786-160,701) | 0.96 | 22.3 | 19.8 |

In every cell each server's median falls inside the other's range.

## What this does and does not show

- With one request in flight per connection, throughput is 100 connections
  divided by the round trip (about 8 ms under load on Wi-Fi), for both
  servers. Server speed barely enters into it.
- With 16 in flight, both reach about 150,000 ops/s, still near the link's
  limit. The everysec/16 cell's 4% gap is within the run-to-run spread.
- It does not rank the servers' raw speed, test a wired or data-centre
  network, or cover memory use, large values, or restart and rewrite
  behaviour under load. A wired LAN or two cloud hosts would move the
  bottleneck back toward the servers.
