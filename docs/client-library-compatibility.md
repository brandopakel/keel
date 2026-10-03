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
replies. Every library closes and reconnects. Authenticated worker-barrier and
ordered-concurrent modes use `appendfsync always`; all surviving strings,
collections and counters are checked after each of two clean AOF restarts.
The AOF-off arm covers initial execution and reconnect. This totals 35 client
invocations, with isolated key prefixes and fresh owned servers per mode.

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
reply association. Transactions, RESP3, cluster routing, pub/sub, scripting,
blocking commands, TLS and every high-level client method are outside this
matrix. Clean restart checks complement the separate crash/failure suites;
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

| Library (default settings) | develop `acb547b` | with the connection commands |
| --- | --- | --- |
| go-redis 9.22.0 | client name fails (`CLIENT`) | all pass except transactions |
| Redigo 1.9.3 | client name fails (`CLIENT`) | all pass except transactions |
| redis-py 5.3.1 | client name (`CLIENT`), `quit()` (`QUIT`) fail | all pass except transactions |
| node-redis 4.7.1 | client name (`CLIENT`), `quit()` (`QUIT`) fail | all pass except transactions |
| ioredis 5.11.1 | `quit()` fails (`QUIT`) | all pass except transactions |
| ioredis 6.0.0 | nothing connects with a password (`NOAUTH` to `HELLO 3 AUTH`) | all pass except transactions |
| redis-py 8.1.0 | nothing connects (`HELLO 3`) | nothing connects (`NOPROTO`) |
| node-redis 6.2.1 | nothing connects (`HELLO 3`) | nothing connects (`NOPROTO`) |

Transactions fail everywhere on `MULTI`/`EXEC`, which Keel does not implement.
redis-py 8 and node-redis 6 default to RESP3 and treat a refused `HELLO 3` as
fatal - neither has a fallback path - so they need `protocol=2` and `RESP: 2`
until Keel speaks RESP3. ioredis 6 falls back on `NOPROTO`, and go-redis on
any error. Raw results for both runs, with binary checksums, are in
`bench/results/client-defaults-2026-10-02.json.gz`.

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
