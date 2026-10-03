# Application pilot: Gitea on Keel

The first application pilot runs a real, unmodified application with its
cache, web sessions and job queues on Keel, and the same application with
identical settings on Redis. It answers the beta's "application pilot" item
with a running workload rather than a benchmark: which commands a real
application sends, whether it works, what it costs in latency and memory, and
what a server crash costs it.

Gitea is the pilot. It is widely deployed (58k GitHub stars), keeps all three
subsystems in Redis when configured to, and its go-redis client falls back to
RESP2 by itself when `HELLO 3` is refused, so nothing in Gitea has to change.
Its global lock (redsync, which runs Lua scripts) and its websocket broker
(Pub/Sub) stay in memory, because Keel has neither feature; the regression
suite records exactly how those two fail.

## What the workflow runs

[`application-pilot.yml`](../.github/workflows/application-pilot.yml) runs on
free GitHub-hosted `ubuntu-24.04` runners, on `workflow_dispatch` and on pull
requests that touch it or [`bench/app-pilot/`](../bench/app-pilot). Every
outside project is pinned by exact commit in
[`pins.json`](../bench/app-pilot/pins.json).

Both arms are configured identically, with every setting spelled out on the
command line and Redis's value used wherever Keel offers the same knob:

| Setting | Keel arm | Redis arm |
| --- | --- | --- |
| Server | built from the checked-out commit | Redis 8.10.2 core, built from its SHA-256-checked release tarball |
| Persistence | `-appendonly -appendfsync everysec` | `appendonly yes`, `appendfsync everysec`, RDB snapshots off |
| AOF rewrite | `-auto-aof-rewrite-percentage 100 -auto-aof-rewrite-min-size 64mb` | the same, Redis's defaults |
| Memory | `-maxmemory 256mb -evict lru -lru-samples 5` | `maxmemory 256mb`, `allkeys-lru`, `maxmemory-samples 5` |
| Clients and threads | `-maxclients 10000 -io-threads 1` | `maxclients 10000`, `io-threads 1` |
| Background work | `-cron-interval-ms 100 -active-expire-samples 20` | `hz 10`, `active-expire-effort 1` |
| Address | 127.0.0.1:16379 behind the wire tap on :6379 | the same |

Keel's own default of 20,000 clients is replaced by Redis's 10,000. Eviction
is the one place Redis's default (`noeviction`) cannot be followed, because
Keel has no such policy; both arms evict by sampled LRU over all keys, and the
pilot's keyspace stays far below the limit.

Both arms of the application run on one runner, one after the other, so a
difference in CPU or disk between hosted machines cannot appear as a
difference between the servers. Which arm runs first alternates with the run
number, and each arm's result records the host it ran on and its position.

The wire tap ([`resp_tap.py`](../bench/app-pilot/resp_tap.py)) forwards bytes
unchanged and logs every command, its options, and every error reply, on both
arms. Keel has no `MONITOR`, so this is the only way to see an application's
real command mix, including the handshake its client sends on each connection.

**Regression suites**, unmodified, on each arm:

| Suite | Pinned commit | Gates the job |
| --- | --- | --- |
| Gitea `modules/cache`, `modules/queue`, `modules/session` Redis tests | `986b0bcae0d3` (main, 2026-10-02) | yes |
| go-redis/cache, all tests | `b8aec2d93201` | yes |
| gin-contrib/sessions `./redis` | `30faf8469f77` | yes |
| Gitea `modules/globallock` and `services/pubsub` Redis tests | `986b0bcae0d3` | no: they record how Lua and Pub/Sub fail |

Gitea's own test helper refuses to skip a Redis test in CI, so a missing server
fails rather than passes. The release v28.0.0 has no Redis tests for its cache
or sessions; main's are used, and the release runs as the application.

**Application**: the real `gitea web` from release v28.0.0 (commit
`15b8a5805adf`), built with Gitea's own `make build` and embedded assets,
SQLite, `INSTALL_LOCK`, cache, sessions and queues on the server under test.
[`gitea_pilot.py`](../bench/app-pilot/gitea_pilot.py) then:

1. creates an administrator and six users, and signs each one in through the
   web form, so every later page view reads a server-side session;
2. creates two repositories per user with a webhook to a local sink, pushes
   commits over HTTP, opens issues through the API and comments through the
   web form;
3. browses dashboards, repositories, issues, commit lists, files and issue and
   code searches repeatedly, so cached values are read back and the indexers'
   queues are used;
4. under mixed browsing and writing load, kills the server with SIGKILL,
   leaves it down for three seconds, restarts it from its append-only file and
   checks whether each user's pre-crash session still signs them in;
