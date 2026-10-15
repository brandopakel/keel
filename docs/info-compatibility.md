# INFO and monitoring compatibility

Status: October 2026. All of the parts below, (a) to (d), are merged; (d)
was the last. Still open: the Replication section in Redis's terms
("Replication" below), and the Keyspace section's `avg_ttl`.

Monitoring built for Redis reads `INFO`, `CONFIG GET`, `SLOWLOG` and `LATENCY`.
The standard Prometheus exporter, `redis_exporter`, and the Grafana dashboards
built on it are the common case. On October 7, 2026, that exporter reported
23 metric names for Keel and 191 for Redis 8.10. A Redis dashboard pointed at
Keel showed no throughput, hit rate, CPU, command latency or slow log.

This page lists what Redis 8.10 reports, what Keel reports, and what Keel will
report. It is checked field by field against a running Redis 8.10. The
**Live telemetry** workflow (`.github/workflows/telemetry.yml`) measures the
gap: it puts the same exporter in front of both servers and reports
`keel_ci_exporter_metric_names{server}` to Grafana.

## Rules

1. **Same quantity, same name.** A field Keel can compute with Redis's meaning
   is reported under Redis's name, in Redis's units and format, and is reset
   by the same events (`CONFIG RESETSTAT`, restart).
2. **A feature Keel does not have reports Redis's value for that feature
   being unused,** when that value is a true statement about Keel. Examples
   are `blocked_clients:0`, since Keel has no blocking commands, and
   `cluster_enabled:0`. Dashboards and alerts that read these then work
   unchanged.
3. **A Redis internal with no Keel counterpart is left out, not faked.** This
   covers jemalloc's allocator figures, active defrag, Lua and functions
   memory, fork copy-on-write, and Redis 8's hash templates.
4. **A quantity Keel measures differently is left out.** For example,
   Keel's `used_memory` estimates the keyspace, not the allocator's total,
   so ratios that set it against RSS (`mem_fragmentation_ratio`,
   `used_memory_dataset_perc`, `rss_overhead_*`) would compare unlike things.
5. **Keel's own fields stay.** This covers `keel_version`, `retained_*`,
   `request_allocation_*`, `command_allocation_*`, the extra `aof_*` fields,
   and the `replication_*` and `failover_*` fields.
6. **`redis_version` stays `7.0.0`.** It names the Redis release whose
   command forms Keel follows, and libraries gate features on it (see
   `RedisCompatibleVersion` in `internal/core/commands_server.go`).

## The work, in six pull requests

Each lands on its own, smallest risk first.

- **(a) Fields with no per-command cost.** Server identity and uptime, CPU,
  peak memory and RSS, the Clients and Keyspace details, the features Keel
  does not have, and Redis's section order. Read when `INFO` runs, or
  sampled in the cron.
- **(b) `CONFIG GET`.** Keel's real settings under Redis's parameter names,
  read-only. `CONFIG SET` and `CONFIG REWRITE` refuse in Redis's words.
- **(c) Per-command statistics,** the part that adds work to every command,
  in three pull requests. Its cost was measured before any of it was written
  (below), and the owner decided on October 9, 2026 how it ships.
  - **(c1) Counts that need no clock:** `total_commands_processed`,
    `instantaneous_ops_per_sec`, `total_error_replies`, errorstats, and
    `CONFIG RESETSTAT`. This adds a counter and a look at the reply's first
    byte, and it passes the paired command-path job's 0.98 rule like any
    change.
  - **(c2) `keyspace_hits` and `keyspace_misses`,** counted on the key
    lookups of read commands, under the same rule.
  - **(c3) Timing,** with Redis's exact fields and meaning. Every command is
    timed, as Redis times it, with no sampling. It ships in three pull
    requests:
    - **(c3a)** commandstats, and the two clock reads it needs;
    - **(c3b)** latencystats and the `LATENCY` command, from a histogram per
      command;
    - **(c3c)** `SLOWLOG`, with its settings in `CONFIG GET`. The slow log hides what Redis hides: the arguments of
      `AUTH`, of `HELLO`'s `AUTH` and of `CONFIG SET requirepass`.

  **What timing costs, measured October 8, 2026.** These are paired
  command-path runs, three each, on hosted runners:
  - two monotonic clock reads (`runtime.nanotime`) around each command add
    45 to 60 ns per command, a median of 1.113;
  - the processor's time-stamp counter costs 6 to 40 ns, a median of 1.037
    to 1.040;
  - counting alone costs nothing measurable, a median of 1.005.

  End to end, through memtier on the matched job, the clock reads cost a
  median of 0.994 and 0.997 across 36 workloads. Pipelined workloads lose 3
  to 10% (pipeline-64 0.905 and 0.969, pipeline-16 0.93), because pipelining
  spreads the system calls and leaves the per-command cost showing.

  **The owner's decision:** the timing is exact, as Redis's, and its cost is
  accepted and documented. (c3)'s paired job will read about 1.11, and the
  0.98 rule is waived for (c3) by that decision. The time-stamp counter is
  used only if it is monotonic and consistent across cores on the hosts
  that matter, and gives the microseconds Redis would report; otherwise
  `runtime.nanotime`. (c3) follows the change to `replicaCommandError`
  that touches the same dispatch path.

  **The clock (c3a).** Redis's default build has `USE_PROCESSOR_CLOCK` off
  (`monotonic.c`), so its `call()` times a command with `ustime()`, the
  wall clock, in whole microseconds: `duration = ustime() - call_timer`.
  The microseconds Redis reports are therefore the wall clock's, and the
  time-stamp counter matches only a Redis built with `USE_PROCESSOR_CLOCK`.
  Keel reads `runtime.nanotime`, the monotonic clock, and rounds as Redis
  does, the difference of the two instants each in whole microseconds.
  - **Improvement over Redis:** a step of the system clock, such as an NTP
    correction, cannot make a command's time negative or huge in Keel, as
    it can in Redis. In steady time the two clocks agree, and no client
    depends on the step.

  **The slow log's secrets (c3c).** Redis redacts what it treats as secret in
  the slow log, as `(redacted)`: `AUTH`'s password, and its user in the
  three-argument form; the user and password after `HELLO`'s `AUTH`; and the
  value of a sensitive setting in `CONFIG SET` (`requirepass`, `masterauth`,
  `masteruser` and the two TLS key passwords).
  - **Improvement over Redis:** Redis redacts them only once the command
    has got as far as reading them. So an `AUTH` of more than two arguments
    (refused for its syntax first), a `HELLO` of an unsupported protocol
    version (refused first), or a `CONFIG SET` of an odd count (refused
    first) lands in the slow log with the password as sent, if it runs
    slowly enough. Keel redacts them whatever the command goes on to do. No
    client reads its own password back from `SLOWLOG GET`.
