#!/usr/bin/env python3
"""Run Redis's own test suite against Keel, unit by unit, and against Redis
as the control, and report what passed.

Redis's tests (tests/unit/*.tcl and tests/unit/type/*.tcl in its source)
run against an external server with ./runtest --host --port. This script
runs them one unit at a time, each against a fresh server started for it,
with a timeout of its own, so that a server that crashes or hangs in one
unit costs that unit alone. Every line the harness prints, every test's own
assertion message included, is kept in the unit's log.

    run    --server keel|redis --bin PATH --redis-src DIR --group N --out DIR
           runs one group of units (GROUPS below) and writes a JSON result
           and a log per unit, and group.json;
    report --out DIR --results DIR...
           joins the group results of both servers into redis-tests.json,
           redis-tests.csv and redis-tests.md.

The harness runs as Redis's own external-server workflow runs it
(.github/workflows/external.yml in the Redis source: --verbose --tags -slow)
with, for both servers alike, --singledb (Keel has one database),
--ignore-encoding and --ignore-digest (OBJECT ENCODING and DEBUG DIGEST are
Redis internals), --durable (an error reply that is not an assertion counts
as that test failing, instead of ending the unit) and --dont-clean. Tests
tagged external:skip are skipped by the harness itself. Redis is started as
that workflow starts it (debug, module and protected configs enabled); Keel
with its append-only file on.

A unit's outcome is ok (the harness finished), exception (the harness
stopped on an error it could not count), timeout (this script stopped it),
or crash (the server was gone when the unit ended). Tests the harness never
reached are counted as not run.
"""
import argparse
import csv
import json
import os
from pathlib import Path
import re
import signal
import socket
import subprocess
import sys
import time

# Units that cannot run against an external server at all, with the reason.
SKIPPED = {
    'unit/shutdown': 'stops and restarts the server',
    'unit/tls': 'needs a TLS build and certificates',
    'unit/oom-score-adj': "reads the server process's oom_score_adj",
    'unit/violations': 'needs --large-memory',
}

# The units, in groups sized by their test counts; a group is one CI job.
GROUPS = {
    1: ['unit/type/string', 'unit/type/incr', 'unit/type/increx', 'unit/keyspace', 'unit/scan', 'unit/quit',
        'unit/printver', 'unit/protocol', 'unit/replybufsize', 'unit/querybuf'],
    2: ['unit/type/hash', 'unit/type/hash-field-expire', 'unit/type/hash-templates', 'unit/type/set', 'unit/expire',
        'unit/bitops', 'unit/bitfield'],
    3: ['unit/type/list', 'unit/type/list-2', 'unit/type/list-3', 'unit/type/list-4', 'unit/type/zset', 'unit/sort',
        'unit/geo', 'unit/hyperloglog'],
    4: ['unit/type/stream', 'unit/type/stream-cgroups', 'unit/type/array'],
    5: ['unit/scripting', 'unit/functions', 'unit/acl', 'unit/acl-v2', 'unit/auth'],
    6: ['unit/multi', 'unit/other', 'unit/info', 'unit/info-command', 'unit/info-keysizes', 'unit/introspection',
        'unit/introspection-2', 'unit/slowlog', 'unit/latency-monitor', 'unit/networking', 'unit/limits',
        'unit/dump', 'unit/memefficiency', 'unit/lazyfree', 'unit/maxmemory', 'unit/obuf-limits',
        'unit/client-eviction', 'unit/aofrw', 'unit/gcra', 'unit/hotkeys'],
    7: ['unit/pubsub', 'unit/pubsubshard', 'unit/tracking', 'unit/pause', 'unit/wait'],
}

HARNESS_FLAGS = ['--verbose', '--tags', '-slow', '--singledb', '--ignore-encoding', '--ignore-digest', '--durable',
                 '--dont-clean']
