#!/usr/bin/env python3
"""Drive Keel and Redis through load phases while Grafana watches both.

The Live telemetry workflow (.github/workflows/telemetry.yml) starts Keel and
Redis with the same maxmemory and eviction policy, puts the standard
redis_exporter in front of each, and has Grafana Alloy send both exporters'
metrics to Grafana Cloud. This script runs the load: each phase runs memtier
against both servers at the same time, so their graphs line up.

It is not a benchmark. Both servers share one hosted runner, unpinned, and
the paired benchmarks (command-path.yml, general-validation.yml's matched job)
are what compare performance. What it shows is how each server behaves
through steady load, pipelining, eviction and expiry, and how much of that a
Redis dashboard can see.

It also serves its own metrics on --metrics-port, which Alloy scrapes:
- keel_ci_phase{phase}: 1 for the phase running now, 0 for the others, so
  that a dashboard can shade each phase;
- keel_ci_exporter_metric_names{server}: how many metric names the exporter
  reports for each server. Redis's count is what monitoring built for Redis
  expects; the gap is what it cannot see on Keel.

At the end it writes, under --out, each phase's memtier output and a
compatibility summary: the exporter metric names Redis has and Keel lacks, in
compat.json and compat.md. The exit status is 1 if any memtier run failed or
an exporter could not be read.
"""
import argparse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import re
import subprocess
import sys
import threading
import time
import urllib.request

SERVERS = ('keel', 'redis')

# Each phase gets an equal share of the run. Values are bytes; the eviction
# phase writes far more than either server's maxmemory holds.
PHASES = [
    ('steady', 'GET-heavy load, 32-byte values',
     ['--ratio=1:10', '--data-size=32', '--key-maximum=100000', '--clients=25']),
    ('pipelined', 'the same, 16 commands per round trip',
     ['--ratio=1:10', '--data-size=32', '--key-maximum=100000', '--clients=25', '--pipeline=16']),
    ('eviction', 'writes of 16 KiB values over more keys than maxmemory holds',
     ['--ratio=1:1', '--data-size=16384', '--key-maximum=200000', '--clients=10']),
    ('expiry', 'writes with 1 to 5 second TTLs',
     ['--ratio=1:2', '--data-size=128', '--key-maximum=100000', '--clients=25', '--expiry-range=1-5']),
]

# Metrics of the exporter itself and of its Go runtime, the same for any server.
OWN = re.compile(r'^(go_|process_|promhttp_|redis_exporter_|redis_last_key_groups_scrape_|'
                 r'redis_target_scrape_request_errors_)')


def metric_names(text):
    """The metric names in a Prometheus text exposition, without the exporter's own."""
    names = set()
    for line in text.splitlines():
        if line and not line.startswith('#'):
            name = re.match(r'[a-zA-Z_:][\w:]*', line)[0]
            if not OWN.match(name):
                names.add(name)
    return names


def compat(keel_text, redis_text):
    keel, redis = metric_names(keel_text), metric_names(redis_text)
    return {'keel': len(keel), 'redis': len(redis), 'shared': len(keel & redis),
            'redis_only': sorted(redis - keel), 'keel_only': sorted(keel - redis)}


def compat_markdown(result):
    out = [f"The exporter reports {result['keel']} metric names for Keel and {result['redis']} for Redis; "
           f"{result['shared']} are shared.", '']
    if result['redis_only']:
        out += [f"**Missing on Keel ({len(result['redis_only'])}):** " +
                ', '.join(f'`{n}`' for n in result['redis_only']), '']
    if result['keel_only']:
        out += [f"**Only on Keel ({len(result['keel_only'])}):** " +
                ', '.join(f'`{n}`' for n in result['keel_only']), '']
    return '\n'.join(out)


def ops_per_second(path):
    try:
        return json.loads(Path(path).read_text())['ALL STATS']['Totals']['Ops/sec']
    except (OSError, ValueError, KeyError):
        return None


class State:
    """What the metrics endpoint reports, updated as the run goes."""

    def __init__(self):
        self.lock = threading.Lock()
        self.phase = 'idle'
        self.names = {}

    def exposition(self):
        with self.lock:
            lines = ['# HELP keel_ci_phase The load phase running now (1) in this telemetry run.',
                     '# TYPE keel_ci_phase gauge']
            for name in ['idle'] + [p[0] for p in PHASES]:
                lines.append(f'keel_ci_phase{{phase="{name}"}} {int(name == self.phase)}')
            if self.names:
                lines += ['# HELP keel_ci_exporter_metric_names Metric names redis_exporter reports per server.',
                          '# TYPE keel_ci_exporter_metric_names gauge']
                lines += [f'keel_ci_exporter_metric_names{{server="{s}"}} {n}' for s, n in self.names.items()]
            return '\n'.join(lines) + '\n'