- **(d) Network bytes**, counted on the connection read and write paths in
  `internal/server`, with atomic counters as Redis's are, since the I/O
  threads read and write too. The engine samples their rates through the
  hook INFO already reads the transport through.

Engine-side counters are new fields on `Engine`, after `settings`, so the hot
offsets do not move. Server-side figures (port, start time, connections
refused, network bytes) come through the hook the event loop already installs
for `INFO clients` (`SetClientBuffers`), and `CONFIG RESETSTAT` resets them
through one more (`SetStatsReset`). That keeps them in `internal/server`,
where the plan's phase 6 wants server state.

## Server

| Redis field | Keel today | Plan |
| --- | --- | --- |
| `redis_version` | `7.0.0` | Stays (rule 6) |
| `redis_mode` | `standalone` | Stays |
| `os` | - | (a) `uname` system, release and machine, as Redis prints them |
| `arch_bits` | - | (a) `64` or `32` |
| `multiplexing_api` | - | (a) The event loop's: `epoll` or `kqueue` |
| `process_id` | - | (a) |
| `process_supervised` | - | (a) `no`. Keel does not talk to systemd or upstart. |
| `run_id` | - | (a) 40 random hex characters, new at each start |
| `tcp_port` | - | (a) |
| `server_time_usec` | - | (a) |
| `uptime_in_seconds`, `uptime_in_days` | - | (a) From when the engine was made, which the server does at startup |
| `hz`, `configured_hz` | - | (a) `1000 / -cron-interval-ms`, which is 10 by default, as in Redis |
| `executable`, `config_file` | - | (a) The executable's absolute path. `config_file` is empty: Keel takes flags. |
| `io_threads_active` | - | (a) `1` when `-io-threads` is above 1 |
| `redis_git_sha1`, `redis_git_dirty`, `redis_build_id`, `gcc_version`, `atomicvar_api`, `monotonic_clock`, `lru_clock`, `listener0` | - | Left out: Redis build details with no Keel counterpart. `keel_version` carries Keel's. |

## Clients

