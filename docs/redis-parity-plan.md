# Redis parity plan

Status: draft, October 7, 2026. For the owner and the embedding plan's
session to agree on before any of it is built.

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
| string | 12/26 | `APPEND`, `GETRANGE`, `SETRANGE`, `STRLEN`, `GETDEL`, `GETEX`, `GETSET`, `SUBSTR`, `INCRBYFLOAT`, `MSETNX`, and 8.x's `DELEX`, `DIGEST`, `INCREX`, `MSETEX` |
| list | 9/24 | `LPOS`, `LINSERT`, `LREM`, `LMOVE`, `LMOVEM`, `RPOPLPUSH`, `LPUSHX`, `RPUSHX`, `LMPOP`, and the blocking forms |
| hash | 11/29 | `HSTRLEN`, `HRANDFIELD`, `HINCRBYFLOAT`, `HMSET`, `HSCAN`, `HGETDEL`, `HGETEX`, `HSETEX`, `HIMPORT`, field expiry (`HEXPIRE` family, 8 commands) |
| set | 8/19 | `SINTER`, `SUNION`, `SDIFF` with `STORE` and `CARD` forms, `SMOVE`, `SSCAN` |
| sorted-set | 12/35 | `ZUNION`, `ZINTER`, `ZDIFF` with `STORE` forms, the lex ranges, `ZREMRANGEBY*`, `ZREVRANGE`, `ZREVRANK`, `ZMSCORE`, `ZRANDMEMBER`, `ZRANGESTORE`, `ZMPOP`, `ZSCAN`, and the blocking forms |
| generic | 13/29 | `RENAME`, `RENAMENX`, `COPY`, `TOUCH`, `OBJECT`, `RANDOMKEY`, `EXPIRETIME`, `PEXPIRETIME`, `SORT`, `SORT_RO`, `DUMP`, `RESTORE`, `MOVE`, `MIGRATE`, `WAIT`, `WAITAOF` |
| server | 5/32 | `CONFIG`, `COMMAND`, `TIME`, `ROLE`, `FLUSHALL`, `SWAPDB`, `SLOWLOG`, `LATENCY`, `MONITOR`, `ACL`, `DEBUG`, `SAVE`, `BGSAVE`, `LASTSAVE`, `REPLICAOF`, `FAILOVER`, `SHUTDOWN`, `MODULE`, `SYNC`, `PSYNC`, `REPLCONF`, and others |
| geo, hyperloglog, connection, transactions | 19/28 | the `GEORADIUS` family, `GEOSEARCHSTORE`, `PFDEBUG`, `PFSELFTEST`, `RESET`, `WATCH` |
| bitmap | 0/7 | all |
| pubsub | 0/9 | all |
| scripting | 0/8 | all: `EVAL`, `EVALSHA`, `SCRIPT`, `FUNCTION`, `FCALL` |
| stream | 0/20 | all |
| cluster | 0/4 | all |
| array (8.x) | 0/18 | all |
| vector sets (8.x) | 0/13 | all |

The census checks names, not options. `ZADD GT`, `LT` and `INCR`, and
`ZRANGE BYLEX`, are known gaps inside commands Keel has. A second census pass
over each command's documented arguments will find the rest.

## How priorities are set

Each item below is ranked by what applications need. The **framework
checks** measure that. Each check runs a small, real workload against Redis
(the control, which must pass) and then against Keel. It records:

- whether the workload passed;
- the first command Keel lacked;
- every command the framework sent, read from Redis's `INFO commandstats`
  after the run.

The first frameworks:

- **Sidekiq and Celery:** job queues on blocking list pops;
- **BullMQ:** Lua and streams;
- **ActionCable:** Pub/Sub;
- **Rails' cache store:** strings, expiry and `redis_version`.

The census, the framework checks and the monitoring gap (#134) each become a
Grafana scoreboard, kept over time on the `bench-history` branch (#132).

## The work, in tiers

The effort figures are rough engineer-weeks, to compare tiers rather than to
schedule them.

1. **Everyday commands, about 6 to 10 weeks.** The missing commands of the
   string, list, hash, set, sorted-set, generic, geo and bitmap families.
   This tier also brings:
   - **`WATCH`**;
   - **server and connection commands:** `COMMAND` (with `COUNT`, `DOCS`,
     `INFO`, `LIST` and `GETKEYS`, which client libraries call at
     connect), `TIME`, `ROLE`, `FLUSHALL`, `RESET`, `MONITOR`, the remaining
     `CLIENT` subcommands, and `CONFIG GET` and `SLOWLOG` from the monitoring
     work;
   - **single-node cluster mode:** `CLUSTER SLOTS`, `SHARDS`, `NODES`,
     `INFO`, `MYID`, `KEYSLOT` and `COUNTKEYSINSLOT`, answering as one node
     that owns all 16384 slots, plus `READONLY`, `READWRITE` and `ASKING`.
     Clients that only speak cluster can then connect.

   Each family also gets its missing options.
2. **Blocking commands and Pub/Sub, about 3 to 4 weeks.**
   - **Blocking pops:** `BLPOP`, `BRPOP`, `BLMOVE`, `BLMPOP`, `BRPOPLPUSH`,
     `BZPOPMIN`, `BZPOPMAX`, `BZMPOP`. Each needs a queue of blocked clients
     per key, wakeups on writes in arrival order, timeouts, the
     non-blocking behaviour inside `MULTI`, and the log and replication
     recording the pop that happened rather than the wait.
   - **Pub/Sub:** `SUBSCRIBE`, `PSUBSCRIBE`, `PUBLISH`, `PUBSUB`, and the
     sharded `S*` forms. RESP3 push messages and RESP2's subscribed
     connection mode come with them.
3. **Lua, then Streams, about 6 to 8 weeks.**
   - **Scripting:** `EVAL`, `EVALSHA`, `SCRIPT`, `FUNCTION` and `FCALL` on an
     embedded Lua 5.1, the version Redis runs. That covers `redis.call`, the
     script cache, atomicity, and effects replication into the log and
     replicas.
   - **Streams:** all 20 commands, including consumer groups, and
     `XREAD BLOCK` on tier 2's blocking machinery.
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

So the open question for the owner and the embedding plan's session is
whether parity interleaves with phase 4, as above, or waits for the public
package and `v0.2.0-alpha.1`. The first reaches parity sooner. The second
ships the library sooner.

## How each piece is verified

- **Differential tests against Redis 8.10.1, byte for byte, in RESP2 and
  RESP3.** The harness already exists for error replies and replies
  (`scripts/differential.py`, `scripts/error-parity.py`). Each new command
  joins it, errors included.
- **The census count rises** by the commands the change adds, and
  `census.json` names them.
- **The framework checks** that needed the piece start to pass.
- **On the command path**, the paired command-path job's 0.98 rule and no
  new allocations, the embedding plan's verification for hot-path steps.
- **Persistence and replication** get restart, rewrite and replica checks
  for every write the piece adds, and AOF transcript goldens.
- **The README's integration contract** gains the piece in the same pull
  request.