5. pushes again, browses again and waits for every queued webhook delivery.

It records each request's latency and status, server and Gitea RSS every half
second, `INFO` at four points, the command mix, and a separate probe that
writes acknowledged `SET`s straight to the server until the kill and counts
which survive. Webhooks are the end-to-end check on the queues: every push,
issue and comment should produce exactly one delivery.

An arm fails if the harness errors, a login fails, any request fails outside
the restart window, a session does not survive the restart, a webhook from
outside the restart window never arrives, or the application uses an
unsupported command other than the client probes go-redis survives by design
(`HELLO`, `CLIENT SETINFO`, `CLIENT MAINT_NOTIFICATIONS`, `COMMAND`). Lost
probe writes and deliveries lost inside the restart window are measurements,
not failures. Each job uploads its raw results even when it fails, and the
summary job always runs.

The restart is a process crash, not a power cut. Data already handed to the
kernel survives SIGKILL, so the probe measures what each server writes before
it replies; it cannot measure what `everysec` loses to an operating-system
crash, which can be up to about a second of writes on either server.

## Hosted results

Run [37088056992](https://github.com/brandopakel/keel/actions/runs/37088056992)
tested Keel at the pull request's merge commit `30a0d1d` (binary SHA-256
`9e5c9ccd…3edfbfbc`) against Redis 8.10.2 (binary `40ece39a…b9538a0ba`, from
the tarball checked against `b9ffee22…c02b3`), with Gitea 28.0.0 built from
`15b8a5805adf` (binary `37758935…ef21737`). Every job passed. Both application
arms ran on the same runner (`runnervm8df0l`, 4 vCPU, Ubuntu 24.04), Redis
first, with the command lines in the settings table above.

| Measure | Keel | Redis |
| --- | --- | --- |
| Gating regression suites | all pass: Gitea 3 tests / 44 subtests, go-redis/cache 4 tests / 45 Ginkgo specs, gin-contrib/sessions 8 tests / 1 subtest | identical counts |
| Known-unsupported Gitea tests | 14/23 subtests; fail on `EVALSHA` (lock) and `SUBSCRIBE`, `PUBLISH`, `PUBSUB NUMSUB` (broker) | 23/23 |
| Requests; errors outside the restart window | 3,720; 0 | 3,344; 0 |
| Errors inside the restart window | 23 | 20 |
| Steady GET latency p50 / p95 / p99 | 37.5 / 102.5 / 169.8 ms | 38.3 / 111.8 / 242.1 ms |
| Server RSS, peak | 17.4 MiB | 11.9 MiB |
| Server `used_memory` at the end | 0.99 MB (8.4k keys) | 1.66 MB |
| Gitea RSS, peak | 262 MiB | 257 MiB |
| Kill to PING; of which restart to PING | 3.026 s; 0.021 s (1.51 MB AOF) | 3.021 s; 0.016 s (1.17 MB AOF) |
| Last signed-in page working after PING | 0.018 s | 0.017 s |
| Sessions surviving SIGKILL | 6/6 | 6/6 |
| Acknowledged probe writes lost (RPO) | 0 of 8,557 (0 ms) | 0 of 8,035 (0 ms) |
| Webhooks lost, all inside the restart window | 2 of 318 | 3 of 281 |
| Unsupported commands | `HELLO` ×38, `CLIENT SETINFO` ×76, both handled by go-redis | `CLIENT MAINT_NOTIFICATIONS` ×3, likewise |

Gitea's command mix was the same on both arms: `EXISTS`, `GET`, `SET` (with
`EX`), `LPOP`, `RPUSH`, `LLEN`, `SADD`, `SREM`, `HSET`, `HDEL`, `DEL` and
`PING`. The load phase is timed, so Keel's arm served more requests in it.
At p50 every page view was within about 3 ms between the arms; writes varied
more (web comments 93 vs 80 ms, `git push` 804 vs 784 ms). At p99 Keel was
lower on most pages and Redis on `git push` and API issue creation. That is
one run on one machine, with Keel second, so treat the tail differences as
indicative only.

Both servers started at about 11.6 MiB RSS. Redis peaked at 11.9 MiB; Keel
grew to 17.4 MiB under load and stayed near it. The `used_memory` figures are
not comparable: Keel's counts keyspace storage only, while Redis's includes its
own buffers and overhead.

So the pilot's recovery numbers are, for a process crash: RTO about 20 ms
from restart to serving, and about 20 ms more until every signed-in page
worked, with RPO zero acknowledged writes. Both servers write to the log
before replying, so a SIGKILL loses nothing they acknowledged; an
operating-system crash under `everysec` would still lose up to about a second
on either.