| Redis field | Keel today | Plan |
| --- | --- | --- |
| `connected_clients` | yes, every connection | (i), done. The connections that are not replicas, as Redis's counts `server.clients` less `server.slaves` (server.c). A connection is a replica's once the primary has accepted its pull (`KEEL.REPL.PULL`), as part (d) marks it. |
| `maxclients` | - | (a) `-maxclients` |
| `blocked_clients`, `total_blocking_keys`, `total_blocking_keys_on_nokey` | - | (a) `0`: no blocking commands |
| `pubsub_clients` | - | (a) `0`: no pub/sub |
| `tracking_clients` | - | (a) `0`: no client tracking |
| `cluster_connections` | - | (a) `0`: no cluster |
| `watching_clients`, `total_watched_keys` | - | (a) `0`: no `WATCH` (see the README's integration contract) |
| `client_recent_max_input_buffer`, `client_recent_max_output_buffer` | - | (i), done. The most one connection held of requests and of replies in the last seven seconds, a replica's too, kept as Redis keeps them: eight slots of a second each, the slot after the current one cleared (`CLIENTS_PEAK_MEM_USAGE_SLOTS`, server.c). Redis samples each client about once a second in its cron; Keel samples every connection when the engine takes its rate sample, ten times a second, and when `INFO` runs, so the command path gains nothing. A connection's size is what the event loop accounts it as retaining (`retained_*`): its request buffer and parsed requests, and its reply buffer. Neither server sees a request read, run and released between two samples. |
| `clients_in_timeout_table`, `active_clients` | - | Left out: Redis bookkeeping with no Keel counterpart |

Keel's `command_allocation_*` lines are written after the Clients section's
blank line today, so they fall outside any section. (a) moves them inside.

## Memory

| Redis field | Keel today | Plan |
| --- | --- | --- |
| `used_memory`, `used_memory_human` | yes, a keyspace estimate | Stays, with its meaning documented (rule 4) |
| `used_memory_peak`, `used_memory_peak_human`, `used_memory_peak_time`, `used_memory_peak_perc` | - | (a) The peak of Keel's `used_memory`, sampled in the cron and when `INFO` runs, as Redis does |
| `used_memory_rss`, `used_memory_rss_human` | - | (a) Resident set size from `/proc/self/statm` on Linux. Left out on platforms where Go cannot read it without cgo. |
| `total_system_memory`, `total_system_memory_human` | - | (a) Physical memory |
| `maxmemory`, `maxmemory_human`, `maxmemory_policy` | yes | Stays |
| `mem_allocator` | - | Left out: Go's allocator is none of the allocators Redis names (jemalloc, tcmalloc, libc) |
| `mem_fragmentation_ratio`, `mem_fragmentation_bytes`, `used_memory_dataset*`, `used_memory_overhead`, `used_memory_startup`, `rss_overhead_*`, `allocator_*` | - | Left out (rules 3 and 4) |
| `used_memory_lua*`, `used_memory_vm_*`, `used_memory_scripts*`, `used_memory_functions`, `number_of_cached_scripts`, `number_of_functions`, `number_of_libraries`, `used_memory_hash_templates` | - | Left out: no scripting, functions or hash templates |
| `mem_clients_slaves`, `mem_clients_normal` | - | (i), done. What the replicas' connections and the others retain, the `retained_client_bytes` split between them. **Differs in what it counts:** Redis adds each client's own structures (about 1 KiB each, idle); Keel counts the buffers its event loop accounts, so an idle connection counts what it still holds, often nothing. |
| `mem_clients_normal_shared`, `mem_clients_normal_unshared` | - | (i) `0`. Redis counts the replies that refer to stored values rather than copy them, and is at 0 while none does (networking.c); every Keel reply is a copy. |
| `mem_aof_buffer`, `mem_replication_backlog`, `mem_total_replication_buffers`, `mem_replica_full_sync_buffer`, `mem_cluster_*`, `mem_not_counted_for_evict`, `mem_overhead_db_hashtable_rehashing` | - | Left out for now. Each needs an accounting Keel does not keep in Redis's terms. |
| `active_defrag_running`, `lazyfree_pending_objects`, `lazyfreed_objects` | - | (a) `0`: Keel defragments nothing and frees nothing lazily |

## Persistence

| Redis field | Keel today | Plan |
| --- | --- | --- |
| `aof_enabled`, `aof_rewrite_in_progress`, `aof_rewrite_scheduled`, `aof_last_rewrite_time_sec`, `aof_current_rewrite_time_sec`, `aof_last_bgrewrite_status`, `aof_rewrites`, `aof_rewrites_consecutive_failures`, `aof_last_write_status` | yes | Stays. Check each against Redis's units: seconds, and `-1` when none. |
| `loading`, `async_loading` | - | (a) `0`. Keel replays its log before it accepts clients. |
| `rdb_bgsave_in_progress`, `rdb_saves`, `rdb_last_bgsave_status` | - | (a) `0`, `0`, `ok`: Keel takes no RDB snapshots |
| `rdb_changes_since_last_save`, `rdb_last_save_time`, `rdb_*_time_sec`, `rdb_last_cow_size`, `rdb_last_load_*`, `rdb_saves_consecutive_failures`, `current_cow_*`, `current_fork_perc`, `current_save_keys_*`, `aof_last_cow_size`, `module_fork_*`, `backup_in_progress` | - | Left out: no snapshots, no fork |

## Stats

| Redis field | Keel today | Plan |
| --- | --- | --- |
| `total_connections_received` | yes | Stays |
| `evicted_keys`, `expired_keys` | yes | Stays |
| `total_commands_processed`, `instantaneous_ops_per_sec` | - | (c1), done. A command counts once it has run, as Redis's `call()` counts it (see below). Ops per second is the mean of the last 16 samples, taken every 100 ms, as Redis takes them. |
| `keyspace_hits`, `keyspace_misses` | - | (c2), done. Each key a command looks up to read, as Redis's `lookupKeyRead` counts it (see below) |
| `total_error_replies` | - | (c1), done. Every error reply, refusals included, inside `EXEC`'s reply too |
| `unexpected_error_replies` | - | Left out. Redis counts the errors its replication link or its AOF client receives; Keel's replication is its own, so it has no counterpart. |
| `total_net_input_bytes`, `total_net_output_bytes`, `instantaneous_input_kbps`, `instantaneous_output_kbps` | - | (d), done. Every byte read from and written to a connection, counted on the transport's read and write paths with replication's included, as Redis's totals include it. The rates are the mean of the last 16 samples taken every 100 ms, divided by 1024, as Redis's are. |
| `total_net_repl_input_bytes`, `total_net_repl_output_bytes`, `instantaneous_*_repl_kbps` | - | (d), done. On a primary, what it sends a connection once that connection has pulled the log as a replica (`KEEL.REPL.PULL`), as Redis counts what it sends a client it has made a replica. On a replica, what it reads from its primary, as Redis counts what it reads from its master; its own requests are ordinary output, as a Redis replica's are. |
| `rejected_connections` | - | (a) Connections refused at `-maxclients` |
| `total_reads_processed`, `total_writes_processed` | - | (d), done. Every read and write the transport made on a connection, an empty read at a client's close included, on whichever thread made it. Redis sums its I/O threads' counts of the same events. The probe that sends each command on a connection of its own reads the same counts from both, and the same bytes. |
| `slowlog_commands_count`, `slowlog_commands_time_ms_max`, `slowlog_commands_time_ms_sum` | - | (c3c), done. The commands the slow log has taken since the last `CONFIG RESETSTAT`, and their time, in milliseconds with two decimals |
| `pubsub_channels`, `pubsub_patterns`, `pubsubshard_channels`, `tracking_total_*`, `evicted_clients`, `evicted_scripts` | - | (a) `0` |
| `total_forks`, `latest_fork_usec` | - | (a) `0`: Keel never forks |
| `sync_full` | - | (e). The full syncs a primary has served |
| `sync_partial_ok`, `sync_partial_err` | - | Left out: Redis counts a replica's reconnection that resumes, and a Keel replica pulls again every 100 ms (see Replication) |
| `expired_subkeys*`, `expired_keys_active`, `expired_stale_perc`, `expired_time_cap_reached_count`, `expire_cycle_cpu_milliseconds`, `total_eviction_exceeded_time`, `current_eviction_exceeded_time` | - | Left out for now. Each is an expiry or eviction internal that needs its own measurement. |
| `active_defrag_*`, `io_threaded_*`, `eventloop_*`, `avg_pipeline_length*`, `client_*_buffer_limit_disconnections`, `reply_buffer_*`, `unexpected_error_replies`, `dump_payload_sanitizations`, `hash_template*`, `acl_access_denied_*`, `migrate_cached_sockets`, `slave_expires_tracked_keys` | - | Left out (rule 3), or until Keel has the feature |

## Replication

Part (e). Redis's fields come first, in Redis 8.10.1's order
(`genRedisInfoString`, server.c), and Keel's own lines follow unchanged, as
extras: `primary_epoch`, `replica_epoch`, the `replication_*`, `failover_*`
and `replica_*` lines, `writable` and `primary_offset`. Keel's own `role` line
gives way to Redis's, since exporters and alert rules key on `role:master`
(redis_exporter's `redis_instance_info{role="master"}`, and the common alerts
for a missing or a second master), and tools and client libraries that find
replicas through INFO look for `role:slave`.

**Names.** Keel calls its servers a primary and a replica, everywhere it speaks
for itself: these docs, its logs, its flags (`-replicaof`), its own INFO lines
and HELLO's `role` (`master` or `replica`, as Redis's HELLO answers). INFO
replication's field names and its `role` value are Redis's wire format, which
Redis itself kept when it renamed the rest (`REPLICAOF`, `replica-*`), because
tooling matches on them: there a Keel primary is `role:master` and a Keel
replica `role:slave`, and the fields keep Redis's names (`connected_slaves`,
`slave_repl_offset`). The owner chose this on October 14, 2026, over a name of
Keel's own, which would have broken that tooling.

