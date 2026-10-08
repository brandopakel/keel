# Error replies

Status: candidate, October 3, 2026.

Where Keel has a command Redis has, its error replies are Redis 8.10.1's, byte
for byte, in both protocols. They also come in Redis's order. A client that
matches on Redis's wording, or that only logs it, sees the same text from Keel.
Keel's own commands (`KEEL.*`, `MORRIS.*`, the old `MEMKV.*` names and `SRAND`)
use the same form.

## The order a command is checked in

Redis looks a command up and counts its arguments before anything else, and
Keel does the same in every place it can be refused:

1. A command it does not have: `ERR unknown command ...`.
2. A container command's subcommand it does not have (`CLIENT`, `MEMORY`):
   `ERR unknown subcommand 'x'. Try CLIENT HELP.`
3. The wrong number of arguments: `ERR wrong number of arguments for 'get' command`,
   with a subcommand named `'memory|usage'`.
4. `NOAUTH Authentication required.` for a connection that has not logged in.
   `AUTH`, `HELLO` and `QUIT` are answered before authentication. `EXEC` is
   refused as `EXECABORT Transaction discarded because of: NOAUTH Authentication required.`
5. `ERR Command not allowed inside a transaction`.
6. A replica's `READONLY You can't write against a read only replica.` and
   `MASTERDOWN`, and a fenced primary's `FENCED`.
7. Then the command runs, or `MULTI` queues it.

Steps 1 to 3 are a single check, `CommandError` in
`internal/core/command_check.go`, made against a single table, `commandArity`.
The transport makes it ahead of `AUTH`, a transaction makes it while queueing,
and `EvalAndResponse` makes it ahead of everything else: log replay, replica
apply and the alternate transports included. So an unauthenticated client, a
transaction and a replica all get the same answer for a malformed command, and a
transaction cannot queue a command that `EXEC` would then refuse for its count.

Within a command, the order of its own checks is Redis's too. For example,
`INCRBY list x` is `ERR value is not an integer or out of range`, not
`WRONGTYPE`, because Redis reads the increment before it looks at the key.
`SET k v EX x NX XX` is a syntax error, because Redis reads every option before
the expiry. `LINDEX nokey x` is nil, because Redis looks the key up before the
index. The type check stays one table, `commandKeyspace`. When that check finds
a key of another type, it first asks `argumentsBeforeType` whether the
command's arguments would have been refused already, using the same parser the
command uses. So a command never runs against a key of the wrong type, and a
well-formed command costs nothing extra.

## Unknown commands

Redis 8 answers `ERR unknown command 'name', with args beginning with: 'a' 'b' `:

- The name is quoted as the client spelled it. The decoder keeps that spelling
  in `Command.Name` when upper-casing changed it.
- The name is cut at 128 bytes, with no `...`.
- Arguments are added while what has been written of them is under 128 bytes.
  Each is cut to the room left, and each is followed by a space, so the reply
  ends `' `.
- A command sent with no arguments is named alone, with no `with args` clause.
- A NUL byte ends a name or an argument, as it ends a C string in Redis's
  `%.*s`.
- Carriage returns and line feeds become spaces.

The error is built from at most those bytes. A name or argument can be as large
as the query buffer, so it is never copied whole to build the error.

Errors in which Redis repeats an argument whole, with `%s`, end at a NUL the same
way: `HELLO`'s syntax error, `CLIENT SETINFO`'s option errors and `EXPIRE`'s
`Unsupported option`. Keel also cuts those at 16 KiB. Redis takes no longer
argument from a connection that has not logged in, and the cap stops an
argument the size of the query buffer from being copied into an error.

## Argument counts

The counts are Redis's for Redis's commands. For `BF.*`, `CF.*` and `CMS.*`
they are the arity RedisBloom v8.10.1 declares; Redis enforces that arity
itself, while queueing too. RedisBloom also checks finer counts inside some
commands: `BF.RESERVE` takes at most seven words, `BF.INFO` three, `CF.RESERVE`
an odd number and `CMS.INCRBY` pairs. Those finer checks answer at the same
point Redis does, when the command runs, which inside a transaction means in
`EXEC`'s reply. Every arity error goes through one helper, `wrongArguments`. It
prints the name in lower case, as Redis's command table and RedisBloom's
declared names have it.

