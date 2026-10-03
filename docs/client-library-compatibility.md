# Real client-library compatibility

Five pinned libraries exercise the same binary-safe RESP2 caching fixture.
GoGIF stays unchanged; this matrix adds independent language/library coverage.

| Library | Pinned version | Required configuration / pipeline |
| --- | --- | --- |
| go-redis | 9.22.0 | `Protocol: 2`; native `Pipeline` |
| Redigo | 1.9.3 | RESP2; `Send` / `Flush` / ordered `Receive` |
| redis-py | 8.1.0 | `protocol=2`, binary replies; `pipeline(transaction=False)` |
| node-redis | 6.2.1 | `RESP: 2`, bulk strings mapped to buffers; automatic pipelining |
| ioredis | 6.0.0 | `protocol: 2`; native `pipeline()` |

The fixture checks binary strings, nil/repeated MGET entries, conditional SET,
hashes, lists, sets, sorted-set rank order, millisecond expiration, uppercase SCAN
TYPE, wrong-type errors followed by a successful command, and 257 ordered INCR
replies. Each library's own transaction API then runs two
[transactions](transactions.md): go-redis `TxPipelined`, Redigo `Send` of `MULTI`
and the queued commands then `Do("EXEC")`, redis-py's default transactional
`pipeline()`, and node-redis and ioredis `multi()`. One commits with a WRONGTYPE
error inside `EXEC`, which each library must report for that command alone,
and a value carrying CRLF and a multibyte character; the other queues an unknown
command, so `EXEC` must abort and its write must never appear. Every library
closes and reconnects. Authenticated worker-barrier and
ordered-concurrent modes use `appendfsync always`; all surviving strings,
collections and counters are checked after each of two clean AOF restarts.
The AOF-off arm covers initial execution and reconnect. This totals 35 client
invocations, with isolated key prefixes and fresh owned servers per mode. The
restart checks include both transactions' results.

node-redis decodes an `EXEC` reply with its default string mapping, whatever
mapping the queued commands asked for, so the transaction's value is valid
UTF-8 for every library. A binary value holding a whole `EXEC` frame inside a
transaction is covered by the process tests instead.

The local matrix passed against runtime 745ff7c4f62e0631ca9a9698faff172010ffcd94,
binary SHA-256 `428f218b3a888b7ef5e555a6003c6ebfe7adc1df850fa5a2cdf295c2b4d1e805`.
The initial ioredis attempt accidentally retained its version-6 default of RESP3:
authenticated HELLO failed with NOAUTH, preventing its RESP2 fallback. Explicit
RESP2 passes. Both the failed and corrected reports are preserved in
`bench/results/client-library-2026-09-07.json.gz`; this is a configuration
requirement, and the matrix does not claim default RESP3 compatibility.

CI builds the candidate and isolated Go client module, installs the locked Node
dependencies and pinned Python package, runs the matrix, and retains reports,
server logs and build/runtime versions even on failure. The only password is a
public synthetic fixture; no account credentials or external services are used.

Run locally:

```sh
(cd bench/clients/go && go build -o /tmp/keel-go-clients .)
npm ci --ignore-scripts --no-audit --no-fund --prefix bench/clients/node
python3 -m venv /tmp/keel-client-env
/tmp/keel-client-env/bin/python -m pip install -r bench/clients/requirements.txt
python3 bench/clients/run.py --bin /path/to/keel --go-clients /tmp/keel-go-clients --python /tmp/keel-client-env/bin/python --out dist/client-matrix
```