ANSI = re.compile(r'\x1b\[[0-9;]*m')
OK = re.compile(r'^\[ok\]: (.*) \((\d+) ms\)$')
ERR = re.compile(r'^\[err\]: (.*?) in (tests/\S+\.tcl)$')
IGNORE = re.compile(r'^\[(ignore|skip)\]: (.*)$')
EXCEPTION = re.compile(r'^\[exception\]: (.*)$')
TIMEOUT = re.compile(r'^\[TIMEOUT\]')
DONE = re.compile(r'^\[\d+/\d+ done\]: (\S+) \((\d+) seconds\)$')


def parse(text):
    """What the harness printed for one unit: the tests that passed, failed
    (each with the lines that followed its marker, the assertion among
    them) and were ignored, and whether it stopped on an exception or its
    own timeout."""
    passed, failed, ignored = [], [], []
    exception, harness_timeout, done = '', False, False
    current = None
    for raw in text.splitlines():
        line = ANSI.sub('', raw).rstrip()
        if m := OK.match(line):
            passed.append({'name': m[1], 'ms': int(m[2])})
            current = None
        elif m := ERR.match(line):
            current = {'name': m[1], 'file': m[2], 'detail': []}
            failed.append(current)
        elif m := IGNORE.match(line):
            ignored.append(m[2])
            current = None
        elif m := EXCEPTION.match(line):
            exception = m[1]
            current = {'name': '', 'detail': []}  # the lines after it explain
        elif TIMEOUT.match(line):
            harness_timeout = True
            current = None
        elif DONE.match(line):
            done = True
            current = None
        elif line.startswith('*** [err]'):
            current = None  # the summary repeats the failures
        elif current is not None and line and len(current['detail']) < 40:
            current['detail'].append(line)
    if exception and current and current['name'] == '':
        exception = (exception + '\n' + '\n'.join(current['detail'])).strip()
    return {'passed': passed, 'failed': failed, 'ignored': ignored, 'exception': exception,
            'harness_timeout': harness_timeout, 'done': done}


def count_tests(redis_src, unit):
    """How many tests a unit file declares, so that those never reached can
    be counted."""
    path = Path(redis_src) / 'tests' / f'{unit}.tcl'
    try:
        return len(re.findall(r'^\s*test\s+[{"]', path.read_text(errors='replace'), re.M))
    except OSError:
        return 0


def free_port():
    with socket.socket() as reservation:
        reservation.bind(('127.0.0.1', 0))
        return reservation.getsockname()[1]


class Server:
    """A Keel or a Redis started for one unit, in its own directory."""

    def __init__(self, kind, binary, directory):
        self.kind, self.binary, self.directory = kind, Path(binary).resolve(), Path(directory).resolve()
        self.directory.mkdir(parents=True, exist_ok=True)
        self.port = free_port()
        self.log = (self.directory / 'server.log').open('wb')
        if kind == 'keel':
            command = [str(self.binary), '-host', '127.0.0.1', '-port', str(self.port), '-appendonly',
                       '-appendfilename', str(self.directory / 'store.aof')]
        else:
            command = [str(self.binary), '--port', str(self.port), '--bind', '127.0.0.1', '--save', '',
                       '--appendonly', 'yes', '--dir', str(self.directory), '--enable-protected-configs', 'yes',
                       '--enable-debug-command', 'yes', '--enable-module-command', 'yes']
        self.process = subprocess.Popen(command, cwd=self.directory, stdout=self.log, stderr=self.log)
        self.wait_ready()

    def wait_ready(self):
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            if self.process.poll() is not None:
                raise RuntimeError(f'{self.kind} exited during startup; see {self.directory}/server.log')
            try:
                with socket.create_connection(('127.0.0.1', self.port), timeout=1) as s:
                    s.sendall(b'*1\r\n$4\r\nPING\r\n')
                    if s.recv(16).startswith(b'+PONG'):
                        return
            except OSError:
                pass
            time.sleep(.05)
        raise TimeoutError(f'{self.kind} not ready on {self.port}')

    def alive(self):
        return self.process.poll() is None

    def stop(self):
        if self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait()
        self.log.close()


