# RESP3

Keel answers every connection in RESP2 until the connection asks for RESP3
with `HELLO 3`. From then on every reply on that connection is framed in RESP3,
in the shape Redis 8 sends, until `HELLO 2` switches it back. Other connections
are unaffected.

redis-py 8 and node-redis 6 open every connection with `HELLO 3` and have no
RESP2 fallback, so before this they could not use Keel with default settings.
go-redis 9 and ioredis 6 also send `HELLO 3`, and now use RESP3 rather than
falling back. Redigo, redis-py 5, node-redis 4 and ioredis 5 do not send
`HELLO`, and stay on RESP2.

## Negotiation

`HELLO [protover [AUTH username password] [SETNAME name]]`, in Redis's order:

- The version is checked first. `2` and `3` are accepted, and anything else
  gets `NOPROTO`, before the password is looked at.
- `AUTH` is then tried, and only an authenticated connection gets a reply. With
  a password configured, `HELLO 3` alone answers `NOAUTH`, and
  `HELLO 3 AUTH default <password>` logs in and switches in one command, which
  is what ioredis 6 and redis-py 8 send.
- A `HELLO` that fails changes nothing: not the protocol, not the name.
- `HELLO` without a version reports the protocol in use and does not change it.

The reply is in the protocol just chosen. For RESP3 it is a map, and for RESP2
the same map flattened into an array: `server` (`keel`), `version` (`7.0.0`,
the Redis release whose commands Keel follows), `proto`, `id`, `mode`, `role`
and an empty `modules`. In a pipeline the switch takes effect at the next
command, so `HELLO 3` followed by `GET` in one write gets the GET answered in
RESP3.

Three replies report a connection's protocol, and all three describe the
connection asking:

| Where | RESP2 | RESP3 |
| --- | --- | --- |
| `HELLO` field `proto` | `2` | `3` |
| `CLIENT INFO` field `resp=` | `2` | `3` |
| `INFO server` field `resp_version` | `2` | `3` |

`resp_version` used to be the constant `2`. It now follows the connection, the
same as `proto`.

## Reply shapes

Every RESP2 type is still a RESP3 type, so most replies are byte for byte the
same in both. The rest:

| RESP3 type | Where Keel sends it |
| --- | --- |
| null `_` | Everywhere RESP2 sends `$-1` or `*-1`: `GET`, `HGET`, `LINDEX`, `ZSCORE`, `ZRANK`, `GEODIST`, `MEMORY USAGE`, `KEEL.DUMP` and `CLIENT GETNAME` of something missing; `SET` with `NX`/`XX` refused, and with `GET` of a missing key; `LPOP`/`RPOP` of a missing key, with or without a count; `SPOP`/`SRANDMEMBER` without a count; missing elements of `MGET`, `HMGET`, `GEOHASH` and `GEOPOS` |
| map `%` | `HGETALL`, `HELLO`, `MEMORY STATS`, `LCS ... IDX`, `BF.INFO`, `CF.INFO`, `MORRIS.INFO` |
| set `~` | `SMEMBERS`, and `SPOP` with a count. `SRANDMEMBER` with a count stays an array, as in Redis |
| double `,` | `ZSCORE`, `ZINCRBY`; scores in `ZRANGE`, `ZRANGEBYSCORE`, `ZREVRANGEBYSCORE` and `ZPOPMIN`/`ZPOPMAX`; coordinates in `GEOPOS` and `GEOSEARCH ... WITHCOORD` |
| `[member, score]` pairs | `ZRANGE`/`ZRANGEBYSCORE`/`ZREVRANGEBYSCORE ... WITHSCORES` are arrays of pairs, and so is `ZPOPMIN`/`ZPOPMAX` given a count. Without a count the one pair stays flat, `[member, score]`, as in Redis |
| boolean `#` | `BF.ADD`, `BF.EXISTS`, `CF.ADD`, `CF.ADDNX`, `CF.EXISTS`, `CF.DEL`, and the elements of `BF.MADD`, `BF.MEXISTS` and `CF.MEXISTS` |
| verbatim string `=`, format `txt` | `INFO`, `CLIENT INFO` |

