# Rewrite failures

Status: unreleased, October 2026.

Keel keeps serving after a failed AOF rewrite, as Redis 8.10.1 does. Before this
change, a rewrite whose own file write failed stopped the server, although the old
log was intact. PR #92 found this when an `EFBIG` on `.rewrite` under the local
wrapper's per-file limit turned into `appendonly: write failed, stopping`.

## What happens now

When a rewrite fails, whether it was started by `BGREWRITEAOF`, automatically, or
by a protocol 2 replica's snapshot:

- The failure is logged as Redis logs it, with its cause:
  `Background AOF rewrite terminated with error: write …/keel.aof.rewrite: file too large; …/keel.aof is still the log and keeps every write`.
- The `.rewrite` file is removed. If a worker still owns it, the worker removes
  it when its I/O returns.
- The old log stays the log. The server keeps serving, and every write is still
  appended to the old log and synced by the configured policy.
- `INFO persistence` reports `aof_last_bgrewrite_status:err` and
  `aof_rewrites_consecutive_failures`.
- Automatic rewrites are retried with Redis's backoff (see [Retrying](#retrying)).
  `BGREWRITEAOF` always starts one at once.

Only a failure of the log itself stops the server, as before. That covers a
failed write or sync of the log and, after a rewrite's rename, a directory sync
that still fails when retried (see [After the rename](#after-the-rename)).

## Each step, before and after

| Step | Before | Now |
| --- | --- | --- |
| Creating `.rewrite` | `BGREWRITEAOF` answered `ERR <cause>`; an automatic one logged and waited a minute | A failed start, as Redis reports one: `aof_last_bgrewrite_status:err`, not counted as a consecutive failure, `Can't rewrite append only file in background: <cause>` logged, and `BGREWRITEAOF` answers Redis's `ERR Can't execute an AOF background rewriting. Please check the server logs for more information.` |
| Key-count ceiling (four million) | As above | As above |
| Snapshot write on the worker, including a short write | Server stopped | Failed rewrite; old log current |
| Snapshot preflush sync on the worker | Server stopped | Failed rewrite |
| Dirty-tail write, synchronous | Server stopped | Failed rewrite |
| Final sync of `.rewrite` | Server stopped | Failed rewrite |
| Closing `.rewrite` | Server stopped | Failed rewrite |
| Opening the new file for appending | Done after the rename, with the old descriptor already closed; a failure left the log closed and stopped the server | Done before the rename; a failure is a failed rewrite |
| Rename onto the log | Server stopped | Failed rewrite. If the rename took effect despite its error, for example a network filesystem's lost reply, the new file is the log and is used |
| Directory sync after the rename | Server stopped | The new file is the log; the directory sync is retried before the log's next sync, and a second failure stops the server as any failed sync of the log does |
| Closing the replaced log | Server stopped | Logged; its contents are superseded |
| Opening the protocol 2 snapshot after the swap | Server stopped | Logged; a waiting replica's pulls start the next rewrite no sooner than a minute later |
| Duration or dirty-key budget | Abandoned, served on, logged `rewrite abandoned` | The same, now also reported as a failed rewrite; the next automatic attempt still waits a minute |

A cancellation at shutdown is not a failure, as a rewrite child killed with
`SIGUSR1` is not one in Redis.

## After the rename

Up to and including the rename, the old log has every write, so abandoning the new
file loses nothing. After the rename the new file is the log and there is no
going back: the old file has no name, and anything appended to it would vanish at
the next restart. So the steps around the swap are ordered so that nothing after
the rename can fail to reach the new file. It is opened for appending first, then
renamed, then the directory is synced, and only then is the old descriptor
closed.

If the directory sync fails, a crash could still leave the old file under the
name. That is safe for everything acknowledged so far: the new file was synced
before the rename, and the old one holds every write up to it. It would not be
safe for writes appended to the new file afterwards. So the directory sync is
tried again before the log's next sync. Under `appendfsync always` that is
before any of those writes is acknowledged, whether appends are synchronous or on
the worker. If the retry fails, `aof_last_write_status` becomes `err` and the
server stops without acknowledging them.

Redis keeps its log in several files named by a manifest, and the file it appends
to does not change at the swap. So its equivalent failure, the manifest's
directory sync, is safe to report as a failed rewrite. Keel's log is one file
whose name moves, so it is not.

## INFO persistence

The fields Redis reports about rewrites, with Redis's values:

| Field | Meaning |
| --- | --- |
| `aof_last_bgrewrite_status` | `err` after a rewrite failed or could not start, `ok` once one finishes, and `ok` after a restart |
| `aof_rewrites_consecutive_failures` | Rewrites in a row that started and did not finish; reset by one that finishes and by a scheduled `BGREWRITEAOF` |
| `aof_rewrite_scheduled`, `aof_pending_rewrite` | 1 while a `BGREWRITEAOF` from inside a transaction waits to start |
| `aof_last_rewrite_time_sec` | How long the last rewrite to end took, finished or not, in whole seconds as Redis counts them; -1 until one ends |
| `aof_current_rewrite_time_sec` | Seconds since the running rewrite started, or -1 |

`aof_rewrite_in_progress` and `aof_last_write_status` are unchanged. A failed
rewrite leaves `aof_last_write_status:ok`, as in Redis. `aof_rewrites` keeps
Keel's meaning, rewrites finished since the log was opened, which tooling waits
on. Redis counts rewrites started.

## BGREWRITEAOF

| Situation | Reply (Redis 8.10.1's) |
| --- | --- |
| No rewrite running, including after a failed one and while automatic rewrites are held back | `+Background append only file rewriting started` |
| A rewrite running | `-ERR Background append only file rewriting already in progress` |
| Inside `EXEC` | `+Background append only file rewriting scheduled` in its place; the rewrite starts once the transaction is over |
| It cannot start | `-ERR Can't execute an AOF background rewriting. Please check the server logs for more information.` |

Keel's own refusals keep their reasons: `ERR appendonly is off` (Redis would
rewrite anyway), and the retry errors for a pending worker append or a previous
rewrite's file still being released.

## Retrying

Automatic rewrites follow Redis's `aofRewriteLimited` (`aof.c`). The first and
second failures in a row are retried at the next check. Redis checks at each
`serverCron` tick, ten times a second at the default `hz`, so a failed attempt
here is followed by the next one no sooner than 100 ms later. From the third
failure in a row each attempt waits: a minute, then two, four, and so on up to an
hour. Redis logs `Background AOF rewrite has repeatedly failed and triggered the
limit, will retry in N minutes`, and so does Keel. A full disk therefore costs
three quick attempts and then at most one an hour, never a loop. A rewrite that
finishes, from any source, ends the limit.

A budget abort counts as a failure, and the next automatic attempt also waits
the minute it always has, since the load that outran one rewrite would outrun the
next. A failed start, which Redis does not count, waits a minute as before; Redis
would retry it at every tick.

A rewrite that a protocol 2 replica needs for its snapshot is held back by the
same limit, so a replica waiting for a snapshot cannot drive a failing disk round
a loop.

## Replication

- Protocol 1 replicates key images from memory and never reads the log or a
  rewrite, so a failed rewrite does not touch it. A replica joining meanwhile
  syncs as usual.
- Protocol 2 streams operations from an in-memory history whose positions belong
  to the primary's epoch, not to a log file. A failed rewrite leaves the epoch,
  the history and any existing snapshot as they were, so streaming replicas carry
  on. A replica that needs a new snapshot gets `pending` frames while the rewrite
  that would make one fails. Its pulls are paced by the limit above, and the
  first rewrite that finishes, whoever started it, serves it.

## Transactions

A transaction written while a rewrite runs goes to the old log framed by `MULTI`
and `EXEC`. If the rewrite then fails, that log is still the log, so the
transaction replays whole. A copy of the log cut inside a block replays none of
the block, as before. `BGREWRITEAOF` inside `EXEC` is now scheduled, as in
Redis, instead of starting part way through the block.

## Checked against Redis

From Redis 8.10.1's source (`src/aof.c`, `src/server.c` at tag `8.10.1`):

- `backgroundRewriteDoneHandler`: the error and signal branches and the
  rename/manifest failures all set `aof_lastbgrewrite_status = C_ERR` and
  increment `stat_aofrw_consecutive_failures`. Then `cleanup:` calls
  `aofRemoveTempFile`, and the live INCR file is left as it was.
- `rewriteAppendOnlyFileBackground`: a failed start sets `C_ERR` without
  counting. `stat_aof_rewrites` counts starts.
- `bgrewriteaofCommand`: the replies above. A child already running is an
  error, `in_exec` schedules and resets the failure count, and otherwise the
  rewrite starts, with no rate limit.
- `aofRewriteLimited`: threshold 3, delay 1 minute doubling to 60.
  `serverCron` asks it only when an automatic or scheduled rewrite would start.
- `genRedisInfoString`: the persistence fields above.

And against the local `redis-server` 8.10.1 (`redis_probe.py`, results in
`bench/results/rewrite-failure-2026-10-03.json.gz`):

- With the working directory read-only, `BGREWRITEAOF` answered `started`. The
  child failed (`Opening the temp file … Permission denied`), and INFO showed
  `aof_last_bgrewrite_status:err`, `aof_rewrites_consecutive_failures:1`,
  `aof_last_rewrite_time_sec:0` and `aof_last_write_status:ok`, with no temp
  file left.
- Writes after the failure were acknowledged and replayed after a restart. A
  second `BGREWRITEAOF` answered `started` and counted 2. One sent while a
  rewrite ran answered the `already in progress` error.
- Inside `MULTI`, `EXEC` answered `scheduled`, with `aof_rewrite_scheduled:1`.
- Automatic rewrites on a failing disk ran three times about 100 ms apart, then
  logged `will retry in 1 minutes`. A manual `BGREWRITEAOF` still started. A
  success reset the count to 0.
- Under `ulimit -f` (256 KiB) a rewrite of a 1 MB value was killed by
  `SIGXFSZ` (`terminated by signal 25`). Redis reported err and kept serving,
  and the data replayed. Keel's Go runtime ignores `SIGXFSZ`, so it sees `EFBIG`.

## Differences that remain

- `aof_rewrites` counts rewrites finished, where Redis counts those started.
- A scheduled `BGREWRITEAOF` whose start fails is dropped and logged. Redis
  keeps it scheduled and retries every tick.
- `BGREWRITEAOF` with the log off is refused. Redis rewrites anyway.
- A failed write or sync of the log itself still stops the server. Under
  `everysec`, Redis instead refuses writes with `MISCONF` and retries. That is
  separate from rewriting and unchanged here.
- A crash part way through a rewrite leaves `.rewrite` behind until the next
  rewrite truncates it. Redis likewise leaves its pid-named temp files.

## Validation

Core tests (`internal/core/aof_rewrite_failure_test.go`) inject each failure in
turn. The cases are `ENOSPC` and short snapshot writes, a snapshot sync `EIO`,
an `EFBIG` dirty-tail write, a final sync `EIO`, failing to open the new file and
a failed rename, each under `always`, `everysec` and `no`. Each case checks the
INFO fields, the log line, that the temporary file is removed, and that the old
descriptor still names the log. It then checks that later writes are in the log,
that a later rewrite succeeds, and that two restarts replay every write. Further
tests cover:

- a start that cannot create its file, and the key ceiling;
- a transaction logged while the snapshot write is held and then fails, which
  replays whole, while a copy cut inside its block replays none of it;
- the directory sync after the rename failing once, and failing again, with
  synchronous appends and with worker appends;
- a rename that took effect despite its error;
- the backoff sequence 1, 2, 4, 8, 16, 32 and 60 minutes, and the 100 ms tick;
- a budget abort;
- protocol 2 deltas, frames included, through a failed rewrite, and snapshot
  pulls paced by the limit and then served.

Process tests (`cmd/keel/rewrite_failure_test.go`) run the real server under a
1 MiB `RLIMIT_FSIZE`. The trigger is a Bloom filter whose log record is one short
`BF.RESERVE` but whose rewritten image is several MiB:

- Under `always`, `everysec`, worker and concurrent appends, the rewrite fails
  with the kernel's `EFBIG`. The server keeps serving, a transaction and later
  writes survive a crash (`always`) or a clean stop (`everysec`), and a later
  rewrite succeeds.
- Automatic rewrites make exactly three attempts and log the limit once, then
  start nothing during a further second of writes.
- Replicas over protocols 1 and 2 keep receiving writes and transactions. A
  protocol 1 replica joins during the failures. A protocol 2 replica that needs
  a snapshot causes two more attempts, then waits, and is served by the next
  rewrite that works.

The same process test against develop `2eeb94b` fails as the bug report
describes: `rewrite abandoned: … file too large`, then `appendonly: write
failed, stopping`.