On a replica:

| Redis field | Keel |
| --- | --- |
| `master_host`, `master_port` | The primary `-replicaof` names |
| `master_link_status` | `up` while the replica's connection to its primary is open and its last pull succeeded; `down` before the first and between reconnections |
| `master_last_io_seconds_ago` | Seconds since the last successful pull; `-1` with no link, as Redis gives with no master client |
| `master_sync_in_progress` | `1` from the replica's first pull until its keyspace is a whole prefix of the primary's stream (`replica_ready`), `0` after |
| `slave_read_repl_offset`, `slave_repl_offset` | `replica_offset`. Keel applies a pull whole, so what it has read and what it has applied are one offset |
| `replica_full_sync_buffer_size`, `replica_full_sync_buffer_peak` | `0`: Keel applies a full sync as it arrives and keeps no buffer for one |
| `master_current_sync_attempts`, `master_total_sync_attempts` | Connections to the primary attempted since the link was last up, and in all |
| `master_link_down_since_seconds` | While down: seconds since the link went down, `-1` if it has never been up |
| `master_link_up_since_seconds`, `master_client_io_thread` | While up: seconds since it came up, and `0`, the event loop's thread |
| `total_disconnect_time_sec` | Seconds the link has been down since it was first up, the current outage included |
| `slave_read_only` | `1`: a Keel replica refuses writes, as Redis's does with `replica-read-only yes` |
| `slave_priority`, `replica_announced` | Left out: Sentinel's settings, which Keel has no counterpart of. A Keel replica is promoted explicitly (`KEEL.PROMOTE`) |
| `master_sync_total_bytes` and the other transfer fields | Left out: a full sync's size is not known before it arrives |

On every server:

| Redis field | Keel |
| --- | --- |
| `connected_slaves` | The replicas connected now: connections whose pull the engine accepted (part (d)) |
| `min_slaves_good_slaves` | Never written, as in Redis without `min-replicas-to-write`, which Keel does not have |
| `slave<n>` | One per connected replica: `ip` and `port` of its connection, `state=online`, `offset` its last pull started from, `lag` the seconds since that pull, `io-thread=0`. A Keel replica announces no listening port, so `port` is its connection's, which also keeps two replicas on one host apart |
| `master_failover_state` | `no-failover`: Keel has no `FAILOVER`; `KEEL.PROMOTE` and `KEEL.FENCE` act at once |
| `master_replid` | The stream's identity: a primary's epoch, and a replica's primary's once it has one (`replica_epoch`), as a Redis replica takes its master's ID. Keel's epoch is 32 hex digits, where Redis's ID is 40 |
| `master_replid2`, `second_repl_offset` | Forty zeros and `-1`, Redis's values with no previous ID: a Keel replica does not continue across a change of epoch |
| `master_repl_offset` | `primary_offset` on a primary, `replica_offset` on a replica |
| `repl_backlog_active` | `1` on a primary feeding replicas, which keeps the history a replica catches up from; `0` otherwise |
| `repl_backlog_size` | The history's bound, 16 MiB |
| `repl_backlog_first_byte_offset`, `repl_backlog_histlen` | The offset of the history's first entry, and the bytes it holds; `0` and `0` with none |

The Stats section gains `sync_full`, the full syncs a primary has served.
`sync_partial_ok` and `sync_partial_err` are left out: Redis counts a
replica's reconnection that resumes, and a Keel replica pulls again every
100 ms, resuming or not, so no count of Keel's means what Redis's does.

**Departures, documented.** Under replication protocol 1, Keel's default,
offsets count the batches the primary has recorded, not bytes; under
protocol 2 they count bytes, as Redis's do. A dashboard that subtracts a
replica's offset from its primary's shows the lag in batches under protocol 1.
The epoch's length is Keel's own. Both keep Redis's fields' meaning (where the
stream stands, which stream it is) and change their unit and form.

## CPU

| Redis field | Plan |
| --- | --- |
| `used_cpu_sys`, `used_cpu_user` | (a) `getrusage(RUSAGE_SELF)`, in seconds with six decimals, as Redis prints them |
| `used_cpu_sys_children`, `used_cpu_user_children` | (a) `getrusage(RUSAGE_CHILDREN)`, which is `0.000000` for Keel |
| `used_cpu_sys_main_thread`, `used_cpu_user_main_thread` | Left out. Redis reports them only where it can read a thread's own usage, and Keel's event loop is a goroutine. |

## Keyspace

`db0:keys=N,expires=N,avg_ttl=N,subexpiry=N`, all four, as Redis 8.10.1
writes them (part (f)):