Run [37073427133](https://github.com/brandopakel/keel/actions/runs/37073427133)
(Keel at the pull request's merge commit `c33fb57`, binary SHA-256
`507736d2…ab876c5`; Redis 8.10.2 binary `a0642818…a16758f`; Gitea 28.0.0
binary `37758935…ef21737`) passed every job too. Its two application arms ran on
separate runners at the same time, before the arms were moved onto one
machine and before every server setting was made explicit, so its latencies
compare two hosts as much as two servers. Its correctness and restart results
agree with the later run:

| Measure | Keel | Redis |
| --- | --- | --- |
| Gating regression suites | all pass: Gitea 3 tests / 44 subtests, go-redis/cache 4 tests / 45 specs, gin-contrib/sessions 8 tests | identical counts |
| Known-unsupported Gitea tests | fail on `EVALSHA` (lock) and `SUBSCRIBE`, `PUBLISH`, `PUBSUB NUMSUB` (broker): 14/23 subtests pass | 23/23 pass |
| Requests, errors outside the restart window | 3,227, 0 | 2,733, 0 |
| Sessions surviving SIGKILL | 6/6 | 6/6 |
| Acknowledged probe writes lost | 0 of 8,247 | 0 of 7,642 |
| Kill to PING, of which restart to PING | 3.03 s, 0.027 s (1.36 MB AOF) | 3.09 s, 0.085 s (1.00 MB AOF) |
| Last signed-in page working after PING | 0.076 s | 0.043 s |
| Webhooks lost, all inside the restart window | 1 of 267 | 2 of 219 |
| Server RSS, peak | 16.3 MiB | 13.6 MiB |
| Unsupported commands | `HELLO`, `CLIENT SETINFO` (handled by go-redis) | `CLIENT MAINT_NOTIFICATIONS` (likewise) |

In both runs the lost webhooks were Gitea's own: an issue created as the
server died, or just after it returned on a pooled connection that had died
with it, logged `PrepareWebhooks: EOF` and never queued its delivery; a few
issue-indexer updates were dropped the same way. That happened on both arms.
Redis also answered a few commands with `LOADING` while it replayed its log
after the restart (seven in the earlier run, one in the later); Keel accepts
connections only once replay is complete.

That run's summary also showed Ginkgo's pass count as unknown: `go test
-json` drops the escape byte of Ginkgo's colour codes, which the parser did not
expect. The spec totals (45 of 45 ran) were in its raw output, and the parser
was fixed before the later run.

The first run, [37072819250](https://github.com/brandopakel/keel/actions/runs/37072819250),
failed before any test: Redis 8's top-level Makefile also builds the bundled
modules, and the search module failed to compile on the runner. The pilot now
builds the core server from `src/` alone.

## Candidate survey, 2026-10-02

Projects were judged on whether their Redis use fits Keel's subset, whether
their own test suite or a reproducible workload gives an objective result,
recognition and maintenance, and whether they run on free runners at no cost.
Commands were read from source and then confirmed on the wire.

| Rank | Project | Client | Fit on the exercised path |
| --- | --- | --- | --- |
| 1 | [Gitea](https://github.com/go-gitea/gitea) | go-redis 9.22.0 | cache, session, queue: all supported; Redis global lock (Lua) and Redis websocket broker (Pub/Sub): not supported |
| 2 | [go-redis/cache](https://github.com/go-redis/cache) | go-redis 9.0.5 | all supported; unmaintained since 2024-06 |
| 3 | [gin-contrib/sessions](https://github.com/gin-contrib/sessions) and [boj/redistore](https://github.com/boj/redistore) | redigo 1.9.3 | all supported; one redistore test selects database 1 |
| 4 | [connect-redis](https://github.com/tj/connect-redis) | node-redis 6.3.0 | data commands all supported; the client's `HELLO 3` is not, and it has no fallback |
| 5 | [rack-attack](https://github.com/rack/rack-attack) | redis-rb 5 | plain Redis store supported (from source, not run: local Ruby is 2.6); its Rails RedisCacheStore path needs `UNLINK` and an `INFO` `redis_version` line |

Excluded: cachelib and Flask-Caching (redis-py 8 `HELLO 3`, and `SETNX` in
`add`), keyv (`UNLINK` on every delete, `MULTI`/`EXEC`), django-redis
(`SELECT`, `EVAL`, set algebra), dogpile.cache (transactional pipelines and Lua
locks), aiocache (moved to valkey-glide), eko/gocache (`FLUSHALL`, mocked
tests), requests-cache (`HSCAN`), YCSB (`HMSET` on every write) and
memtier_benchmark (already used by `bench/run-memtier.sh`).

### Client protocol defaults

The largest compatibility factor was not a data command but the client's
first request. The newest default clients ask for RESP3 with `HELLO 3`:

| Client | When `HELLO 3` is refused | Unmodified on Keel |
| --- | --- | --- |
| node-redis 6.3.0 | no fallback; the connection fails | no |
| redis-py 8.1.0 | no fallback; the connection fails | no |
| go-redis 9.0.5 and 9.22.0 | falls back to RESP2; ignores refused `CLIENT SETINFO` | yes |
| redis-rb 6.0.0 (redis-client) | reconnects with RESP2 on `unknown command 'HELLO'` (from source) | yes |
| redigo 1.9.3, redis-rb 5, node-redis 5.12.1, redis-py 7.4.1 | never send `HELLO` | yes |

Replying `NOPROTO` would not help node-redis 6 or redis-py 8, which have no
fallback path at all. Separately, Keel's `INFO` has no `redis_version`; keyv
tolerates that, but Rails' RedisCacheStore would raise on every counter with an
expiry.

### Local trial counts

Every run used each project's own test command unmodified, through
`scripts/run-local-validation.py`, against Keel `acb547b` (binary SHA-256
`3c9b8247…f1d1c`) and Homebrew Redis 8.10.1 with persistence off. A
`redis-server` PATH shim started the backend and put the wire tap on the port
each suite expects. Variant runs changed only the client version, within the
project's declared range.

| Project, client | Redis | Keel | What Keel refused |
| --- | --- | --- | --- |
| Gitea `TestStringCacheAdapters` + `TestBaseRedis`, go-redis 9.22.0 | 2/2 tests, 7/7 subtests | 2/2, 7/7 | `HELLO`, `CLIENT SETINFO`; both handled by the client |
| go-redis/cache, go-redis 9.0.5 | 45/45 specs, 4/4 tests | 45/45, 4/4 | `HELLO`, `COMMAND`; both handled |
| gin-contrib/sessions `./redis`, redigo | 8/8 | 8/8 | nothing |
| boj/redistore v2, redigo | 32/32, 12/12 subtests | 31/32, 11/12 | `SELECT 1` in `TestRediStore/Round_7` |
| connect-redis, node-redis 6.3.0 | 4/4 | 2/4 (`defaults`, `redis`) | `HELLO`, before any data command |
| connect-redis, node-redis 5.12.1 | 4/4 | 4/4 | `CLIENT SETINFO`, ignored |
| cachelib, redis-py 8.1.0 | 100/100 | 8/100 (the 8 never connect) | `HELLO` |
| cachelib, redis-py 7.4.1 | 100/100 | 92/100 | `SETNX` in `test_add` and `test_serializer_injection_add` (four parameters each) |

Failed attempts, kept because a failed run is a result:

- connect-redis on Redis, first attempt: the harness skipped `npm run build`,
  so the suite found no module and ran nothing.
- gin-contrib/sessions, first attempt on both backends: downloading the
  module graph exceeded the wrapper's 512 MiB output budget (peak 578 MB).
- gin-contrib/sessions on Redis, second attempt: `proxy.golang.org` returned an
  HTTP/2 `INTERNAL_ERROR` during module download (`[setup failed]`).
- The pilot harness's first unit-test run failed on an inline-command parsing
  bug, fixed before any hosted run.

The harness was also run locally at reduced size (two users) against the
Gitea v28.0.0 release binary on both backends: every check passed on both, both
sessions survived the crash, and no acknowledged probe write was lost.

Per-run counts, wrapper reports, command mixes and a SHA-256 manifest of every
evidence file are in
[`bench/results/application-pilot-local-2026-10-02.json.gz`](../bench/results/application-pilot-local-2026-10-02.json.gz).
(SHA-256 `1fd3668f3a4ba8b6deb65616457e7314fc9e1d610f2964ea948df504b028caa6`).
The raw directories were deleted after this publication; the clones,
`node_modules`, virtual environments, module caches and binaries were never
kept.

## Not covered

- Operating-system crash or power loss, which `everysec` does not protect on
  either server.
- Gitea features configured to use Redis Lua scripts or Pub/Sub, and Gitea's
  cluster mode.
- Long-running behaviour: the pilot runs for minutes, not days.
- Clients that require RESP3; rack-attack and Rails' RedisCacheStore, which need
  a Ruby toolchain this machine lacks.
