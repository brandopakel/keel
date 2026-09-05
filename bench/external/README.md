# External benchmark integrations

The integrations below execute locally or on controlled hosts. Hosted publication,
AWS deployment, and real application pilots require an existing account/target and
budget; none has been provisioned or claimed here. Keep raw measurements beside
provider dashboards. Never compare runs with different load or persistence settings
as though the provider were the only variable.

## Grafana k6

`k6/keel.js` uses `k6/x/tcp` with a byte-oriented RESP2 parser. It supports AUTH via
`KEEL_PASSWORD`, verified TLS via `KEEL_TLS=1` and a TLS proxy, finite socket timeouts,
reply checks, a reply-buffer cap, request/error metrics and a constant-arrival-rate
scenario. `RATE` is **batches per second**, and `BATCH` is sequential requests per
connection. Connections are reused within each batch and closed afterward. This
is not a fixed persistent-connection benchmark. `dropped_iterations` must remain
zero; request latency does not include connection establishment or scheduled wait.
Keep iteration/connect metrics as well. The default error budget is zero and p99
threshold is 100 ms; set the application's real threshold with `P99_MS`.

Validated with xk6-tcp v0.3.1's bundled k6 v2.0.0. Acquire an appropriate pinned
[official release](https://github.com/grafana/xk6-tcp/releases/tag/v0.3.1), verify its
GitHub asset SHA-256 digest, and retain the tool version/hash. The Darwin arm64
archive tested here has SHA-256
`a9ad89e14be405f9e84f7f445ed7b814a4d7c35b293c1f0d18aceda94476ca5c`.

```sh
KEEL_HOST=keel.internal KEEL_PORT=6379 RATE=100 BATCH=20 DURATION=30s \
  k6 run --out json=raw-k6.json.gz bench/external/k6/keel.js
node bench/external/k6/resp.test.mjs
python3 bench/external/smoke.py --bin ./keel --k6 /path/to/k6
```

Use a runtime secret store/environment for the password. Do not put it in scripts,
archives or source control. Grafana-hosted execution supports a subset of extensions;
verify xk6-tcp availability in that execution mode before selecting it. A custom
binary can execute on a controlled host; use the provider's supported local-execution
upload path only after configuring the project/account. Do not treat a local smoke
as a successful Cloud run. [Cloud extension contract](https://grafana.com/docs/grafana-cloud/testing/k6/author-run/use-k6-extensions/).

## Bencher

The exporter converts raw tail CSV (plain or gzip) into Bencher Metric Format.
Latency converts milliseconds to Bencher's built-in nanoseconds. Repetitions,
policies and probe/load roles stay separate; attempts and errors are exported too.
Invalid/empty measurements fail. Export does not contact a service.

```sh
python3 bench/external/bencher-export.py bench/results/tail-async.csv.gz --out results.bmf.json
# Configure BENCHER_PROJECT, BENCHER_TESTBED, BENCHER_BRANCH, BENCHER_HASH and
# BENCHER_API_KEY in the runtime environment, then explicitly publish:
bash bench/external/bencher-publish.sh results.bmf.json
```

Use a distinct stable testbed for each machine/storage/configuration. Supply the
exact commit; a dirty executable is not adequately identified by its parent hash.
Retain executable hash and raw CSV alongside the report. This integrates results
with Bencher; registration/provisioning of a bare-metal runner is a separate account
operation. [BMF documentation](https://bencher.dev/docs/how-to/track-custom-benchmarks/).

## AWS Distributed Load Testing

The AWS adapter is a Locust test archive with a stdlib RESP client, avoiding a
custom k6 binary dependency in the managed test container. It checks SET/GET values,
reports connection/authentication and command failures through Locust request events,
uses unique bounded hot sets per user and reconnects only by starting a new user.
Failed connections are discarded. Local validation uses Locust 2.46.4; record the
actual Locust/Taurus/container version used by the deployed AWS solution.

```sh
python3 bench/external/aws/package.py --host keel.internal --port 6379 --out keel-dlt.zip
# Local equivalent:
KEEL_HOST=keel.internal locust -f bench/external/aws/locustfile.py \
  --headless -u 100 -r 10 -t 30s --csv results
python3 bench/external/smoke.py --bin ./keel --locust /path/to/locust
```

Upload the zip as a Locust test to the existing DLT console, configure private
network access and runtime secret injection, then set Traffic Shape and the cost
ceiling before launch. The archive contains only code and host/port/TLS metadata.
It never embeds a password. If the deployment cannot inject the AUTH environment
secret, configure that capability before running against an authenticated target.
Download raw failures, timing data and CloudWatch server/generator telemetry after
execution. DLT overrides script concurrency/ramp/duration with its Traffic Shape;
this Locust workload is closed-loop, so compare it to an equivalent closed-loop
run. [Official AWS packaging and traffic-shape contract](https://docs.aws.amazon.com/solutions/latest/distributed-load-testing-on-aws/design-considerations.html).

## Evidence gates

- Run against a dedicated benchmark namespace/instance, with explicit AUTH and TLS policy.
- Record separate load and server host shape, placement, storage, OS, tool and binary hashes.
- Interleave baseline/candidate order; match traffic shape, values, expiry and durability.
- Retain errors/drops, raw latency, RSS/CPU/GC and recovery correctness.
- Passing adapters establish protocol/tool integration, not controlled-cloud results or adoption.