- **The line is written only when the keyspace holds keys,** as Redis writes a
  database's line only then (`if (keys || vkeys)`, server.c). An empty Keel
  has a Keyspace section with no line, where it used to write
  `db0:keys=0,expires=0`.
- **`avg_ttl`** is Redis's running estimate, in milliseconds, of the time left
  to the keys that carry a TTL, from the active expiry cycle's sample, as
  Redis's `activeExpireCycle` keeps it (expire.c).
  - Each key the cycle samples that is not yet due adds its time left. Every 16
    rounds, and as the cycle stops, the sample's mean is folded in: the first
    estimate as it is, and after it the old one weighted 0.98 for each round
    that sampled, with Redis's `avg_ttl_factor` table, truncated as Redis's
    `long long` is.
  - It is `0` when no key carries a TTL, after `FLUSHDB` (Redis's `emptyData`),
    and on a replica, which runs no active expiry, as Redis's does not.
  - While expiry is suspended, as a replay of the log suspends it, the cycle
    samples nothing.
- **`subexpiry`** is `0`: Keel has no hash field expiry.

## Commandstats, Errorstats and Latencystats

**What counts as a command and as an error (c1)** follows Redis 8.10.1's
`call()` and `afterErrorReply()`:
- A command counts once it has run. INFO never counts itself, and
  `CONFIG RESETSTAT` counts as the first command after the reset.
- A command refused before it runs doesn't count: an unknown name, the wrong
  count, `NOAUTH`, a replica's `READONLY`.
- A command that fails as it runs does count: `WRONGTYPE`, a value that is
  not an integer, `AUTH` with the wrong password.
- `MULTI`, `EXEC` and `DISCARD` count as they run. A queued command counts
  when `EXEC` runs it, not when it is queued.
- The commands the transport answers itself (`AUTH`, `HELLO`, `QUIT`,
  `CLIENT`) count as they run. Inside `EXEC`, `WATCH` runs rather than
  queues, and counts.
- Replaying the log counts only the commands of a `MULTI` block. Redis's
  replay runs each command without `call()`, except that `EXEC` runs its
  block through `call()` (`aof.c`, `multi.c`).
- Every error reply counts toward `total_error_replies` and errorstats:
  refusals, the transport's own (`NOAUTH`, protocol errors), and those
  inside `EXEC`'s reply.

**What counts as a keyspace hit or miss (c2)** follows Redis's `lookupKey()`
and RedisBloom's `RedisModule_OpenKey` modes:
- Each key a command looks up to read is a hit if it is held, whatever its
  type, and a miss if not. An expired key is a miss, as Redis expires it on
  the lookup.
- These commands count, per key named:
  - `GET`, `MGET`, `LCS`, `STRLEN`, `GETRANGE`, `SUBSTR`, `GETEX`, `GETDEL`,
    `GETSET`;
  - `EXISTS` (per argument, repeats included), `TYPE`, `TTL`, `PTTL`;
  - the hash, list, set and sorted-set reads, and `GEODIST`, `GEOHASH`,
    `GEOPOS`, `GEOSEARCH`;
  - `PFCOUNT`, and `PFMERGE` with its destination;
  - `SET`'s `GET` option;
  - RedisBloom's reads (`BF.EXISTS`, `BF.MEXISTS`, `BF.INFO`, `CF.EXISTS`,
    `CF.MEXISTS`, `CF.COUNT`, `CF.INFO`, `CMS.QUERY`).
- Writes count nothing, as Redis's `lookupKeyWrite` counts nothing. Nor do
  `KEYS`, `SCAN` and `MEMORY USAGE`.
- A command that reads several keys stops counting where Redis's stops, at
  the first key of the wrong type (`PFCOUNT`, `PFMERGE`). `LCS` looks both
  its keys up first.
- `SRAND`, `MORRIS.QUERY`, `MORRIS.INFO` and `KEEL.DUMP` have no Redis
  counterpart of their own name. They count as `SRANDMEMBER`, `CMS.QUERY`,
  `CMS.INFO` and `DUMP` do.
- The log's replay counts the keys it reads, as Redis's does (`PFMERGE`).

The typed reads count in the type check, which already looks every key up,
so counting costs an increment.

**A read that parses first.** Some Redis reads parse arguments before
`lookupKeyRead`, and a malformed argument returns before anything is
counted: `getrangeCommand` reads its indexes first, `getexCommand` its
options, `lrangeCommand` and `lindexCommand` their indexes, and the like.
As each family is converted (phase 4 of the embedding plan), such a read
counts its hit or miss in its typed operation after parsing
(`readsAfterArguments`), and the type check counts only the hit of a key
of another type, once those arguments have passed; `stats-parity.py` has a
case with a bad argument for each. Strings are converted: `GETRANGE`,
`SUBSTR` and `GETEX` count this way. **The remaining gap:** the families not
yet converted (lists, sets, sorted sets, geo) still count through the type
check, before their arguments are parsed, so a read of theirs with a
malformed argument counts a hit or a miss that Redis's does not; each
family's conversion closes its part. `MGET` asks the other stores about a key the
strings do not hold only if they hold any key at all.

`scripts/stats-parity.py` checks all of this against Redis. It runs each case
on both servers, on one connection, after the same keys and a
`CONFIG RESETSTAT`, and compares how `total_commands_processed`,
`total_error_replies`, errorstats and the keyspace hits and misses moved. That
covers every command the two share, with present and missing keys and the
wrong type, plus refusals, transactions and the connection's own commands.
General validation runs it against Redis 8.10.1 and RedisBloom.


