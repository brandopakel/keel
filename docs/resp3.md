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

`BF.*`, `CF.*` and `CMS.*` answer RESP3 the way RedisBloom 8.10.1 does,
verified against it with the module loaded. Booleans replace yes-or-no
integers, `BF.INFO` and `CF.INFO` are maps from simple-string names to
integers, and `CMS.INCRBY` and `CMS.QUERY` stay arrays of integers. Where
Keel's RESP2 replies already differed from RedisBloom's, RESP2 keeps Keel's
form, so no RESP2 client sees a change:

- `CF.MEXISTS` sends the bulk strings `"1"`/`"0"` in RESP2, where RedisBloom
  sends integers. In RESP3 it sends booleans, as RedisBloom does.
- `CF.INFO` sends its numbers as bulk strings in RESP2. In RESP3 they are
  integers, as RedisBloom's are. Keel's `CF.INFO` fields are its own (a cuckoo
  filter here does not grow, so there is no `Number of filters` or `Expansion
  rate`), and only the framing follows RedisBloom.
- `BF.INFO` field names are bulk strings in RESP2 and simple strings in RESP3,
  RedisBloom's form.

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
  and `HELLO 3`, so their replies switch protocol inside `EXEC`. Those leave
  out `CF.MEXISTS`, whose RESP2 reply is Keel's own (see above). With
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
- `bench/clients/defaults` runs each library with only a host, port and
  password. See [client defaults](client-library-compatibility.md#default-configurations).
- Unit tests pin each command's RESP3 bytes and its unchanged RESP2 bytes
  (`internal/core/resp3_test.go`). Integration tests cover switching, `HELLO 3
  AUTH`, a pipeline that changes protocol mid-batch, and RESP2 wire bytes
  (`cmd/keel/resp3_test.go`).