def run_unit(server_kind, binary, redis_src, unit, out, timeout):
    """One unit against a fresh server. Returns its result and writes its
    log and the server's."""
    directory = Path(out) / unit.replace('/', '-')
    directory.mkdir(parents=True, exist_ok=True)
    result = {'unit': unit, 'server': server_kind, 'outcome': 'ok', 'seconds': 0, 'declared': count_tests(redis_src, unit)}
    try:
        server = Server(server_kind, binary, directory / 'server')
    except (RuntimeError, TimeoutError) as e:
        result.update(outcome='crash', detail=str(e), passed=[], failed=[], ignored=[], exception='',
                      harness_timeout=False, done=False)
        return result
    log_path = directory / 'harness.log'
    started = time.monotonic()
    command = ['tclsh', 'tests/test_helper.tcl', '--host', '127.0.0.1', '--port', str(server.port), '--single', unit,
               '--timeout', str(min(timeout, 600)), *HARNESS_FLAGS]
    with log_path.open('wb') as log:
        process = subprocess.Popen(command, cwd=redis_src, stdout=log, stderr=subprocess.STDOUT,
                                   start_new_session=True)
        try:
            exit_code = process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait()
            exit_code = None
            result['outcome'] = 'timeout'
    result['seconds'] = round(time.monotonic() - started, 1)
    result['exit_code'] = exit_code
    text = log_path.read_text(errors='replace')
    parsed = parse(text)
    result.update(parsed)
    if not server.alive():
        result['outcome'] = 'crash'
    elif result['outcome'] == 'ok' and (parsed['exception'] or parsed['harness_timeout'] or not parsed['done']):
        result['outcome'] = 'exception' if parsed['exception'] else 'timeout' if parsed['harness_timeout'] else 'incomplete'
    server.stop()
    result['not_run'] = max(0, result['declared'] - len(parsed['passed']) - len(parsed['failed']) - len(parsed['ignored']))
    result['log'] = str(log_path.relative_to(out))
    (directory / 'result.json').write_text(json.dumps(result, indent=1) + '\n')
    return result


def run(args):
    """Run the group's units in turn, rewriting group.json after each so
    that a job stopped by its own time limit still reports the units it
    finished."""
    units = GROUPS[args.group]
    results = []
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    for unit in units:
        print(f'== {unit} against {args.server}', flush=True)
        result = run_unit(args.server, args.bin, args.redis_src, unit, args.out, args.timeout)
        results.append(result)
        print(f"   {result['outcome']}: {len(result['passed'])} passed, {len(result['failed'])} failed, "
              f"{len(result['ignored'])} ignored, {result['not_run']} not run, {result['seconds']} s", flush=True)
        (out / 'group.json').write_text(json.dumps({'server': args.server, 'group': args.group, 'units': results,
                                                    'complete': len(results) == len(units)}, indent=1) + '\n')
    return 0


def summarise_unit(result):
    return {'outcome': result['outcome'], 'passed': len(result['passed']), 'failed': len(result['failed']),
            'ignored': len(result['ignored']), 'not_run': result.get('not_run', 0), 'seconds': result['seconds'],
            'failing': [f['name'] for f in result['failed']],
            'details': {f['name']: ' '.join(f['detail'])[:300] for f in result['failed']},
            'exception': result.get('exception', '')[:300]}