- **Commandstats (c3a), done:**
  `cmdstat_<name>:calls=N,usec=N,usec_per_call=N.NN,rejected_calls=N,failed_calls=N`,
  in `INFO all`, `everything` and `commandstats`, not in the default, as in
  Redis.
  - `<name>` is lower case, with `|` before a subcommand (`cmdstat_config|get`).
  - A command or subcommand Redis does not have gets no line.
  - A command counts as a call once it has run. Its time runs from just
    before the type check, which is where Redis's command finds its key's
    type, to the end of the command: the part Redis's `call()` times.
    `EXEC`'s time includes its commands', as Redis's does.
  - A refusal before the command runs (the wrong count, `NOAUTH`, a
    replica's `READONLY`, `MISCONF`, a refusal while queued inside `MULTI`)
    counts as `rejected_calls`, against the subcommand when one is named.
  - A command that answers with an error as it runs (`WRONGTYPE`, a failed
    `AUTH`) counts as `failed_calls`. `EXEC` fails only for its own
    `EXECABORT`, not for its commands' errors.
  - `usec_per_call` is computed in single precision, as Redis computes it.
  - Lines come in name order; Redis's come in its hash table's order, which
    no client relies on.
  - The log's replay counts no command's line, a `MULTI` block's
    included: Redis's `call()` updates commandstats only outside the loading
    of its log (`update_command_stats`, `isAOFLoadingContext`), though it
    counts every call it makes in `total_commands_processed`.
    `CONFIG RESETSTAT` clears every line.
  - Once the slow log has taken a command, its line adds `slowlog_count`,
    `slowlog_time_ms_sum` and `slowlog_time_ms_max`, as Redis 8.10's does
    (c3c).
- **Errorstats (c1), done:** `errorstat_<PREFIX>:count=N`, in Redis's
  default sections, between CPU and Cluster.
  - The prefix is the error's first word (`ERR`, `WRONGTYPE`, `NOAUTH`) when
    a space ends it within 32 bytes, and `ERR` otherwise.
  - `#`, `:`, CR and LF in a prefix are written as underscores.
  - Lines come in byte order.
  - Like Redis, it tracks at most 128 prefixes. One more replaces them all
    with `errorstat_ERRORSTATS_DISABLED:count=1`, and nothing more is counted
    by prefix until `CONFIG RESETSTAT`. `total_error_replies` goes on
    counting.
- **Latencystats (c3b), done:**
  `latency_percentiles_usec_<name>:p50=N.NNN,p99=N.NNN,p99.9=N.NNN`, in
  `INFO all`, `everything` and `latencystats`, not in the default, as in
  Redis.
  - Each command's time goes into a histogram of its own, as Redis records
    it with `latency-tracking` on, its default. The time is in nanoseconds,
    a whole number of microseconds times 1000, kept between 1 ns and 1 s.
  - The histogram is a line-for-line Go port of the HdrHistogram Redis
    bundles (`deps/hdr_histogram`), at Redis's settings: 1 ns to 1 s, two
    significant figures. Its percentiles, and `LATENCY HISTOGRAM`'s buckets,
    are the ones Redis reports for the same times.
  - `testdata/hdr_histogram_redis.json` holds the output of Redis's own C
    code for ten sets of times: none, sub-microsecond, cache reads, a long
    tail, values over a second, every bucket edge. The port is tested
    against it.
  - Percentiles are Redis's defaults (`latency-tracking-info-percentiles`,
    `50 99 99.9`), and `CONFIG GET` reports them.
  - `CONFIG RESETSTAT` clears every histogram, and the log's replay records
    no time, as for commandstats.

## Commands

| Command | Plan |
| --- | --- |
| `CONFIG GET pattern [pattern ...]` | (b), done; extended by `CONFIG SET` (below). Every directive Keel answers for, from Redis 8.10.1's own table (`internal/core/config_specs.go`), under Redis's names and aliases: Keel's settings read live, the settings that only tune Redis's internals at the value last set (Redis's default until then), and the settings Keel cannot change at Keel's value. `replicaof` is written `host port`, as Redis writes it; `dir` is the working directory, against which relative paths resolve. The transport's (`port`, `bind`, `maxclients`, `tcp-backlog`, `io-threads`, `hz`, `requirepass`) only with the server; Redis gives the password to any client that has logged in. A RESP2 flat array, or a RESP3 map. A name matches without regard to case and comes back as asked; a glob matches names and aliases in Redis's spelling, and never a hidden directive, which only its name finds, as in Redis. The slow log's settings came with (c3c). |
| `CONFIG SET parameter value [parameter value ...]` | Done (S1). Redis's `configSetCommand`, all pairs or none: see "CONFIG SET" below. |
| `CONFIG REWRITE` | (b), done. `The server is running without a config file`, Redis's answer when it was started without one: Keel takes flags. |
| `CONFIG RESETSTAT` | (c1), done. Resets what Redis's `resetServerStats` resets that Keel reports: `total_commands_processed` and the ops-per-second samples, `total_error_replies` and errorstats, `total_connections_received` and `rejected_connections` (client ids keep growing, as Redis's do), `expired_keys`, `evicted_keys`, `keyspace_hits`, `keyspace_misses`, commandstats, latencystats, the slow log's counts (not its entries, which `SLOWLOG RESET` clears), `aof_rewrites`, `aof_rewrites_consecutive_failures`, and the network bytes and reads and writes (d). `used_memory_peak` stays, as Redis keeps it. Keel's own fields keep counting. |
| `SLOWLOG GET [count]`, `LEN`, `RESET`, `HELP` | (c3c), done. The commands that took `slowlog-log-slower-than` microseconds or more (10000), newest first, at most `slowlog-max-len` (128). Each entry has Redis 8.10's seven fields: id, Unix time, duration in µs, the arguments (at most `slowlog-entry-max-argc`, 32, the last place saying how many more; each cut at `slowlog-entry-max-string-len`, 128 bytes, saying how many more), the client's address (`ip:port`, `[ip]:port` for IPv6, as Redis writes it) and name, and how many arguments the command really had. `EXEC` is not logged, as in Redis; what it runs is. `GET`'s count is read as Redis reads it, `-1` meaning all. The four settings are Redis's defaults, which `CONFIG GET` reports. `CONFIG RESETSTAT` clears the counts and keeps the entries, and the log's replay adds none. |
| `LATENCY LATEST`, `HISTORY`, `RESET`, `GRAPH`, `DOCTOR`, `HELP` | (c3b), done. Redis's latency monitor is off by default (`latency-monitor-threshold 0`, which `CONFIG GET` reports), and Keel has none, so these answer as Redis's do when it has never been on: `LATEST` and `HISTORY` an empty array, `RESET` `0`, `GRAPH` Redis's `No samples available for event` error, and `DOCTOR` its report that monitoring is disabled. Each subcommand has Redis's count. |
| `LATENCY HISTOGRAM [command ...]` | (c3b), done. Each command that has run, or each named (a container brings its subcommands, `config|get` names one, an unknown name nothing): its `calls` and its `histogram_usec`, the cumulative count at each power of two microseconds from 1, as Redis's `fillCommandCDF` walks its histogram. The exporter reads it for `redis_commands_latencies_usec`. |