Coverage is limited to these commands and connection modes. Raw-command methods
exercise each library's wire codec and handshake; native pipelines exercise its
reply association, and transaction APIs its MULTI/EXEC handling. `WATCH`, RESP3,
cluster routing, pub/sub, scripting, blocking commands, TLS and every high-level
client method are outside this matrix. RESP3 is covered by the default-settings
probe below, and by the RESP3 differential against Redis described in
[RESP3](resp3.md#validation). Clean restart checks complement the separate crash/failure suites;
they do not establish application compatibility or a durability guarantee for
other fsync policies. Other application traces remain useful pilot work.

The final startup review adds up to three attempts for a confirmed address-in-use
startup exit, recording each port/outcome in the phase report. Other startup
failures remain errors, and no client commands or partially executed fixtures
are retried. All 35 invocations pass again with this startup helper.

## Default configurations

The matrix above configures each library for Keel. An application usually
does not: it passes a host, a port and maybe a password, and the library
decides the rest. `bench/clients/defaults` runs each library that way and then
does what an application does first: SET and GET, the same with a client name,
a pipeline, the library's transaction API and its `quit()`. Every connection
goes through a logging proxy, so what each library sent unprompted is recorded
beside the outcome.

On October 2, 2026, against Redis 8.10.1 every scenario passed for every
library, with and without a password. Against Keel:

| Library (default settings) | develop `acb547b` | with the connection commands | with transactions | with RESP3 |
| --- | --- | --- | --- | --- |
| go-redis 9.22.0 | client name fails (`CLIENT`) | all pass except transactions | all pass | all pass, over RESP3 |
| Redigo 1.9.3 | client name fails (`CLIENT`) | all pass except transactions | all pass | unchanged (RESP2) |
| redis-py 5.3.1 | client name (`CLIENT`), `quit()` (`QUIT`) fail | all pass except transactions | all pass | unchanged (RESP2) |
| node-redis 4.7.1 | client name (`CLIENT`), `quit()` (`QUIT`) fail | all pass except transactions | all pass | unchanged (RESP2) |
| ioredis 5.11.1 | `quit()` fails (`QUIT`) | all pass except transactions | all pass | unchanged (RESP2) |
| ioredis 6.0.0 | nothing connects with a password (`NOAUTH` to `HELLO 3 AUTH`) | all pass except transactions | all pass | all pass, over RESP3 |
| redis-py 8.1.0 | nothing connects (`HELLO 3`) | nothing connects (`NOPROTO`) | nothing connects (`NOPROTO`) | all pass, over RESP3 |
| node-redis 6.2.1 | nothing connects (`HELLO 3`) | nothing connects (`NOPROTO`) | nothing connects (`NOPROTO`) | all pass, over RESP3 |

Before [transactions](transactions.md), every library's transaction scenario
failed on `MULTI`/`EXEC`. With them, every library that connects passes it,
with and without a password: hosted run 37092508827 moved the `tx` scenario
from fail to ok for all six, and changed nothing else.
redis-py 8 and node-redis 6 default to RESP3 and treat a refused `HELLO 3` as
fatal - neither has a fallback path. With RESP3 they connect with default
settings and pass every scenario, transactions included, and so do go-redis 9 and ioredis 6, which now use RESP3 rather than
falling back. Three of them also send `CLIENT MAINT_NOTIFICATIONS ON` while
connecting over RESP3 (redis-py 8, node-redis 6 and go-redis 9) and continue
past `ERR unknown subcommand`, the answer a standalone Redis 8.10.1 gives too.
The wire logs show the four that send `HELLO 3` and the four that send
nothing about protocol and stay on RESP2. Raw results for every run, with
binary checksums, are in `bench/results/client-defaults-2026-10-02.json.gz`.

`run.py` compares Keel's outcomes with `expected.json` and fails on any
difference: a scenario that stops passing is a regression, and one that starts
passing means the change has to say so by updating the file
(`--update-expected`). `--redis-server` adds Redis arms as the reference. CI
runs the Keel arms on every pull request. Run it locally with:

```sh
go build -o /tmp/keel ./cmd/keel
(cd bench/clients/defaults/go && go build -o /tmp/keel-default-go-probe .)
npm ci --ignore-scripts --no-audit --no-fund --prefix bench/clients/defaults/node
python3 -m venv /tmp/redis8 && /tmp/redis8/bin/pip install -r bench/clients/defaults/requirements-redis8.txt
python3 -m venv /tmp/redis5 && /tmp/redis5/bin/pip install -r bench/clients/defaults/requirements-redis5.txt
python3 bench/clients/defaults/run.py --bin /tmp/keel --go-probe /tmp/keel-default-go-probe \
  --node-dir bench/clients/defaults/node --python /tmp/redis8/bin/python --python /tmp/redis5/bin/python \
  --redis-server "$(command -v redis-server)" --out dist/client-defaults
```

## Bloom and cuckoo filter helpers

Libraries with RedisBloom helpers decode `BF.*` and `CF.*` replies as
RedisBloom sends them. redis-py's `cf().info()`, for example, reads named
fields from `CF.INFO`. Keel's filter commands now send RedisBloom's replies in
both protocols. The [RESP3 notes](resp3.md#the-probabilistic-commands) list
what changed and the differences that remain.

On October 3, 2026, redis-py 8.1.0's `bf()` and `cf()` helpers ran the same
session over `protocol=2` and `protocol=3` against Keel and against Redis
8.10.1 with RedisBloom 8.10.1. On develop at `6567ca7`, eight calls answered
differently in RESP2 and seven in RESP3:

- `cf().info()` raised `KeyError: 'Number of filters'` in RESP2. In RESP3 it
  returned Keel's own fields.
- `cf().mexists()` returned `[b'1', b'0']` in RESP2.
- `cf().delete()` of a missing key returned `0`/`False`, not `Not found`.
- A second `cf().reserve()` raised `CF: key already exists`, not `item exists`.
- `cf().info()` of a missing key raised Keel's own message.
- `bf().reserve(..., noScale=True)` was refused.

With this change every call answered the same on both servers, in both
protocols. The info fields that describe memory and cuckoo geometry were left
out of the comparison. The one exception was `bf().insert()`, because Keel
has no `BF.INSERT`. Both sets of results are in
`bench/results/redisbloom-parity-2026-10-03.json.gz`. This was a local check,
not a CI job. `scripts/redisbloom-parity.py`, which CI runs, compares the
commands themselves byte for byte.
