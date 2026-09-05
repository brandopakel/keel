# External benchmark integrations

The integrations below support local, controlled-host and provider execution.
Keel's public Bencher project supports Free-plan hosted jobs. AWS/Grafana execution,
deployment hosts and application pilots still require selected targets and account
access. Keep raw measurements beside provider dashboards. Never compare runs with
different load or persistence settings as though the provider were the only variable.

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

The same authenticated adapter smoke also passed with xk6-tcp v0.2.0, the version
listed in Grafana Cloud's supported extensions when checked on September 5, 2026.
Its Darwin arm64 archive SHA-256 is
`13a54e7160a765f4048b4eef5408b40a20ce35a21638cec3127967c42539bd43`.
This verifies local compatibility with that extension version; Cloud execution
requires a configured account and reachable test instance.

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
as a successful Cloud run. [Cloud extension contract](https://grafana.com/docs/grafana-cloud/observe-and-act/testing/k6/author-run/use-k6-extensions/).

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

### Free hosted comparisons

The `hosted` suite in `.github/workflows/bench.yml` builds the exact alpha.2 and
alpha.3 source revisions with Go 1.27.1, packages both binaries and the Python
harness, and exercises the image with external networking disabled. Its artifact
contains the Docker image tar, checksums, build information and smoke evidence.
The build context contains only those binaries and scripts; provider credentials
are supplied locally when uploading the image and starting a job.

Upload the verified image to the project's Bencher OCI registry using a temporary
credential store. Select its immutable digest in the command below. Do not place a
registry password in command arguments or persist it in a Docker configuration.

```sh
# BENCHER_API_KEY comes from the local secret store; IMAGE is the uploaded digest.
bencher run --project keel --branch develop \
  --hash a9de9156aeb4d3ef18c06cff8784350f05867045 \
  --testbed bencher-intel-v1-alpha3-loopback-gc \
  --image "$IMAGE" --spec intel-v1 --job-timeout 240 \
  --adapter json --format json --native-tls \
  -- --policy everysec --rep 0 --seconds 5 --enable-loopback

# Save the authenticated project Jobs API response as job.json, then verify it:
python3 bench/external/bencher-evidence.py job.json --out fresh-evidence
```

Use `off`, `everysec` and `always` for each repetition 0 through 4, submitting one
job at a time. Each job runs all three arms with the same load and rotates/reverses
their order by repetition. Five seconds is a short comparison window; it does not
measure sustained capacity. The `off` worker-labelled arm disables persistence and
therefore also disables the worker, providing a repeat control.

Bencher's Free plan permits public projects, one concurrent hosted job and at most
five minutes per job. The command uses a four-minute timeout. The `intel-v1` Spec
provides a Firecracker Linux x86_64 guest with four CPUs, 48 GiB RAM and 128 GiB
disk. Each triplet shares one guest and loopback connection placement. Record the
job's runner UUID; this is on-demand provider hardware, with server and load
generator colocated, not a reserved pair of deployment machines.
[Free-plan limits](https://bencher.dev/pricing/),
[Spec contract](https://bencher.dev/docs/explanation/testbeds/).

The image is self-contained because the guest has no external network. Its
loopback interface can also start down; `--enable-loopback` explicitly brings up
only `lo` in this owned guest. The wrapper records its before/after flags and
retains every attempt, server log, RSS/CPU sample, GC output, binary hash and build
record. BMF metrics go to stdout; a checksum-protected compressed evidence archive
goes to stderr and can be retrieved through the authenticated Jobs API. The
decoder checks archive integrity, paths, member types and size limits before
extracting into a new directory. Keep failed setup jobs as failures, outside
performance summaries. [Image contract](https://bencher.dev/docs/explanation/images/),
[Jobs API](https://bencher.dev/docs/api/projects/jobs/).

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

### Bencher controlled runner entrypoint

`bencher-runner.sh` starts an already registered runner on a Linux/KVM host with
`BENCHER_HOST`, `BENCHER_RUNNER` and `BENCHER_RUNNER_KEY` supplied by the host's secret
store. It requires a Firecracker Spec and disables automatic runner updates so a
benchmark series keeps the same verified executable. It does not enable unsandboxed
jobs or register resources under an unspecified account.

Register the Runner, create a Spec describing the actual CPU/memory/disk and network
policy, and assign that Spec to the Runner in the owner-selected Bencher account
before invoking the script. Record the chosen runner binary version and SHA-256.
This self-hosted entrypoint has been syntax-checked; execution on an owner-selected
KVM host remains pending host/account access. It is separate from the hosted
Free-plan jobs above. [Official registration and runner contract](https://bencher.dev/docs/explanation/self-hosted-runners/).
