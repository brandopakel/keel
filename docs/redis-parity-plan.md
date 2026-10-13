# Redis parity plan

Status: agreed, October 9, 2026. The embedding plan's session agreed on
October 7, and the owner decided the open question, the sequencing, on
October 9: parity interleaves with phase 4 (see below).

The owner decided on October 7, 2026 that Keel implements everything Redis
has. The README's integration contract lists, as absent:

- Lua and Pub/Sub;
- blocking list commands and `WATCH`;
- RESP3 push messages and client tracking;
- ACL roles, native TLS and cluster routing.

Those boundaries come down, one piece at a time. Each piece updates the
contract when it lands.

The owner set the order. Grafana is set up first, and the owner learns it on
Keel's own data. Meanwhile three things go ahead:

- the monitoring fields (`docs/info-compatibility.md`);
- the command census (#136);
- the framework checks below.

Parity work starts after that, in the order below, around the embedding
plan's remaining phases.

## Where Keel stands

The command census (`scripts/command-census.py`, #136) asks a running Redis
for every command and subcommand in `COMMAND DOCS`, then asks Keel for each.
Against Redis 8.10.2 on October 7, 2026, Keel had **89 of 301 commands and
9 of 148 subcommands**. That count is core plus vector sets; Bloom, JSON and
TimeSeries come in CI.

| Area | Keel has | Missing |
| --- | --- | --- |
| string | 12/26 (26 since steps 4.3c and 4.3d of the embedding plan) | ~~`APPEND`, `GETRANGE`, `SETRANGE`, `STRLEN`, `GETDEL`, `GETEX`, `GETSET`, `SUBSTR`, `INCRBYFLOAT`, `MSETNX`~~ (step 4.3c), and 8.x's ~~`DELEX`, `DIGEST`, `INCREX`, `MSETEX`~~ (step 4.3d, with `SET`'s `IFEQ`, `IFNE`, `IFDEQ` and `IFDNE`) |
| list | 9/24 | `LPOS`, `LINSERT`, `LREM`, `LMOVE`, `LMOVEM`, `RPOPLPUSH`, `LPUSHX`, `RPUSHX`, `LMPOP`, and the blocking forms |
| hash | 11/29 | `HSTRLEN`, `HRANDFIELD`, `HINCRBYFLOAT`, `HMSET`, `HSCAN`, `HGETDEL`, `HGETEX`, `HSETEX`, `HIMPORT`, field expiry (`HEXPIRE` family, 8 commands) |
| set | 8/19 | `SINTER`, `SUNION`, `SDIFF` with `STORE` and `CARD` forms, `SMOVE`, `SSCAN` |
| sorted-set | 12/35 | `ZUNION`, `ZINTER`, `ZDIFF` with `STORE` forms, the lex ranges, `ZREMRANGEBY*`, `ZREVRANGE`, `ZREVRANK`, `ZMSCORE`, `ZRANDMEMBER`, `ZRANGESTORE`, `ZMPOP`, `ZSCAN`, and the blocking forms |
| generic | 13/29 (18 since step 4.4 of the embedding plan) | ~~`RENAME`, `RENAMENX`~~, `COPY`, ~~`TOUCH`~~, `OBJECT`, `RANDOMKEY`, ~~`EXPIRETIME`, `PEXPIRETIME`~~ (step 4.4), `SORT`, `SORT_RO`, `DUMP`, `RESTORE`, `MOVE`, `MIGRATE`, `WAIT`, `WAITAOF` |
| server | 5/32 | `CONFIG`, `COMMAND`, `TIME`, `ROLE`, `FLUSHALL`, `SWAPDB`, `SLOWLOG`, `LATENCY`, `MONITOR`, `ACL`, `DEBUG`, `SAVE`, `BGSAVE`, `LASTSAVE`, `REPLICAOF`, `FAILOVER`, `SHUTDOWN`, `MODULE`, `SYNC`, `PSYNC`, `REPLCONF`, and others |
| geo, hyperloglog, connection, transactions | 19/28 | the `GEORADIUS` family, `GEOSEARCHSTORE`, `PFDEBUG`, `PFSELFTEST`, `RESET`, `WATCH` |
| bitmap | 0/7 | all |
| pubsub | 0/9 | all |
| scripting | 0/8 | all: `EVAL`, `EVALSHA`, `SCRIPT`, `FUNCTION`, `FCALL` |
| stream | 0/20 | all |
| cluster | 0/4 | all |
| array (8.x) | 0/18 | all |
| vector sets (8.x) | 0/13 | all |

The name census checks names, not options. The **argument census**
(`scripts/argument-census.py`, in the same workflow) checks the options: for
every command Keel has, every argument form Redis's own specifications
document (`src/commands/*.json` in the Redis source: each optional argument,
each alternative of each choice, each repeated argument given twice, and so
each option Redis 8 added) is sent to Keel and to Redis, and the replies are
compared. The first CI run, against Redis 8.10.1 (run 38302176673, October
12, 2026; a local run against 8.10.2 agreed), found Keel accepting **207 of
the 217 forms** of its 100 commands and subcommands, with 10 refused and none
answered differently:

| Area | Commands | Forms | Accepted | Refused | Refused forms |
| --- | ---: | ---: | ---: | ---: | --- |
| connection | 12 | 18 | 18 | 0 | |
| generic | 13 | 35 | 35 | 0 | |
| geo | 5 | 32 | 32 | 0 | |
| hash | 11 | 14 | 14 | 0 | |
| hyperloglog | 3 | 8 | 8 | 0 | |
| list | 9 | 13 | 13 | 0 | |
| server | 11 | 18 | 16 | 2 | `CONFIG SET` (parsed, then refused: no setting is writable) |
| set | 8 | 13 | 13 | 0 | |
| sorted_set | 12 | 32 | 28 | 4 | `ZADD GT`, `LT`, `INCR`; `ZRANGE BYLEX` |
| string | 12 | 30 | 26 | 4 | `SET IFEQ`, `IFNE`, `IFDEQ`, `IFDNE` (8.4) |
| transactions | 4 | 4 | 4 | 0 | |

Of the four forms Redis 8 added to commands Keel has, none is accepted. The
forms a command has are limited by what Keel has: `ZRANGE` without `BYLEX`
also lacks the lexical bounds, `SET` without `IFDEQ` the digest conditions.
Module commands have no specification in `src/commands` and are covered by
`scripts/redisbloom-parity.py` instead. Every run's counts go to
`census/argument-runs.csv` and `census/argument-areas.csv` on the
`bench-history` branch, and every form of the newest run of `develop`, with
both replies, to `census/arguments.csv`.

The **reply census** (`scripts/reply-census.py`, same workflow) checks the
shape of what Keel answers: the same forms, and each command's base form with
missing keys, with a key of another type and with an argument too many, each
in RESP2 and in RESP3, with the reply's protocol types compared to Redis's,
and each RESP3 reply checked against the `reply_schema` Redis keeps for the
command and validates its own replies with. The first CI run, against Redis
8.10.1 (run 38305863905, October 12, 2026; a local run against 8.10.2
agreed), found Keel's replies with Redis's shape in **886 of 906 cases**, the
other 20 being the refused forms above; none differed. Two of Keel's RESP3
replies violate the schema:

- `MEMORY STATS` answers Keel's own fields (`keyspace.bytes`, `keys.count`,
  `string.bytes` and so on). Redis's schema (`src/commands/memory-stats.json`,
  `additionalProperties: false`) lists Redis's allocator fields and allows no
  others; `memoryCommand` in `src/object.c:1808` builds them. A client reading
  Redis's fields finds none. This is a wrong reply, not a shape one, and
  joins tier 1's server commands.