def serve(state, port):
    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            body = state.exposition().encode()
            self.send_response(200)
            self.send_header('Content-Type', 'text/plain; version=0.0.4')
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *args):
            pass

    server = ThreadingHTTPServer(('127.0.0.1', port), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


def fetch(url):
    with urllib.request.urlopen(url, timeout=10) as response:
        return response.read().decode()


def run_phase(args, name, options, seconds):
    """memtier against both servers at once; returns the failures."""
    runs = {}
    for server in SERVERS:
        port = args.keel_port if server == 'keel' else args.redis_port
        base = args.out / f'{name}-{server}'
        command = [str(args.memtier), '-s', '127.0.0.1', '-p', str(port), '--threads=2', f'--test-time={seconds}',
                   '--key-prefix=telemetry:', '--distinct-client-seed', '--hide-histogram',
                   f'--json-out-file={base}.json', *options]
        log = open(f'{base}.log', 'w')
        runs[server] = (subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT), log)
    failures = []
    for server, (process, log) in runs.items():
        try:
            code = process.wait(timeout=seconds + 60)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()
            code = 'timeout'
        log.close()
        if code != 0:
            failures.append(f'{name} on {server}: memtier exited {code}')
    return failures


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--memtier', type=Path, required=True)
    ap.add_argument('--keel-port', type=int, required=True)
    ap.add_argument('--redis-port', type=int, required=True)
    ap.add_argument('--keel-exporter', required=True, help='URL of the Keel exporter metrics')
    ap.add_argument('--redis-exporter', required=True, help='URL of the Redis exporter metrics')
    ap.add_argument('--metrics-port', type=int, required=True)
    ap.add_argument('--minutes', type=float, required=True, help='length of the whole run')
    ap.add_argument('--settle', type=float, default=30, help='seconds of idle before and after the phases')
    ap.add_argument('--out', type=Path, required=True)
    args = ap.parse_args(argv)
    args.out.mkdir(parents=True, exist_ok=True)

    state = State()
    server = serve(state, args.metrics_port)
    seconds = max(10, int(args.minutes * 60 / len(PHASES)))
    failures, timeline, result = [], [], None
    time.sleep(args.settle)  # An idle baseline before the first phase.
    for name, description, options in PHASES:
        with state.lock:
            state.phase = name
        began = time.time()
        failures += run_phase(args, name, options, seconds)
        timeline.append({'phase': name, 'description': description, 'start': began, 'end': time.time(),
                         'ops_per_second': {s: ops_per_second(args.out / f'{name}-{s}.json') for s in SERVERS}})
        try:
            result = compat(fetch(args.keel_exporter), fetch(args.redis_exporter))
            with state.lock:
                state.names = {'keel': result['keel'], 'redis': result['redis']}
        except OSError as e:
            failures.append(f'after {name}: an exporter could not be read: {e}')
    with state.lock:
        state.phase = 'idle'
    time.sleep(args.settle)  # Lets the last scrapes, with the final counts, reach Grafana.
    server.shutdown()

    (args.out / 'timeline.json').write_text(json.dumps(timeline, indent=2) + '\n')
    lines = ['| Phase | What runs | Keel ops/s | Redis ops/s |', '| --- | --- | ---: | ---: |']
    for t in timeline:
        ops = {s: ('-' if v is None else f'{v:,.0f}') for s, v in t['ops_per_second'].items()}
        lines.append(f"| {t['phase']} | {t['description']} | {ops['keel']} | {ops['redis']} |")
    lines += ['', 'Both servers share one unpinned runner, so these are not a comparison of speed.', '']
    if result:
        (args.out / 'compat.json').write_text(json.dumps(result, indent=2) + '\n')
        (args.out / 'compat.md').write_text(compat_markdown(result))
        lines += ['## What a Redis dashboard sees', '', compat_markdown(result)]
    if failures:
        lines += ['**FAILED**', ''] + [f'- {f}' for f in failures]
    print('\n'.join(lines))
    return 1 if failures else 0


if __name__ == '__main__':
    sys.exit(main())