## Subcommands

`CLIENT`, `MEMORY` and `CONFIG` are the container commands Keel has. Their
subcommands are in the same table: `CLIENT ID`, `SETNAME`, `GETNAME`, `SETINFO`,
`INFO`, `HELP`; `MEMORY STATS`, `USAGE`, `HELP`; and `CONFIG GET`, `SET`,
`REWRITE`, `HELP`. Any other subcommand gets Redis's error for a subcommand it
does not have. `CLIENT HELP`, `MEMORY HELP` and `CONFIG HELP` list the
subcommands Keel has, in Redis's form, so `Try CLIENT HELP.` has an answer.
`CONFIG SET` refuses every setting and `CONFIG REWRITE` has no file to write,
each in Redis's words (see [INFO compatibility](info-compatibility.md#commands)).
`OBJECT` and `COMMAND` are not implemented, so they are unknown commands.

## Authentication

- `AUTH` with no arguments is the arity error; more than two arguments is
  `ERR syntax error`.
- A wrong password, or a user other than `default`, is
  `WRONGPASS invalid username-password pair or user is disabled.`, from `AUTH`
  and from `HELLO ... AUTH` alike.
- A failed `AUTH` leaves the connection logged in if it was, as Redis does.
- Without a configured password, `AUTH x` is Redis's
  `ERR AUTH <password> called without any password configured for the default user. Are you sure your configuration is correct?`
  `AUTH default x` and `HELLO 3 AUTH default x` succeed, as they do with
  Redis's passwordless default user.

## Evidence

`scripts/error-parity.py` starts six servers: a Keel and a Redis primary with a
password, one of each without, and a read-only replica of each. It then runs
1,301 cases, each on a fresh connection, and compares every reply's bytes. Cases
for sessions that log in run in both protocols. The cases cover:

- Unknown commands: case, length, binary, CRLF, NUL and many-argument forms,
  before authentication and inside `MULTI`.
- Every command's arity on both sides of its count, also queued and before
  authentication.
- Every subcommand form.
- About 250 generic errors: syntax errors, `WRONGTYPE`, integer and float
  parsing, overflow, expiry, index and missing-key errors, set and sorted-set
  counts, geo, HyperLogLog, `MEMORY`, `INFO`, transactions, `HELLO`, `AUTH` and
  `CLIENT`.
- Replica refusals.

With `--redis-module` naming RedisBloom it adds the `BF`, `CF` and `CMS` counts
and type errors. CI runs it in the general validation job against Redis 8.10.1
and its bundled RedisBloom. `scripts/differential.py` now compares errors byte
for byte instead of by class, in both protocols and inside `EXEC` replies, and
its RESP2 mode runs against Redis 8.10.1 rather than Ubuntu's Redis 7.0.

[`bench/results/error-parity-2026-10-03.json.gz`](../bench/results/error-parity-2026-10-03.json.gz)
holds the local run against Homebrew's Redis 8.10.1, before and after, with
every differing case. Before the change, 776 of 1,301 case runs differed. After
it, none differ apart from the 32 known differences below.

### What differed, and what changed

The counts are distinct cases in the run before the change. A case that ran
in both protocols counts once.