- `GEOHASH geo BOGUS` answers `[null]`, exactly as Redis does
  (`geohashCommand`, `src/geo.c:893`, `addReplyNull` for a missing member).
  Redis's own schema (`src/commands/geohash.json`) says every item is a
  string, so Redis violates it too: a flaw in the schema, not in either
  server, kept here so the count is understood.

Every run's counts go to `census/reply-runs.csv` and `census/reply-areas.csv`
on `bench-history`, and every case of the newest run of `develop`, with both
replies and where in the Redis source the reply is built, to
`census/replies.csv`.

### Configuration

The **configuration census** (`scripts/config-census.py`, same workflow)
reads Redis's configuration table (`static_configs` in `src/config.c`) and
`redis.conf`, asks Redis and Keel `CONFIG GET` for each directive, and
checks Keel's flags. The first CI run, against Redis 8.10.1 (run
38305867780, October 12, 2026; a local run against 8.10.2 agreed): of the
**216 directives** this build of Redis has, Keel reports **20** under
Redis's name through `CONFIG GET`, has a flag for **5** more under another
meaning, and lacks **191**.

| Family | Directives | Has | Mapped | Lacks |
| --- | ---: | ---: | ---: | ---: |
| clients and network | 23 | 5 | 1 | 17 |
| cluster | 26 | 0 | 0 | 26 |
| encodings | 22 | 0 | 0 | 22 |
| logging and process | 19 | 0 | 0 | 19 |
| memory and eviction | 26 | 5 | 2 | 19 |
| monitoring | 9 | 0 | 0 | 9 |
| other | 8 | 1 | 0 | 7 |
| persistence | 27 | 7 | 0 | 20 |
| replication | 27 | 1 | 2 | 24 |
| scripting | 1 | 0 | 0 | 1 |
| security | 6 | 1 | 0 | 5 |
| tls | 22 | 0 | 0 | 22 |