Some replies stay as they are where a RESP3 type might be expected. Each one
follows Redis 8.10.1:

- `GEODIST` and `GEOSEARCH ... WITHDIST` stay bulk strings, at four decimals.
- `SCAN` stays `[cursor, [keys]]`. `KEYS`, `HKEYS`, `HVALS`, `LRANGE`,
  `SRANDMEMBER` with a count, `GEOSEARCH` and `GEOHASH` stay arrays.
- `EXISTS`, `SISMEMBER`, `SMISMEMBER`, `HEXISTS`, `SETNX`, `HSETNX`,
  `EXPIRE` and `PFADD` stay integers. Redis sends booleans only where
  RedisBloom does.

### The probabilistic commands

`BF.*` and `CF.*` answer as RedisBloom 8.10.1 does in both protocols: the same
reply types, the same error text, and the same answer at each edge. This is
checked byte for byte against RedisBloom (see [Validation](#validation)).
Yes-or-no answers are booleans in RESP3 and the integers `1`/`0` in RESP2.
`BF.INFO` and `CF.INFO` are maps from simple-string names to integers in RESP3,
and in RESP2 the flat array of the same simple strings and integers. `BF.INFO
key FIELD` answers a one-entry map in RESP3 and, in RESP2, an array holding
only the value. `CMS.INCRBY` and `CMS.QUERY` stay arrays of integers.

Three RESP2 replies used to be Keel's own, and RESP3 support kept them. They
now follow RedisBloom, so a RESP2 client sees these changes:

- `CF.MEXISTS` sends integers, not the bulk strings `"1"`/`"0"`.
- `CF.INFO` sends RedisBloom's eight fields in its order: `Size`, `Number of
  buckets`, `Number of filters`, `Number of items inserted`, `Number of items
  deleted`, `Bucket size`, `Expansion rate`, `Max iterations`. Their names
  are simple strings and their values integers. It used to send bulk strings
  for both, and it had its own fields. `Number of items inserted` is, as
  RedisBloom counts it, what the filter holds now.
- `BF.INFO` field names are simple strings, not bulk strings.

The errors changed too, in both protocols. `CF.RESERVE` of an existing filter
answers `ERR item exists`, not `CF: key already exists`. `CF.INFO` of a missing
key answers `ERR not found`. `CF.DEL` of a key with no cuckoo filter answers
`Not found` where it answered `0`. A full cuckoo filter answers `Filter is full`.
The `BF.RESERVE` and `CF.RESERVE` refusals are RedisBloom's, word for word, and
come in its order: the parameters first, then the key. `BF.EXISTS`,
`BF.MEXISTS`, `CF.EXISTS`, `CF.MEXISTS` and `CF.COUNT` answer no, or `0`, for a
key of another type, where they answered `WRONGTYPE`. `BF.RESERVE` now takes
`NONSCALING`, and `EXPANSION 0`, which means the same. `BF.INFO` takes a single
field. Both commands read their numbers as Redis does, so `007` and `+5` are
not integers.

Some differences remain, all because Keel's filters are built differently.
None of them is about framing:

- Keel's cuckoo filters have one geometry: `BUCKETSIZE 4`, `MAXITERATIONS 500`,
  `EXPANSION 0`. A full filter refuses an item instead of growing. `CF.INFO`
  reports that geometry. `CF.RESERVE` reads and checks each option as RedisBloom
  does. It then refuses any other value with `ERR this server's cuckoo filters
  have BUCKETSIZE 4, MAXITERATIONS 500 and EXPANSION 0 only`, where RedisBloom
  would build the filter. With no options it builds Keel's filter, where
  RedisBloom's defaults are `2`, `20` and `1`.
- `Size` in both INFO replies, and `Number of buckets` in `CF.INFO`, describe
  each implementation's own memory. Items hash differently in the two, so they
  differ in false positives and in when a cuckoo filter fills.
- RedisBloom lowers a Bloom error rate above `0.25` to `0.25`. Keel builds the
  filter at the rate asked for. That shows in no reply, and the filters already
  in logs and dumps were built that way.
- `TYPE` names the two `bloom` and `cuckoo`, where Redis names them `MBbloom--`
  and `MBbloomCF`.
- `BF.INSERT`, `BF.CARD`, `CF.INSERT`, `CF.INSERTNX`, `CF.COMPACT`, the
  `SCANDUMP`/`LOADCHUNK` pairs and the `DEBUG` forms are not implemented.

What is logged, replicated and dumped did not change. A log written by the
earlier build still replays to the same filters. That includes commands
RedisBloom refuses: a `CF.RESERVE` of capacity 1, a capacity written `007`, an
expansion past 32768, and a `CF.DEL` of a missing key. It also includes
commands RedisBloom reads differently, because it looks for options among all
the arguments, the key included. `BF.RESERVE nonscaling 0.01 100` and
`CF.RESERVE expansion 1000` were only key names to the earlier build, and a
replay reads them that way. This build follows RedisBloom and reads that first
one as `NONSCALING`, so it logs it with the option spelled out. A `NONSCALING`
filter is new. A log or dump that holds one does not load on a build from
before this change.

Nothing in RedisBloom corresponds to `MORRIS.*`, so Keel chose. `MORRIS.INFO` is
a map in RESP3, RESP3's type for name/value pairs, and keeps the bulk-string
names and values it sends in RESP2. `MORRIS.QUERY` and `MORRIS.INCRBY` keep
their arrays of bulk-string counts. `PFADD`, `PFCOUNT` and `PFMERGE` answer
exactly as Redis does in both protocols.

## Transactions

`EXEC` answers with an array of each queued command's reply, and each reply is
framed in the protocol the connection has when that command runs. On a RESP3
connection that is RESP3 throughout. A `HELLO` queued inside the transaction
runs in its place, as Redis runs it, so it switches the protocol partway
through: from RESP2, `MULTI`, `HELLO 3`, `GET missing`, `HGETALL missing`,
`EXEC` answers the RESP3 `HELLO` map, `_` and `%0`, and the connection stays on
RESP3 afterwards. A queued `HELLO 2` does the reverse, and a queued `HELLO`
that fails changes nothing. `+QUEUED`, `MULTI`'s `+OK` and the `EXECABORT`
errors are the same in both protocols. Each queued command's protocol is read
as `EXEC` reaches it, through the connection, rather than when it was queued.

## What does not change

- **Persistence and replication.** The append-only file, both replication
  protocols' feeds, log replay and replica apply record and run commands, not
  replies. They are the same bytes whatever protocol the commands arrived in.
  Replay and replica apply always run as RESP2, whatever a command says, so
  what they do with a reply (look for an error) cannot depend on it. A
  `KEEL.DUMP` image is a bulk string in both protocols.
- **Reply admission.** Every reply that is sized before it is built (MGET,
  HMGET, the collection reads and pops, GEOSEARCH, SCAN) counts the framing of
  the protocol it is built in. A RESP3 map header counts pairs, a double has no
  length line, a null is two bytes shorter, and a nested pair has a header of
  its own. The buffer is allocated to the exact size and reserved as such, and
  the 64 MiB output limit applies to the reply as it will be sent. See
  [reply admission](reply-admission.md) and
  [collection reply admission](collection-reply-admission.md).
- **The other `-mode` variants.** Only the production event loop negotiates.
  The `net*` benchmark transports have no `HELLO`, `CLIENT` or `QUIT`, and
  speak RESP2 only.

## Not supported

Push messages (`>`), `CLIENT TRACKING` and client-side caching, attributes and
big numbers are not sent. `CLIENT MAINT_NOTIFICATIONS` answers
`ERR unknown subcommand`, which is exactly what a standalone Redis 8.10.1
answers. redis-py 8, node-redis 6 and go-redis 9 send it during their RESP3
handshake, and carry on without it on that answer.

## How handlers learn the protocol

The protocol belongs to the connection (`client.resp3` in the server, set by
`HELLO`). The server copies it onto each `core.Command` as the command runs.
`EvalAndResponse` holds it in a command-scoped variable for exactly that
command, the way the log's staging state is held, and restores the previous
value when the command returns. Handlers do not read it directly. They say what
they are answering (a map, a set, a score, a yes-or-no, a null) through the
helpers in `internal/core/resp3.go`, which hold every byte that differs between
the protocols. The connection layer frames its own replies (`HELLO`, `CLIENT`)
with `core.EncodeAs`.

## Validation

- `scripts/differential.py --protocol 3` connects to Keel and Redis with
  `HELLO 3 AUTH` and compares every reply with its RESP3 type: map against
  map, double against double, null against null. It normalizes only what the
  RESP2 mode does: set and map ordering, and the arrays that mode sorts. It
  runs transactions as the RESP2 mode does, and some of them queue `HELLO 2`
  and `HELLO 3`, so their replies switch protocol inside `EXEC`. With
  `--redis-module` naming RedisBloom it adds `BF`, `CF` and `CMS`. Locally,
  against Redis 8.10.1 with RedisBloom, 20,000 seeded steps covered 99
  commands, and the run made 20,019 reply checks, 1,933 of them transactions,
  20 state comparisons, 46 shape checks, 1,296 geo checks and two
  crash/restarts. The reports, with
  the failures found on the way, are in
  `bench/results/resp3-differential-2026-10-02.json.gz`. One failure was not
  about RESP3: `LCS ... IDX` placed matches differently from Redis among
  equally good choices, in both protocols, and now places them where Redis
  does. CI runs it on every pull request against
  Redis 8.10.1 and its RedisBloom, built from the release tarball, pinned by
  checksum.
- `scripts/redisbloom-parity.py` compares every `BF` and `CF` reply with
  RedisBloom's byte for byte, in RESP2 and in RESP3. Errors are compared too,
  including those inside `BF.MADD` arrays and `EXEC` replies. Each protocol
  gets three passes. The first is a fixed corpus of 510 edge cases: every
  parameter RedisBloom parses, at and past its limits and malformed; missing
  keys and keys of other types; full filters; argument counts; and
  transactions. The second is seeded random commands with `MULTI` blocks among
  them. The third reads every filter back. The only fields masked are the ones
  listed among the differences above. The script then checks that every
  filter's `KEEL.DUMP` image is the same over both protocols, and through two
  crash restarts and a rewrite. Locally on October 3, 2026, against Redis
  8.10.1 and the RedisBloom 8.10.1 it ships, 20,000 steps per protocol made
  28,250 RESP2 checks and 28,217 RESP3 checks with no mismatch. Three more
  seeds of 8,000 steps also passed. The same harness found 16,182 mismatches
  on develop at `6567ca7`. One run failed along the way: 76 RESP3 `CF.INFO`
  mismatches, on a filter of RedisBloom's default geometry that RedisBloom
  had grown and then compacted. That is a geometry difference, so those two
  fields are now masked for such filters. The reports are in
  `bench/results/redisbloom-parity-2026-10-03.json.gz`. CI runs the script on
  every pull request, after the RESP3 differential, against the same Redis
  and RedisBloom built from the release tarball.
- `bench/clients/defaults` runs each library with only a host, port and
  password. See [client defaults](client-library-compatibility.md#default-configurations).
- Unit tests pin each command's RESP3 bytes and its RESP2 bytes
  (`internal/core/resp3_test.go`), and RedisBloom's refusals and edge cases
  (`commands_bf_test.go`, `commands_cf_test.go`, `redisbloom_test.go`).
  `redisbloom_persistence_test.go` holds the filter log, both replication
  streams and the dump images to bytes captured on develop at `6567ca7`,
  before any filter reply changed. It also replays a log the earlier build
  could have written, both as a log and as a replica applies one.
  Integration tests cover switching, `HELLO 3 AUTH`, a pipeline that changes
  protocol mid-batch, and RESP2 wire bytes (`cmd/keel/resp3_test.go`).