| Found | Cases | Now |
| --- | --- | --- |
| Arity errors naming the command in upper case (`'GET'`), as `'MEMORY USAGE'`, or in Keel's own words: `increment command`, `expiry command`, `expiring SET command`, `wrong number of arguments or unsupported user for AUTH` | 112 | Redis's `'get'` and `'memory\|usage'`, through `wrongArguments` |
| Errors sent as `-(error) ERR ...` (23 handlers' arity errors, so also `INFO a b` and `MEMORY USAGE k SAMPLES 5`, which Redis accepts). `(error)` is how redis-cli displays an error, not part of the protocol | 35 | `-ERR ...`; `INFO` takes several sections and `MEMORY USAGE` takes `SAMPLES` |
| `NOAUTH` where Redis first reports an unknown command, an unknown subcommand or the wrong count | 70 | Named and counted before `NOAUTH` |
| `NOAUTH Authentication required` without Redis's final period; `EXEC` before login answered `NOAUTH`, not `EXECABORT` | 9 | Redis's text and `EXECABORT` |
| Unknown command: the name upper-cased, the arguments not echoed, `...` added past 128 bytes, a NUL not ending the name | 40 | Redis 8's form, built from at most 128 bytes of each |
| A count refused as a syntax error, or in the handler's own words (`LTRIM`, `ZCOUNT`, `ZINCRBY`, `GEOSEARCH`, `PERSIST`, `KEEL.REPL.*`), where Redis gives the arity error | 21 | The arity error, from the table |
| An arity error where Redis accepts the count (`FLUSHDB ASYNC`) or gives a syntax error (`SPOP s 1 2`, `SRANDMEMBER s 1 2`, `ZRANK z m BAD`, `GEODIST ... km x`, `FLUSHDB BAD`) | 12 | Redis's answer: `FLUSHDB SYNC`/`ASYNC` accepted, syntax errors |
| Queued in `MULTI` where Redis refuses while queueing (an unknown subcommand or a subcommand's count), so `EXEC` ran it | 17 | Refused while queueing; `EXEC` answers `EXECABORT` |
| `WRONGTYPE` ahead of an argument error Redis reports first (`INCRBY`, `DECRBY`, `HINCRBY`, `HSET` pairs, `LPOP`/`SPOP`/`SRANDMEMBER`/`ZPOPMIN` counts, `LRANGE`/`LTRIM`, `ZADD`, `ZINCRBY`, `ZRANGE*`, `ZCOUNT`, `ZRANK`, `GEOADD`, `GEODIST`) | 29 | The argument error |
| `LCS`, `PFADD`, `PFCOUNT` and `PFMERGE` on a key of another type: the generic `WRONGTYPE` | 6 | `ERR The specified keys must contain string values`; `WRONGTYPE Key is not a valid HyperLogLog string value.` for a string |
| `WRONGPASS invalid username-password pair` without `or user is disabled.` | 8 | Redis's text |
| `AUTH x` without a configured password in Keel's own words; `AUTH default x` and `HELLO 3 AUTH default x` refused; `AUTH a b c` and a queued `AUTH x y z` the arity error | 9 | Redis's answers; a failed `AUTH` also leaves a logged-in connection logged in now, as in Redis |
| Replica: `READONLY replica rejects writes`; `READONLY` ahead of a wrong count; the unknown command upper-cased | 7 | `READONLY You can't write against a read only replica.`, after the name and count |
| `invalid expire time in 'set'` for `SETEX`/`PSETEX`, and `in 'expire'` for `PEXPIRE`/`EXPIREAT` | 7 | Names the command given the expiry |
| `EXPIRE`'s options: `syntax error` for an unknown option and for each conflict | 5 | `Unsupported option X`, `NX and XX, GT or LT options at the same time are not compatible`, `GT and LT options at the same time are not compatible` |
| Integers spelled `+5`, `05` or `00` accepted (`SET EX`, `INCRBY`, `LRANGE`, `LPOP`, `SELECT` and the other integer arguments); `SET ... EX 10 EX 20` refused; `SET k v EX x NX XX` reporting the integer first | 7 | Redis's strict spelling (log replay still accepts the old ones); a repeated expiry option allowed; every option read before the expiry |
| Counts: `value is not an integer` for a non-numeric or negative `LPOP`/`SPOP`/`ZPOPMIN` count; `SRANDMEMBER s -9223372036854775808`; `DECRBY k -9223372036854775808` | 7 | `value is out of range, must be positive`; Redis's range error; `decrement would overflow` |
| Score ranges: `value is not a valid float` for a `ZRANGEBYSCORE`/`ZCOUNT` bound; `ZRANGEBYSCORE` reading its range before its options; `LIMIT` without `BYSCORE`; `ZRANGE BYSCORE` and `ZRANK WITHSCORE` missing; `ZRANGE ... REV REV` accepted | 20 | `min or max is not a float`, Redis's order and its `LIMIT` error; `ZRANGE BYSCORE` (on the `ZRANGEBYSCORE` path) and `ZRANK WITHSCORE` added; a second `REV` refused |
| `ZADD z NX XX 1` and `GEOADD k NX XX ...`: the option conflict reported in Keel's order and words | 2 | Redis's order; `GEOADD`'s conflict is a syntax error |
| `MEMORY`: `unknown MEMORY subcommand 'x'`; `MEMORY STATS x` a syntax error | 5 | Redis's subcommand error and count |
| `LINDEX nokey x` and `LSET nokey x v`: the index read before the key | 2 | The key first: nil, and `ERR no such key` |
| A NUL inside an echoed subcommand or option (`CLIENT a\0b`, `CLIENT SETINFO a\0b`, `HELLO 3 B\0AD`); `CLIENT SETINFO`'s option cut at 128 runes | 4 | Echoed as C prints it, up to the NUL; uncut up to 16 KiB |
| `GEOSEARCH`'s old bare-radius form, a Keel extension that Redis refuses (`GEOSEARCH` is a read and never logged) | not a harness case | Answered as Redis answers it |
| `BGREWRITEAOF` during a rewrite: `ERR a rewrite is already running` | not a harness case | `ERR Background append only file rewriting already in progress` |

The other 21 cases that differed before are the known differences below.

### Differences that stay

These are features Keel does not have. Each is answered as Redis answers a
command, subcommand or option it does not have. The harness lists them in
`KNOWN` with the reason, still runs them, and reports them as known differences.

| Difference | Why it stays |
| --- | --- |
| `WATCH` is an unknown command, before authentication too | Not implemented, deliberately: an optimistic-locking API fails loudly instead of not watching. See [transactions](transactions.md) |
| `OBJECT`, `COMMAND` and other commands Keel does not have | Unknown commands, in Redis's words |
| `CLIENT KILL`, `LIST` and other subcommands, `MEMORY DOCTOR`/`PURGE`/`MALLOC-STATS` | Unknown subcommands, in Redis's words |
| `SELECT 1` and above | Keel has one database: `ERR DB index is out of range` |
| `ZADD GT`/`LT`/`INCR`, `ZRANGE BYLEX`, `SET IFEQ`/`IFNE`/`IFDEQ`/`IFDNE` | Not implemented. The word is read as a score or refused as a syntax error, as a Redis without the option reads it |
| More than ten arguments from a connection that has not logged in | Redis answers `Protocol error: unauthenticated multibulk length` and closes the connection, and also limits unauthenticated arguments to 16 KiB and the query buffer to 1 MiB. Keel bounds requests with its own admission limits and has not adopted these. That is a hardening change of its own, not a wording change |
| `MASTERDOWN replica has no recent primary state` | Redis's text names `replica-serve-stale-data`, a setting Keel does not have. Keel's replica refuses reads after five seconds without its primary. Clients match the `MASTERDOWN` class, which is the same |
| A failed `AUTH` inside `EXEC` | Redis 8.10.1 sends no element for it while its array still counts one, so a client waits forever. Keel sends the `WRONGPASS` element |
| `SET k v PX 9223372036854775807` | Redis's own answer depends on how it was compiled (signed overflow), and Homebrew's build answers `OK`. Keel refuses the expiry. It is not a harness case |
| `SRANDMEMBER` counts past 16 million, `SCAN` `COUNT` past Keel's limit | Keel's allocation bounds, which Redis does not have |
| Protocol errors and inline commands | Keel refuses anything that is not a RESP array of bulk strings, with its own `ERR Protocol error: invalid RESP input`. Redis accepts inline commands and words each protocol error separately |
| RedisBloom's own replies and errors beyond counts and `WRONGTYPE` | Compared byte for byte by `scripts/redisbloom-parity.py` (#94), which CI runs beside this harness |