The 20 Keel has are those `CONFIG GET` reports (`docs/info-compatibility.md`).
The 5 mapped: `lfu-decay-time` (`-lfu-decay-period`, by accesses rather than
idle minutes), `active-expire-effort` (`-active-expire-samples`),
`masterauth` (`-primary-password-env`), `shutdown-timeout` (a grace period
for cleanup rather than for replicas) and `protected-mode` (no mode: Keel
listens on loopback unless `-host` says otherwise).

Which of the lacking ones matter is measured two ways. First, who reads or
sets a directive, checked against the sources on October 12, 2026:
`redis_exporter` reads `CONFIG GET *` and makes metrics of `maxmemory`,
`maxclients`, `io-threads`, `databases` (all of which Keel has),
`repl-backlog-size` and `client-output-buffer-limit` (which it lacks);
Sidekiq and BullMQ warn unless `maxmemory-policy` is `noeviction`, read from
`INFO`, which Keel reports; the Bitnami chart sets `appendonly yes` and
`save ""` (Keel has both); the spotahome operator writes `port`, `save`,
`slaveof`, `requirepass`, `masterauth` and `tcp-keepalive`; the official
Docker image's docs show `--save` and `--loglevel`. So the lacking
directives an operator meets first are **`loglevel`, `tcp-keepalive`,
`repl-backlog-size` and `client-output-buffer-limit`**. Second, Redis's own
test suite (the oracle below) sets 125 of the lacking directives in at least
one test file; the most set are `rdb-key-save-delay` (17 files, a test-only
delay), `hash-max-listpack-entries` (12, an encoding threshold Keel has no
counterpart to), `aof-use-rdb-preamble` (11), `repl-diskless-sync-delay`
(9) and `client-query-buffer-limit` (8). A test that sets a directive Keel
lacks fails at `CONFIG SET`, whatever it then checks.

The whole list, with Redis's defaults and Keel's values, is in the
workflow's `config.md`; every run's counts go to `census/config-runs.csv` on
`bench-history` and every directive of the newest run of `develop` to
`census/config.csv`.

### Redis's own tests

The **Redis tests** workflow (`.github/workflows/redis-tests.yml`, weekly,
on dispatch and on a pull request that changes it) runs Redis 8.10.1's own
test suite against Keel and against Redis, unit by unit. Its first baseline
(October 13, 2026: runs 38335449455, 38337899874 and 38343833581 on its pull
request, and 38352790820, the first on `develop`, the same unit by unit)
found Keel passing **206 of the 3,194 tests Redis passes (6.4%)**, with
1,241 failing and 1,663 never reached. Most failures are commands Keel
lacks: 1,008 of the 1,241, led by `XADD`, `FUNCTION`, the array commands,
`INCREX`, `FLUSHALL` and `COMMAND`. Most of the tests never reached sit in
17 units whose setup runs a `CONFIG SET` Keel refuses. Redis passes 1,780
tests in those units, and Keel reaches 333 of them before the refusal. Every
run goes to `census/redis-tests.csv` on `bench-history`.

