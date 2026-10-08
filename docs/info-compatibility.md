# INFO and monitoring compatibility

Status: October 7, 2026. Part (a) is merged and part (b) is in review; (c) and (d) are planned.

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

## The work, in four pull requests

Each lands on its own, smallest risk first.

- **(a) Fields with no per-command cost.** Server identity and uptime, CPU,
  peak memory and RSS, the Clients and Keyspace details, the features Keel
  does not have, and Redis's section order. Read when `INFO` runs, or
  sampled in the cron.
- **(b) `CONFIG GET`.** Keel's real settings under Redis's parameter names,
  read-only, plus `CONFIG RESETSTAT` once (c) gives it something to reset.
- **(c) Per-command counters and timing on dispatch.** This covers:
  - `total_commands_processed` and `instantaneous_ops_per_sec`;
  - `keyspace_hits` and `keyspace_misses`;
  - `total_error_replies` and errorstats;
  - commandstats and latencystats;
  - `SLOWLOG` and `LATENCY`.

  It is the only part that adds work to every command: a counter, and two
  monotonic clock reads for the timing. It has to pass the paired
  command-path job's 0.98 rule, and the timing is measured before it ships.
  The slow log hides what Redis hides: the arguments of `AUTH`, of `HELLO`'s
  `AUTH` and of `CONFIG SET requirepass`.
- **(d) Network bytes**, counted on the connection read and write paths in
  `internal/server`.