## CONFIG SET

`CONFIG SET` answers as Redis 8.10.1's `configSetCommand` (`src/config.c`)
does, from Redis's own table of directives, `static_configs`, which
`scripts/generate-config-specs.py` reads into `internal/core/config_specs.go`
(CI checks the file is current). Every directive is read with Redis's set
function for its type, so its errors are Redis's, word for word: `argument
must be 'yes' or 'no'`, `argument couldn't be parsed into an integer`,
`argument must be a memory value`, `argument must be between <lower> and
<upper> inclusive`, `percentage argument must be less or equal to <n>`,
`argument(s) must be one of the following: ...`, and the checks some
directives have of their own. The error-parity job compares them with Redis's.

All pairs are set, or none:

1. Every name is looked up first. The first name Redis does not know, or Keel
   does not answer for, fails the command: `Unknown option or number of
   arguments for CONFIG SET - '<name>'`. If a pair before it already failed,
   that pair's error is returned instead.
2. A directive Redis cannot set at all fails the command, whether Keel
   answers for it or not: `can't set immutable config`, or `can't set
   protected config` for `dir` and `dbfilename`, as Redis's default
   `enable-protected-configs no` keeps them. So does a directive given twice,
   under its name or its alias: `duplicate parameter`.
3. Each value is then read, in order. The first that does not read fails the
   command. The error names the directive as Redis spells it, its alias if
   that is what was set.
4. Only then is anything changed, in order. A change that fails puts back
   every change made before it.

An odd count is Redis's `syntax error`. Keel answers for these directives:

- **Keel's own settings, changed at once.**
  - `maxmemory`, `maxmemory-samples` and `lfu-log-factor`.
  - `maxmemory-policy`, for the policies Keel has: `allkeys-lru`,
    `allkeys-lfu` and `allkeys-random`. Redis's other policies are refused in
    Redis's words, naming the three: `argument(s) must be one of the
    following: allkeys-lru, allkeys-lfu, allkeys-random`.
  - The log's `appendfsync`, `auto-aof-rewrite-percentage` and
    `auto-aof-rewrite-min-size`.
  - The slow log's `slowlog-log-slower-than`, `slowlog-max-len`,
    `slowlog-entry-max-argc` and `slowlog-entry-max-string-len`. A shorter
    `slowlog-max-len` cuts the log back when the next entry joins it, as
    Redis's does.
  - With the server, three more:
    - `requirepass`. New connections, and `AUTH` on any connection, use the new
      password. A connection already logged in stays logged in, as in Redis,
      and so does one made while no password was needed.
    - `hz`. The event loop's clock changes rate at once. A value outside
      Redis's 1 to 500 is taken at its nearest end, as Redis's `updateHZ`
      takes it.
    - `maxclients`. Raising it raises the process's limit on open files to
      fit, as Redis's `adjustOpenFilesLimit` does. Where the system cannot,
      the value is refused in Redis's words: `The operating system is not able
      to handle the specified number of clients, try with <n>`.
- **Settings that only tune Redis's internals, accepted without effect.**
  Nothing a client sees depends on them, beyond `OBJECT ENCODING`, and Keel
  has nothing they would tune. They are validated as Redis validates them,
  kept, and reported by `CONFIG GET`, so that tools and Redis's own test suite
  can set them:
  - the encoding thresholds: `hash-max-listpack-*`, `list-max-listpack-size`,
    `list-compress-depth`, `set-max-intset-entries`, `set-max-listpack-*`,
    `zset-max-listpack-*` (with their `ziplist` aliases), the hash template
    settings, `hll-sparse-max-bytes`, `stream-node-max-*` and `array-*`;
  - how Redis spends memory and CPU: `activerehashing`, `activedefrag` and
    `active-defrag-*`, `jemalloc-bg-thread`, `dynamic-hz`, `lazyfree-lazy-*`,
    `lazyexpire-nested-arbitrary-keys`, `maxmemory-eviction-tenacity`,
    `lookahead` and `prefetch-batch-max-size`;
  - the format and pace of Redis's own files: `rdbcompression`,
    `rdb-save-incremental-fsync`, `aof-rewrite-incremental-fsync`,
    `rdb-del-sync-files`, `aof-use-rdb-preamble`, `aof-timestamp-enabled`,
    `no-appendfsync-on-rewrite` (Keel keeps syncing during a rewrite) and
    `sanitize-dump-payload`;
  - how Redis moves a snapshot to a replica: `repl-diskless-*`,
    `repl-rdb-channel` and `repl-disable-tcp-nodelay`;
  - the `cluster-*` settings, which change nothing on a server not in cluster
    mode, as Keel never is;
  - Redis's crash reports, log level and redaction, and the delays and
    watchdog its own tests set: `crash-log-enabled`, `crash-memcheck-enabled`,
    `use-exit-on-panic`, `hide-user-data-from-log`, `loglevel`,
    `watchdog-period`, `rdb-key-save-delay`, `key-load-delay`,
    `loading-process-events-interval-bytes` and `aof-disable-auto-gc`;
  - `busy-reply-threshold` (`lua-time-limit`), since Keel runs no scripts.
- **Settings Keel cannot change, at Keel's value.** `CONFIG SET` accepts the
  value Keel has, as Redis accepts setting a directive to the value it has,
  and refuses any other value with `can't set immutable config`:
  - `appendonly`: turning the log on or off while the server runs is not done
    yet;
  - `save` (`""`: no snapshots);
  - `notify-keyspace-events` (`""`: no keyspace notifications);
  - `maxmemory-clients` (`0`: no eviction of clients);
  - `latency-tracking` (`yes`: every command's latency is tracked) and
    `latency-tracking-info-percentiles` (Redis's default `50 99 99.9`, read
    as Redis reads numbers, so `50.0 99 99.900` is the same value), and
    `latency-monitor-threshold` (`0`: there is no latency monitor);
  - `proto-max-bulk-len` (Keel's 512 MiB, Redis's default);
  - `client-query-buffer-limit` (Keel's 16 MiB of a request not yet whole);
  - with the server, `port` and `bind`: changing the listener while the
    server runs is not done yet.

  Both values are still validated as Redis validates them first: `Invalid
  save parameters`, and `Invalid event class character`.
- **Settings Redis cannot change while it runs, at Keel's value:** `databases`
  (`1`), `appendfilename`, `dir`, `replicaof` and `slaveof`, `tcp-backlog`
  and `io-threads`. They are refused before any value is read, as Redis
  refuses them.

Every other directive in Redis's table is one whose being ignored would
change what a client relies on. Examples are `timeout`,
`client-output-buffer-limit`, `min-replicas-to-write`, `tracking-table-max-keys`,
`key-memory-histograms`, the `tls-*` and `acl*` settings, `repl-backlog-size`
and `repl-timeout`. Keel answers each as a name it does not know, and they
come one by one, with the features they configure. `CONFIG SET`'s sensitive
arguments (`requirepass`, `masterauth`) are to be redacted from the slow log
when it comes (c3c), as Redis's `redactClientCommandArgument` does.

## What each part should show in Grafana

These are the exporter metrics each part should turn on for Keel. The
Live telemetry run after each merge confirms them.

- **(a):**
  - `redis_uptime_in_seconds`, `redis_start_time_seconds`, `redis_process_id`;
  - `redis_cpu_sys_seconds_total`, `redis_cpu_user_seconds_total` and their
    `_children_` forms;
  - `redis_memory_used_peak_bytes`, `redis_memory_used_rss_bytes`,
    `redis_total_system_memory_bytes`;
  - `redis_max_clients`, `redis_blocked_clients`, `redis_pubsub_*`,
    `redis_rejected_connections_total`, `redis_cluster_enabled`,
    and the zero-valued features. (`redis_db_avg_ttl_seconds` waits for `avg_ttl`.)
- **(b):** `redis_config_maxmemory`, `redis_config_maxclients`,
  `redis_config_io_threads` and `redis_configured_hz`. Locally, `redis_exporter`
  1.93.0 read 77 metric names from Keel with (b), against 60 with (a) alone
  and 199 from Redis 8.10.2. The two `redis_config_*` metrics still missing,
  `redis_config_client_output_buffer_limit_*` and
  `redis_config_repl_backlog_size`, are the settings left out above.
- **(c1):** `redis_commands_processed_total`, `redis_total_error_replies`
  and `redis_errors_total{err}`. Locally, `redis_exporter` 1.93.0 read 80
  metric names from Keel with (c1), against 77 with (b) and 200 from Redis
  8.10.2, given the same commands and one `WRONGTYPE`. The exporter reports no
  `instantaneous_ops_per_sec` for either server.
- **(c2):** `redis_keyspace_hits_total` and `redis_keyspace_misses_total`, the
  same as Redis's for the same commands. 82 metric names from Keel, locally.
- **(c3a):** `redis_commands_total`, `redis_commands_duration_seconds_total`,
  `redis_commands_failed_calls_total` and `redis_commands_rejected_calls_total`,
  by command. 86 metric names from Keel, locally.
- **(c3b):** `redis_latency_percentiles_usec` and `redis_commands_latencies_usec`,
  by command. 92 metric names from Keel, locally. The exporter's scrape now
  draws three `ERR` replies from Keel instead of five: `SLOWLOG LEN`,
  `SLOWLOG GET 1` and `COMMAND INFO COMMANDLOG`.
- **(c3c):** `redis_slowlog_length`, `redis_slowlog_last_id`,
  `redis_last_slow_execution_duration_seconds` and
  `redis_slowlog_commands_total`. 98 metric names from Keel, locally. With
  `SLOWLOG` answered, the scrape draws one `ERR` reply from Keel, `COMMAND
  INFO`, which waits for `COMMAND` (#137, tier 1).
- **(d):** `redis_net_input_bytes_total`, `redis_net_output_bytes_total`, their
  replication forms, `redis_total_reads_processed` and
  `redis_total_writes_processed`. 104 metric names from Keel, locally, against
  200 from Redis 8.10.2. Given the same commands, each on a connection of its
  own, Keel's six are the same numbers as Redis's: 3,972 bytes in, 777 out,
  210 reads, 105 writes.
- **(i):** `redis_client_recent_max_input_buffer_bytes`,
  `redis_client_recent_max_output_buffer_bytes`, `redis_mem_clients_normal` and
  `redis_mem_clients_slaves`, and `redis_connected_clients` without the
  replicas. Locally, a Keel primary with a live replica attached and one
  client asking reported `connected_clients:2` before (i) and `1` after, as
  Redis would. With an idle client beside the one asking, both servers
  report 2; `redis_mem_clients_normal` was 2304 from Redis and 95 from Keel,
  for the reason above.