**What the tests rank.** Each failing test is counted once, under the first
thing Keel lacked in its error. A test may need more than that, so each
count is an upper bound on what that step alone unlocks, not a forecast.
The counts are from run 38352790820 on `develop` (38343833581 gives the
same), by Redis's command groups (`src/commands/*.json`).

| Step | Tests behind it | Detail |
| --- | --- | --- |
| `CONFIG SET` accepts what the units set | 1,447 never reached, 72 failing | See below. |
| `FLUSHALL` | 48 failing | In 10 units. The harness's own setup also calls it; the workflow adapts that. |
| `COMMAND` | 40 failing | `INFO` 20, `GETKEYS` 10, `LIST` 6, `GETKEYSANDFLAGS` 3, `COUNT` 1. |
| `CLIENT` subcommands | 37 failing | `PAUSE` 13, `LIST` 9, `REPLY` 5, `NO-TOUCH` 3, `KILL` 2, tracking 5. `unit/pause` also stops at `CLIENT UNPAUSE`. |
| `DEBUG` | 23 failing | `SET-ACTIVE-EXPIRE` 16, `RELOAD` 3, `PROTOCOL` 2, `SLEEP` 1, `REPLYBUFFER` 1. |
| Other server and connection commands | 14 failing | `MONITOR` 6, `SAVE` 3, `TIME` 2, `RESET` 2, `ACL SETUSER` 1. |

`CONFIG SET` is the largest step by far. Redis passes 1,780 tests in the 17
units, and Keel reaches 333 of them before the refusal. Each unit stops at
the first setting Keel refuses. Ranked by Redis's passes in the units each
one stops:

- `appendonly` 363;
- `zset-max-ziplist-entries` 300;
- `save` 288;
- `hash-min-template-entries` 222;
- `hash-max-listpack-entries` 179;
- `set-max-intset-entries` 152;
- `hash-max-listpack-value` 91;
- `list-max-ziplist-size` 90;
- `list-max-listpack-size` 47;
- `slowlog-log-slower-than` 32;
- `latency-monitor-threshold` 16.

Past its first refusal, a unit may need more: the 17 units set 54 settings
in all, read from their files. Inside tests that run, a refused `CONFIG SET`
fails 72 more: `notify-keyspace-events` 26, `maxmemory` 19, and 27 others.

By family, the failing tests are: streams 229, strings 184, scripting
162, arrays 129, bitmaps 74, generic 36, geo 29 and Pub/Sub 16. Pub/Sub
also stops `unit/tracking` at `SUBSCRIBE`, where Redis passes 62. Hashes
(7), sets (6), sorted sets (6) and lists (1) look small only because their
own units are among the 17 that never get past `CONFIG SET`. Their real
counts appear once it accepts those settings.

## How priorities are set

Each item below is ranked by what applications need. The **framework
checks** measure that. Each check runs a small, real workload against Redis
(the control, which must pass) and then against Keel. It records:

- whether the workload passed;
- the first command Keel lacked;
- every command the framework sent, read from Redis's `INFO commandstats`
  after the run.