Engine-side counters are new fields on `Engine`, after `settings`, so the hot
offsets do not move. Server-side figures (port, start time, connections
refused, network bytes) come through the hook the event loop already installs
for `INFO clients` (`SetClientBuffers`). That keeps them in `internal/server`,
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
| `connected_clients` | yes | Stays |
| `maxclients` | - | (a) `-maxclients` |
| `blocked_clients`, `total_blocking_keys`, `total_blocking_keys_on_nokey` | - | (a) `0`: no blocking commands |
| `pubsub_clients` | - | (a) `0`: no pub/sub |
| `tracking_clients` | - | (a) `0`: no client tracking |
| `cluster_connections` | - | (a) `0`: no cluster |
| `watching_clients`, `total_watched_keys` | - | (a) `0`: no `WATCH` (see the README's integration contract) |
| `client_recent_max_input_buffer`, `client_recent_max_output_buffer` | - | Left out for now. Redis's are maxima over a recent window; Keel's `retained_*` fields are totals. |
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
| `mem_clients_*`, `mem_aof_buffer`, `mem_replication_backlog`, `mem_total_replication_buffers`, `mem_cluster_*`, `mem_not_counted_for_evict`, `mem_overhead_db_hashtable_rehashing` | - | Left out for now. Each needs an accounting Keel does not keep in Redis's terms. |
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
| `total_commands_processed`, `instantaneous_ops_per_sec` | - | (c). Ops per second is averaged over the last 16 samples taken every 100 ms, as Redis does. |
| `keyspace_hits`, `keyspace_misses` | - | (c) Counted on the key lookups of read commands, per key, as Redis's `lookupKeyRead` counts them |
| `total_error_replies` | - | (c) Every error reply, refusals included |
| `total_net_input_bytes`, `total_net_output_bytes`, `instantaneous_input_kbps`, `instantaneous_output_kbps` | - | (d) |
| `total_net_repl_input_bytes`, `total_net_repl_output_bytes`, `instantaneous_*_repl_kbps` | - | (d), on the replication connections |
| `rejected_connections` | - | (a) Connections refused at `-maxclients` |
| `total_reads_processed`, `total_writes_processed` | - | (d) Socket reads and writes that moved bytes |
| `slowlog_commands_count`, `slowlog_commands_time_ms_max`, `slowlog_commands_time_ms_sum` | - | (c) |
| `pubsub_channels`, `pubsub_patterns`, `pubsubshard_channels`, `tracking_total_*`, `evicted_clients`, `evicted_scripts` | - | (a) `0` |
| `total_forks`, `latest_fork_usec` | - | (a) `0`: Keel never forks |
| `sync_full`, `sync_partial_ok`, `sync_partial_err` | - | Left out until Keel's replication protocol is mapped onto Redis's terms |
| `expired_subkeys*`, `expired_keys_active`, `expired_stale_perc`, `expired_time_cap_reached_count`, `expire_cycle_cpu_milliseconds`, `total_eviction_exceeded_time`, `current_eviction_exceeded_time` | - | Left out for now. Each is an expiry or eviction internal that needs its own measurement. |
| `active_defrag_*`, `io_threaded_*`, `eventloop_*`, `avg_pipeline_length*`, `client_*_buffer_limit_disconnections`, `reply_buffer_*`, `unexpected_error_replies`, `dump_payload_sanitizations`, `hash_template*`, `acl_access_denied_*`, `migrate_cached_sockets`, `slave_expires_tracked_keys` | - | Left out (rule 3), or until Keel has the feature |

## Replication

Keel reports its own replication fields (`role:primary`, `primary_offset`, the
`replication_*` and `failover_*` lines). Redis reports `role:master` or
`role:slave`, `connected_slaves`, `master_repl_offset` and `master_replid`.
Exporters and dashboards key on Redis's `role` values, so this needs a
decision of its own, and none of (a) to (d) changes it.

## CPU

| Redis field | Plan |
| --- | --- |
| `used_cpu_sys`, `used_cpu_user` | (a) `getrusage(RUSAGE_SELF)`, in seconds with six decimals, as Redis prints them |
| `used_cpu_sys_children`, `used_cpu_user_children` | (a) `getrusage(RUSAGE_CHILDREN)`, which is `0.000000` for Keel |
| `used_cpu_sys_main_thread`, `used_cpu_user_main_thread` | Left out. Redis reports them only where it can read a thread's own usage, and Keel's event loop is a goroutine. |

## Keyspace

`db0:keys=N,expires=N,avg_ttl=N,subexpiry=N`. Keel reports `keys` and
`expires`. `avg_ttl` is Redis's estimate in milliseconds of the TTL of keys
that have one, taken from the active expiry cycle's samples. It waits for a
later part, because Keel's keyspaces report how many keys a sample examined
and removed, not their TTLs. `subexpiry=0` comes with it, since Keel has no
hash field expiry.

## Commandstats, Errorstats and Latencystats, all (c)

- **Commandstats:**
  `cmdstat_<name>:calls=N,usec=N,usec_per_call=N.NN,rejected_calls=N,failed_calls=N`.
  - `<name>` is lower case, with `|` before a subcommand (`cmdstat_client|list`).
  - A refusal before the command runs (arity, `NOAUTH`, a replica's
    `READONLY`) counts as `rejected_calls`.
  - An error from the command itself counts as `failed_calls`.
- **Errorstats:** `errorstat_<PREFIX>:count=N`, where the prefix is the
  error's first word (`ERR`, `WRONGTYPE`, `NOAUTH`). Like Redis, it tracks at
  most 128 prefixes.
- **Latencystats:** `latency_percentiles_usec_<name>:p50=N,p99=N,p99.9=N`,
  from a histogram per command. Redis keeps these with `latency-tracking`
  on, its default.

## Commands

| Command | Plan |
| --- | --- |
| `CONFIG GET pattern [pattern ...]` | (b), done. Keel's settings under Redis's names, read live: `maxmemory`, `maxmemory-policy`, `maxmemory-samples`, `lfu-log-factor`, `appendonly`, `appendfilename`, `appendfsync`, `auto-aof-rewrite-percentage`, `auto-aof-rewrite-min-size`, `replicaof` and its old name `slaveof` (`host port`, as Redis writes it), `databases` (`1`), `save` (`""`, no snapshots) and `dir` (the working directory, against which relative paths resolve). With the server, also `port`, `bind`, `maxclients`, `tcp-backlog` (Keel listens with a backlog of `maxclients`), `io-threads`, `hz` and `requirepass`, which Redis gives to any client that has logged in. The slow log and latency settings come with (c). A RESP2 flat array, or a RESP3 map. A name matches without regard to case and comes back as asked; a glob comes back in Redis's spelling. Settings whose meaning differs are left out: `lfu-decay-time` against Keel's access-counted decay; `client-output-buffer-limit`, since Keel's 64 MiB `MaxReplyBytes` caps one reply rather than disconnecting a client that falls behind; and `repl-backlog-size`, since a Keel replica reads the primary's log, not a backlog. |
| `CONFIG SET parameter value [parameter value ...]` | (b), done. Keel has no runtime configuration yet, so every pair is refused in Redis's words for the first that fails: `Unknown option or number of arguments for CONFIG SET - '<name>'` for a name Keel does not report, `CONFIG SET failed (possibly related to argument '<name>') - can't set immutable config` for one it does, and `can't set protected config` for `dir`, as Redis keeps it by default. An odd count is Redis's `syntax error`. |
| `CONFIG REWRITE` | (b), done. `The server is running without a config file`, Redis's answer when it was started without one: Keel takes flags. |
| `CONFIG RESETSTAT` | (c). Resets what Redis resets: commandstats, errorstats, latencystats, hits and misses, error replies, the connection and network counters, and the expired and evicted counts. |
| `SLOWLOG GET [count]`, `LEN`, `RESET`, `HELP` | (c). Entries are id, Unix time, duration in µs, arguments (at most 32, each cut at 128 bytes as Redis does), client address and name. Defaults are `slowlog-log-slower-than` 10000 and `slowlog-max-len` 128. |
| `LATENCY LATEST`, `HISTORY`, `RESET`, `DOCTOR`, `HELP` | (c). Redis's latency monitor is off by default (`latency-monitor-threshold 0`), so these answer as an idle monitor: empty replies and `0`. |
| `LATENCY HISTOGRAM [command ...]` | (c), from the latencystats histograms. The exporter reads it for `redis_commands_latencies_usec`. |

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
- **(c):**
  - `redis_commands_processed_total`, `redis_keyspace_hits_total`,
    `redis_keyspace_misses_total`, `redis_total_error_replies`;
  - `redis_commands_total`, `redis_commands_duration_seconds_total`,
    `redis_commands_failed_calls_total`, `redis_commands_rejected_calls_total`,
    `redis_errors_total`;
  - `redis_latency_percentiles_usec`, `redis_commands_latencies_usec`;
  - `redis_slowlog_length`, `redis_slowlog_last_id`,
    `redis_last_slow_execution_duration_seconds`.
- **(d):** `redis_net_input_bytes_total`, `redis_net_output_bytes_total`, and
  their replication forms.