def report(args):
    """Join every group.json under the result directories into one report."""
    sides = {'keel': {}, 'redis': {}}
    for root in args.results:
        for path in Path(root).rglob('group.json'):
            group = json.loads(path.read_text())
            for result in group['units']:
                sides[group['server']][result['unit']] = summarise_unit(result)
    units = sorted(set(sides['keel']) | set(sides['redis']))
    rows = []
    for unit in units:
        k, r = sides['keel'].get(unit), sides['redis'].get(unit)
        rows.append({'unit': unit, 'keel': k, 'redis': r})
    totals = {}
    for side in ('keel', 'redis'):
        done = [u for u in sides[side].values()]
        totals[side] = {'units': len(done), 'passed': sum(u['passed'] for u in done),
                        'failed': sum(u['failed'] for u in done), 'ignored': sum(u['ignored'] for u in done),
                        'not_run': sum(u['not_run'] for u in done),
                        'units_crashed': sum(u['outcome'] == 'crash' for u in done),
                        'units_timed_out': sum(u['outcome'] == 'timeout' for u in done),
                        'units_exception': sum(u['outcome'] == 'exception' for u in done)}
    result = {'skipped_units': SKIPPED, 'totals': totals, 'units': rows}
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    (out / 'redis-tests.json').write_text(json.dumps(result, indent=1) + '\n')
    with (out / 'redis-tests.csv').open('w', newline='') as f:
        writer = csv.writer(f, lineterminator='\n')
        writer.writerow(['unit', 'redis_passed', 'redis_failed', 'redis_ignored', 'redis_outcome', 'keel_passed',
                         'keel_failed', 'keel_ignored', 'keel_not_run', 'keel_outcome', 'keel_seconds',
                         'keel_failing'])
        for row in rows:
            k, r = row['keel'] or {}, row['redis'] or {}
            writer.writerow([row['unit'], r.get('passed', ''), r.get('failed', ''), r.get('ignored', ''),
                             r.get('outcome', ''), k.get('passed', ''), k.get('failed', ''), k.get('ignored', ''),
                             k.get('not_run', ''), k.get('outcome', ''), k.get('seconds', ''),
                             '; '.join(k.get('failing', []))])
    text = markdown(result)
    (out / 'redis-tests.md').write_text(text)
    print(text)
    return 0


def markdown(result):
    t = result['totals']
    k, r = t['keel'], t['redis']
    parity = f"{100 * k['passed'] / r['passed']:.1f}%" if r['passed'] else 'n/a'
    out = [f"Redis's own tests: Keel passes **{k['passed']}** of the **{r['passed']}** Redis passes in external mode "
           f"({parity}), over {k['units']} units; {k['failed']} fail, {k['not_run']} are not reached, "
           f"{k['units_crashed']} units crash the server, {k['units_timed_out']} time out, "
           f"{k['units_exception']} stop on an exception. Redis itself: {r['passed']} passed, {r['failed']} failed.", '',
           '| Unit | Redis passed | Redis failed | Keel passed | Keel failed | Not run | Keel outcome | Seconds |',
           '| --- | ---: | ---: | ---: | ---: | ---: | --- | ---: |']
    for row in result['units']:
        kk, rr = row['keel'] or {}, row['redis'] or {}
        out.append(f"| {row['unit']} | {rr.get('passed', '-')} | {rr.get('failed', '-')} | {kk.get('passed', '-')} | "
                   f"{kk.get('failed', '-')} | {kk.get('not_run', '-')} | {kk.get('outcome', '-')} | "
                   f"{kk.get('seconds', '-')} |")
    out += ['', 'Units not run: ' + ', '.join(f'{u} ({why})' for u, why in result['skipped_units'].items()) + '.', '']
    return '\n'.join(out) + '\n'


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest='command', required=True)
    r = sub.add_parser('run')
    r.add_argument('--server', choices=['keel', 'redis'], required=True)
    r.add_argument('--bin', required=True, help='the server binary')
    r.add_argument('--redis-src', required=True, help='the unpacked Redis source tree, with tests/')
    r.add_argument('--group', type=int, choices=sorted(GROUPS), required=True)
    r.add_argument('--out', required=True)
    r.add_argument('--timeout', type=int, default=1200, help='seconds per unit (default 1200)')
    r.set_defaults(func=run)
    p = sub.add_parser('report')
    p.add_argument('--out', required=True)
    p.add_argument('--results', nargs='+', required=True, help='directories holding group.json files')
    p.set_defaults(func=report)
    args = ap.parse_args(argv)
    return args.func(args)


if __name__ == '__main__':
    sys.exit(main())