The first run (#143, local against Redis 8.10.2, October 7, 2026):

| Framework | Keel | Commands it sent that Keel lacks |
| --- | --- | --- |
| Rails' cache store | **passes** | none |
| ActionCable | fails at `SUBSCRIBE` | `PUBLISH`, `SUBSCRIBE`, `UNSUBSCRIBE` |
| Sidekiq | fails at `BRPOP` | `BRPOP`, `EVALSHA`, `SCRIPT LOAD`, `BITFIELD`, `SSCAN` |
| Celery | fails at `EVALSHA` | `BRPOP`, `EVALSHA`, `SCRIPT LOAD`, `PUBLISH`, `SUBSCRIBE`, `PSUBSCRIBE`, `UNSUBSCRIBE` |
| BullMQ | fails at `EVAL` | `EVAL`, `EVALSHA`, `BZPOPMIN`, `XADD`, `XREAD`, `XTRIM`, `HMSET`, `LREM`, `RPOPLPUSH` |

Of the four frameworks Keel fails, **Lua is needed by three**, blocking pops
by three and Pub/Sub by two. Streams are needed by one. The rest are
everyday commands (`BITFIELD`, `SSCAN`, `HMSET`, `LREM`, `RPOPLPUSH`). No
framework passes with Lua alone, or with blocking pops and Pub/Sub alone.
So those three move together in tier 2, and Streams follow in tier 3.

The census, the framework checks and the monitoring gap (#134) each become a
Grafana scoreboard, kept over time on the `bench-history` branch (#132).

## The work, in tiers

The effort figures are rough engineer-weeks, to compare tiers rather than to
schedule them.

1. **Everyday commands, about 6 to 10 weeks.** The missing commands of the
   string, list, hash, set, sorted-set, generic, geo and bitmap families.
   This tier also brings:
   - **`WATCH`**;
   - **server and connection commands,** ordered by how many of Redis's
     tests each unlocks (see "What the tests rank"):
     1. `CONFIG SET` for the settings Redis's tests set. How far each one
        acts is the owner's decision; Keel has no encodings for the
        encoding thresholds to tune, for example.
     2. `FLUSHALL`.
     3. `COMMAND`, with `COUNT`, `DOCS`, `INFO`, `LIST` and `GETKEYS`.
        Client libraries also call it at connect.
     4. The remaining `CLIENT` subcommands, `PAUSE` and `UNPAUSE` first.
     5. `MONITOR`, `TIME`, `RESET` and `ROLE`, and `SLOWLOG` from the
        monitoring work.

     `CONFIG GET` is done (#156). `DEBUG SET-ACTIVE-EXPIRE` is a test hook
     that 16 tests use; adding it is the owner's choice;
   - **server settings Redis has and Keel ties to others:** for example the
     listen backlog. Keel used `-maxclients` as its backlog. Redis has
     `tcp-backlog`, 511 by default and capped by the kernel's `somaxconn`,
     independent of `maxclients`. Done: Keel has `-tcp-backlog`, with
     Redis's default and, on Linux, Redis's startup warning when
     `somaxconn` is lower;
   - **single-node cluster mode:** `CLUSTER SLOTS`, `SHARDS`, `NODES`,
     `INFO`, `MYID`, `KEYSLOT` and `COUNTKEYSINSLOT`, answering as one node
     that owns all 16384 slots, plus `READONLY`, `READWRITE` and `ASKING`.
     Clients that only speak cluster can then connect.

   Each family also gets its missing options.
2. **Blocking commands, Pub/Sub and Lua, about 6 to 8 weeks.** Together
   they unblock four of the five frameworks checked.
   - **Blocking pops:** `BLPOP`, `BRPOP`, `BLMOVE`, `BLMPOP`, `BRPOPLPUSH`,
     `BZPOPMIN`, `BZPOPMAX`, `BZMPOP`. Each needs a queue of blocked clients
     per key, wakeups on writes in arrival order, timeouts, the
     non-blocking behaviour inside `MULTI`, and the log and replication
     recording the pop that happened rather than the wait.
   - **Pub/Sub:** `SUBSCRIBE`, `PSUBSCRIBE`, `PUBLISH`, `PUBSUB`, and the
     sharded `S*` forms. RESP3 push messages and RESP2's subscribed
     connection mode come with them.
   - **Scripting:** `EVAL`, `EVALSHA`, `SCRIPT`, `FUNCTION` and `FCALL` on an
     embedded Lua 5.1, the version Redis runs. That covers `redis.call`, the
     script cache, atomicity, and effects replication into the log and
     replicas.
3. **Streams, about 3 to 4 weeks.** All 20 commands, including consumer
   groups, and `XREAD BLOCK` on tier 2's blocking machinery. BullMQ needs
   them once it has Lua.
4. **Tracking, ACL, notifications and the rest, about 4 to 6 weeks.**
   - Client tracking: `CLIENT TRACKING`, `CACHING`, `GETREDIR`, on tier 2's
     push messages.
   - ACL users and rules.
   - Keyspace notifications, on Pub/Sub.
   - Numbered databases. The embedding plan already says they belong inside
     one engine, as namespaces sharing its budget and its log.
   - Native TLS.
   - RDB: `SAVE`, `BGSAVE`, and `DUMP`/`RESTORE` in Redis's serialization
     format, so that data moves between Redis and Keel.
5. **Redis 8's other bundled data types, separately sized.**
   - **Already part-done:** probabilistic. Keel has most of Bloom, Cuckoo and
     Count-Min; TopK and t-digest are missing.
   - **JSON, about 3 to 4 weeks.**
   - **TimeSeries, about 3 weeks.**
   - **Vector sets and arrays:** 8.x additions.
   - **Search:** the largest by far, a query engine of its own.
6. **Multi-node cluster.** This means:
   - hash-slot ownership across nodes, a cluster bus, `MOVED` and `ASK`
     redirects;
   - resharding and slot migration;
   - cluster failover, on top of Keel's replication, which is still
     experimental.

   It's a distributed system of its own, and its own decision when the tiers
   before it are done.

Also outside the command list: replication with Redis itself (`PSYNC` and
`REPLCONF`, so that Keel could replicate from or to Redis) and reading
Redis's AOF. Both are interoperability decisions for later.

## Sequencing with the embedding plan

The embedding plan (`docs/embedding-plan.md`) still has three phases:

- **Phase 4, operations:** each command family moves to one op-descriptor
  table, typed operations and a `Reply` sink, with byte-identical golden
  replies and log transcripts.
- **Phase 5:** the public Go package.
- **Phase 6:** the server's state moves into `server.Server`.

That shapes the order.

- **Tier 1 goes family by family, right after each family's phase 4
  conversion.** A command added before its family converts is written
  twice: once now, and again in phase 4 with its goldens. Added after, it is
  written once, as a typed operation with a public Go method for phase 5.
  Phase 4 converts a family; that family's missing commands and options
  follow at once, before the next family converts.
- **Tier 2 needs connection state that phase 6 moves.** Blocked clients,
  subscriptions and push messages belong to the server's clients. Building
  them before phase 6 means moving them again, so tier 2 starts after
  phase 6, or phase 6 moves earlier.
- **Lua calls the command language** (string arguments to typed operation
  to reply sink). So it comes after phase 4 has converted the families
  scripts use.
- **Streams are a new family,** written directly in the phase 4 form.
- **Tiers 4 to 6** come after.

Phase 3 (`Open`/`Close`, the per-instance lock, durability for direct
callers) came before phase 4, as the embedding plan has it, and is done:
#148, #149, #151, #152 and #153, merged October 8, 2026.

**`v0.2.0-alpha.1` does not wait for all of tier 1.** The public package
ships with the families phase 4 has converted by then. A family converted
later adds its commands and its Go methods in a minor release, so the library
and parity do not hold each other up.

Phase 6 is about a week. Moving it ahead of phase 5 would let tier 2 start
before the public package ships. That is the owner's option to weigh.

**The owner's decision (October 9, 2026): parity interleaves with phase 4.**
The alternative was to wait until phases 4 to 6 are done. Instead, each
family's missing commands and options follow right after that family's phase
4 conversion, family by family, and `v0.2.0-alpha.1` does not wait for all of
tier 1. Nothing is written twice, and each family reaches parity as soon as
its new form exists. The embedding plan's session had recommended this on
October 7.

## Where Keel improves on Redis

The owner's rule (October 7, 2026) is to follow Redis in everything
wire-visible that clients rely on: replies, error strings, INFO field names
and units, defaults and command behaviour. Where Keel's behaviour is a clear
improvement, meaning it fixes a Redis flaw that clients don't depend on (a
stall, a bug, a needless limitation), Keel keeps it. Each such case is:
- documented as a departure, citing Redis's source;
- listed in its pull request under "Improvements over Redis".

When it's unclear which a difference is, Keel follows Redis and the case is
flagged here.

**Improvement: no spurious OOM while keys are evictable.** Before each write,
Redis's `processCommand` (`src/server.c`) calls `performEvictions`
(`src/evict.c`), and rejects the write with `OOM command not allowed when used
memory > 'maxmemory'` when that returns `EVICT_FAIL`. Under an `allkeys-*`
policy, `performEvictions` samples candidates with `evictionPoolPopulate`,
which reads a bounded number of hash-table buckets (`dictGetSomeKeys`). If a
round samples no key at all, the loop stops (`if (!total_sampled_keys)
break;`), goes to `cant_free` and returns `EVICT_FAIL`. That happens even when
thousands of evictable keys remain, once the main table has grown and then
emptied out, because a sparse table can give an empty sample.

Measured locally with Redis 8.10.2, `maxmemory 64mb`, `allkeys-lru`, and 20 s
of 16 KiB writes over 200,000 keys from 10 connections (October 7, 2026):
- starting from an empty table: no refusals;
- starting after 60,000 small keys had grown the table: 2,629 refusals, with
  about 3,200 evictable keys present throughout.

The first k6 CI run (#145) hit the same case: its earlier scenarios grow the
table. Redis refused 1,106 of 8,553,062 checks there, and Keel refused none of
7,845,956. Keel evicts inline from its own index and never answers OOM while
it holds an evictable key. No client depends on a write being refused when the
keyspace can make room, so Keel keeps this behaviour, as a documented
departure.

**Improvement: one reply to `SET ... GET IFDEQ` with a malformed digest.**
Redis 8.10's `setGenericCommand` (`src/t_string.c`) answers `GET` with the
old value (lines 99-101) and only then checks an `IFDEQ` or `IFDNE` digest's
length (lines 136-137), so `SET k v GET IFDEQ short` on a key holding a
string sends two replies, the value and `ERR must be exactly 16 hexadecimal
characters`, and the client reads the error as the answer to its next
command (seen with Redis 8.10.2, October 12, 2026). Keel answers the error
alone and writes nothing. No client can depend on a command answered twice.

A separate, already-stated semantic difference, not an improvement: Keel's
`maxmemory` bounds the keyspace (the README's integration contract). Redis's
counts all of `used_memory` except replica output buffers and the AOF buffer
(`freeMemoryGetNotCountedMemory` in `src/evict.c`), so normal clients' buffers
count too.

## How each piece is verified

- **Differential tests against one pinned Redis release, byte for byte, in
  RESP2 and RESP3.** The harness already exists for error replies and replies
  (`scripts/differential.py`, `scripts/error-parity.py`). Each new command
  joins it, errors included. Today the harness and the CI census build Redis
  8.10.1, and the local census ran against 8.10.2. One change moves the
  harness, the census and every workflow that builds Redis to 8.10.2 together,
  before tier 1 starts, so that every parity claim is made against the same
  release.
- **The census count rises** by the commands the change adds, and
  `census.json` names them.
- **The framework checks** that needed the piece start to pass.
- **Redis's own tests** (the Redis tests workflow, dispatched on the pull
  request) pass more in the units the piece touches, and none fewer
  anywhere.
- **On the command path**, the paired command-path job's 0.98 rule and no
  new allocations, the embedding plan's verification for hot-path steps.
  Every family's tier 1 work is command-path code, so every tier 1 pull
  request carries this gate.
- **Persistence and replication** get restart, rewrite and replica checks
  for every write the piece adds, and AOF transcript goldens.
- **The command corpora** (the embedding plan's step 4.0) hold every
  command's RESP2 and RESP3 replies and log records, captured from develop
  and replayed on every run. A piece that changes a reply on purpose
  recaptures them with `scripts/capture-command-goldens.py`, and the
  fixture's diff is the change's wire-visible effect, reviewed as such; a
  new command adds its case to the corpus.
- **The README's integration contract** gains the piece in the same pull
  request.
