#!/usr/bin/env python3
"""Fresh-process, repeated Keel/baseline/Redis comparisons across cache workloads.

Requires a native memtier_benchmark executable. All servers are disposable and
loopback-only. Full raw tool JSON/histograms/logs accompany every recorded arm.
This is closed-loop performance measurement, not an open-loop capacity test.
"""
import argparse
import csv
import hashlib
import json
import os
from pathlib import Path
import platform
import signal
import socket
import statistics
import subprocess
import sys
import threading
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'scripts'))
from validation_lib import Client, sha256


def scenarios():
    base = dict(keys=2500, size=64, clients=16, pipeline=1, ratio='1:19', pattern='R:R', kind='cache')
    changes = [
        ('cache-read-64', {}), ('cache-balanced-64', {'ratio': '1:1'}),
        ('cache-write-64', {'ratio': '19:1'}), ('cache-read-1k', {'size': 1024}),
        ('cache-read-16k', {'size': 16384, 'keys': 1024}),
        ('cache-read-1m', {'size': 1048576, 'keys': 32, 'clients': 4}),
        ('cache-miss', {'ratio': '0:1', 'kind': 'miss'}),
        ('cache-hot', {'pattern': 'G:G'}), ('cache-ttl', {'kind': 'ttl', 'ratio': '1:1'}),
        ('one-client', {'clients': 1}), ('many-clients', {'clients': 256}),
        ('pipeline-16', {'pipeline': 16}), ('pipeline-64', {'pipeline': 64}),
        ('working-set-100k', {'keys': 100000}),
        ('hash', {'kind': 'hash'}), ('set', {'kind': 'set'}),
        ('sorted-set', {'kind': 'zset'}), ('queue', {'kind': 'list'}),
        ('counter', {'kind': 'counter'}), ('hll', {'kind': 'hll'}),
        ('large-list-read', {'kind': 'large-list', 'keys': 1, 'size': 1024}),
        ('reconnect', {'reconnect': 100}),
    ]
    return [dict(base, name=name, **change) for name, change in changes]


def wire(parts):
    parts = [part if isinstance(part, bytes) else str(part).encode() for part in parts]
    return b'*%d\r\n' % len(parts) + b''.join(b'$%d\r\n' % len(part) + part + b'\r\n' for part in parts)


def sample_rss(pid):
    fields = subprocess.check_output(['ps', '-o', 'rss=,pcpu=', '-p', str(pid)], text=True).split()
    if len(fields) != 2:
        raise RuntimeError('owned process disappeared during telemetry')
    return int(fields[0]), float(fields[1])


def preload(client, case):
    payload = b'x' * case['size']
    commands = []
    digest = hashlib.sha256()
    kind = case['kind']
    for index in range(case['keys'] if kind not in ('miss', 'list', 'large-list') else 0):
        key = f'bench:{index+1}'
        if kind == 'hash':
            command = ['HSET', key, 'field', payload]
        elif kind == 'set':
            command = ['SADD', key, 'member']
        elif kind == 'zset':
            command = ['ZADD', key, 1, 'member']
        elif kind == 'hll':
            command = ['PFADD', key, 'member']
        else:
            command = ['SET', key, b'0' if kind == 'counter' else payload]
        encoded = wire(command)
        digest.update(encoded)
        commands.append(encoded)
        if len(commands) == 64:
            client.socket.sendall(b''.join(commands))
            for _ in commands:
                reply = client.read()
                assert reply in (b'OK', 1)
            commands.clear()
    if commands:
        client.socket.sendall(b''.join(commands))
        for _ in commands:
            assert client.read() in (b'OK', 1)
    if kind == 'large-list':
        command = ['RPUSH', 'bench:list'] + [payload] * 256
        digest.update(wire(command))
        assert client.call(*command) == 256
    return digest.hexdigest()


def traffic_options(case):
    kind = case['kind']
    commands = {
        'hash': [('HGET __key__ field', 19), ('HSET __key__ field __data__', 1)],
        'set': [('SISMEMBER __key__ member', 19), ('SADD __key__ member', 1)],
        'zset': [('ZRANGE __key__ 0 9 WITHSCORES', 19), ('ZADD __key__ 2 member', 1)],
        'list': [('RPUSH bench:queue __data__', 1), ('LPOP bench:queue', 1)],
        'counter': [('INCR __key__', 1)],
        'hll': [('PFADD __key__ member', 1), ('PFCOUNT __key__', 19)],
        'large-list': [('LRANGE bench:list 0 -1', 1)],
    }
    if kind in commands:
        options = []
        for command, ratio in commands[kind]:
            options += ['--command='+command, '--command-ratio='+str(ratio)]
        return options
    options = ['--ratio='+case['ratio'], '--key-pattern='+case['pattern']]
    if kind == 'ttl':
        options += ['--expiry-range=300-300']
    if case.get('reconnect'):
        options += ['--reconnect-interval='+str(case['reconnect'])]
    return options


def run_arm(args, arm, binary, case, repetition, directory):
    directory.mkdir()
    with socket.socket() as reservation:
        reservation.bind(('127.0.0.1', 0))
        port = reservation.getsockname()[1]
    env = {key: os.environ[key] for key in ('PATH', 'HOME', 'TMPDIR') if key in os.environ}
    env['GODEBUG'] = 'gctrace=1'
    configuration = None
    if arm == 'redis':
        command = [str(binary), '-']
        configuration = (f'bind 127.0.0.1\nport {port}\nsave ""\nmaxmemory 0\n'
                         f'dir {json.dumps(str(directory))}\nappendonly {"no" if args.policy == "off" else "yes"}\n'
                         f'appendfsync {args.policy if args.policy != "off" else "everysec"}\n'
                         'auto-aof-rewrite-percentage 0\n')
    else:
        command = [str(binary), '-host', '127.0.0.1', '-port', str(port), '-maxkeys', '5000000',
                   '-auto-aof-rewrite-percentage', '0']
        if args.policy != 'off':
            command += ['-appendonly', '-appendfsync', args.policy, '-appendfilename', str(directory / 'store.aof')]
        if args.worker:
            command += ['-aof-async-append']
        if args.profiles:
            command += ['-profile-dir', str(directory / 'profiles')]
    process = client = load = None
    idle_clients, samples = [], []
    stopped = threading.Event()
    telemetry_errors = []
    began = time.monotonic()
    report = {'status': 'running', 'arm': arm, 'case': case, 'repetition': repetition,
              'binary_sha256': sha256(binary), 'policy': args.policy, 'worker': args.worker and arm != 'redis',
              'profiles_enabled': args.profiles, 'server_command': command}
    log = (directory / 'server.log').open('w')
    sampler = None
    try:
        process = subprocess.Popen(command, cwd=directory, env=env, stdout=log, stderr=log,
                                   stdin=subprocess.PIPE if configuration else subprocess.DEVNULL)
        if configuration:
            process.stdin.write(configuration.encode())
            process.stdin.close()
        deadline = time.monotonic() + 10
        while True:
            if process.poll() is not None:
                raise RuntimeError('server exited before readiness')
            try:
                client = Client('127.0.0.1', port)
                assert client.call('PING') == b'PONG'
                break
            except OSError:
                if time.monotonic() >= deadline:
                    raise
                time.sleep(.02)
        # A responding unrelated process must never be used as our benchmark.
        owners = subprocess.check_output(['lsof', '-nP', '-tiTCP:'+str(port), '-sTCP:LISTEN'], text=True).split()
        assert owners == [str(process.pid)], 'listener ownership mismatch'
        report['baseline_rss_kib'] = sample_rss(process.pid)[0]
        report['dataset_sha256'] = preload(client, case)
        time.sleep(.1)
        report['loaded_rss_kib'] = sample_rss(process.pid)[0]
        if args.suite == 'memory':
            for _ in range(case['clients']):
                connection = Client('127.0.0.1', port)
                idle_clients.append(connection)
                assert connection.call('PING') == b'PONG'
            for _ in range(10):
                samples.append([time.monotonic()-began, *sample_rss(process.pid)])
                time.sleep(.05)
            report['verified_idle_clients'] = len(idle_clients)
        else:
            def sample():
                while not stopped.is_set():
                    try:
                        samples.append([time.monotonic()-began, *sample_rss(process.pid)])
                    except Exception as exc:
                        telemetry_errors.append(str(exc))
                        return
                    stopped.wait(.1)

            common = [str(args.memtier), '-s', '127.0.0.1', '-p', str(port), '-P', 'redis',
                      '-t', '1', '-c', str(case['clients']), '--pipeline='+str(case['pipeline']),
                      '--key-prefix=bench:', '--key-minimum=1', '--key-maximum='+str(max(2, case['keys'])),
                      '--data-size='+str(case['size']), '--hide-histogram', '--distinct-client-seed'] + traffic_options(case)
            with (directory / 'warmup.log').open('w') as output:
                subprocess.run(common + ['--test-time=1'], stdout=output, stderr=subprocess.STDOUT, env=env, check=True, timeout=30)
            sampler = threading.Thread(target=sample)
            sampler.start()
            with (directory / 'load.log').open('w') as output:
                load = subprocess.Popen(common + ['--test-time='+str(args.seconds),
                    '--json-out-file='+str(directory / 'memtier.json'),
                    '--hdr-file-prefix='+str(directory / 'latency')], stdout=output, stderr=subprocess.STDOUT, env=env)
                if args.profiles:
                    time.sleep(min(.5, args.seconds / 2))
                    process.send_signal(signal.SIGUSR1)
                assert load.wait(timeout=args.seconds+45) == 0, 'load generator failed'
            stopped.set()
            sampler.join()
            assert not telemetry_errors, telemetry_errors
            raw = json.loads((directory / 'memtier.json').read_text())
            totals = raw['ALL STATS']['Totals']
            report['totals'] = totals
            assert float(totals['Ops/sec']) > 0
            assert 'Connection Errors' in totals and totals['Connection Errors'] == 0
            report['operation_count'] = int(totals['Count'])
            # The raw report and logs retain command errors, misses and tool
            # warnings. Misses are intentional in the explicit miss scenario.
            for name, stats in raw['ALL STATS'].items():
                if isinstance(stats, dict):
                    for key, value in stats.items():
                        if 'error' in key.lower() and isinstance(value, (int, float)):
                            assert value == 0, (name, key, value)
            log_text = (directory / 'load.log').read_text()
            assert 'error response' not in log_text.lower(), 'server returned command errors'
            report['client_cpu_warning'] = 'CPU' in log_text and 'bottleneck' in log_text
            assert client.call('PING') == b'PONG'
            if args.profiles:
                assert (directory / 'profiles/live-1-runtime.json').exists(), 'live profile capture missing'
        report['rss_kib'] = {'median': statistics.median(row[1] for row in samples),
                             'minimum': min(row[1] for row in samples), 'maximum': max(row[1] for row in samples)}
        report['status'] = 'passed'
    except BaseException as exc:
        report['status'] = 'failed'
        report['failure'] = repr(exc)
        raise
    finally:
        stopped.set()
        if sampler:
            sampler.join(timeout=3)
        if load is not None and load.poll() is None:
            load.kill()
            load.wait()
        for connection in idle_clients:
            connection.close()
        if client:
            client.close()
        if process is not None:
            exit_code = process.poll()
            if exit_code is None:
                process.terminate()
                try:
                    exit_code = process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
                    exit_code = -9
            if exit_code != 0:
                report.update(status='failed', shutdown_exit_code=exit_code)
        log.close()
        with (directory / 'telemetry.csv').open('w') as output:
            writer = csv.writer(output)
            writer.writerow(['elapsed_seconds', 'rss_kib', 'lifetime_cpu_percent'])
            writer.writerows(samples)
        report['elapsed_seconds'] = time.monotonic()-began
        (directory / 'report.json').write_text(json.dumps(report, indent=2)+'\n')
    assert report['status'] == 'passed', report
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--candidate', type=Path, required=True)
    parser.add_argument('--baseline', type=Path)
    parser.add_argument('--redis', type=Path)
    parser.add_argument('--memtier', type=Path, default=Path('/opt/homebrew/bin/memtier_benchmark'))
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--suite', choices=['smoke', 'standard', 'memory'], default='standard')
    parser.add_argument('--cases', help='comma-separated case names; default all cases in suite')
    parser.add_argument('--reps', type=int, default=3)
    parser.add_argument('--seconds', type=int, default=5)
    parser.add_argument('--policy', choices=['off', 'no', 'everysec', 'always'], default='off')
    parser.add_argument('--worker', action='store_true')
    parser.add_argument('--profiles', action='store_true')
    args = parser.parse_args()
    if not 1 <= args.reps <= 20 or not 1 <= args.seconds <= 3600:
        parser.error('reps must be 1..20 and seconds 1..3600')
    if args.worker and args.policy == 'off':
        parser.error('worker requires AOF')
    if args.profiles and (args.baseline or args.redis):
        parser.error('profiles are diagnostic candidate-only runs, separate from comparisons')
    os.umask(0o077)
    args.out = args.out.resolve()
    args.out.mkdir(parents=True, exist_ok=False)
    arms = [(name, binary.resolve()) for name, binary in
            [('baseline', args.baseline), ('candidate', args.candidate), ('redis', args.redis)] if binary]
    args.memtier = args.memtier.resolve()
    cases = scenarios()
    if args.suite == 'smoke':
        cases = [case for case in cases if case['name'] in ('cache-read-64', 'hash', 'pipeline-16')]
    elif args.suite == 'memory':
        cases = [dict(name=f'keys-{keys}-value-{size}-clients-{clients}', keys=max(keys, 1), size=size,
                      clients=clients, pipeline=1, ratio='0:1', pattern='R:R', kind='cache' if keys else 'miss')
                 for keys, size, clients in [(0,64,1),(0,64,256),(2500,64,1),(2500,64,256),
                     (25000,64,1),(100000,64,1),(2500,1024,1),(25000,1024,1)]]
    if args.cases:
        names = set(args.cases.split(','))
        cases = [case for case in cases if case['name'] in names]
        if {case['name'] for case in cases} != names:
            parser.error('unknown case name for selected suite')
    metadata = {'status': 'running', 'platform': platform.platform(), 'python': platform.python_version(),
                'harness_sha256': sha256(__file__), 'arguments': {key: str(value) if isinstance(value, Path) else value for key,value in vars(args).items()},
                'tools': {'memtier': subprocess.check_output([str(args.memtier), '--version'], text=True).strip()},
                'binaries': {name: {'path': str(binary), 'sha256': sha256(binary)} for name,binary in arms},
                'method': 'Fixed memtier default seeds, sequential fresh servers, rotated arms, one-second warmup, closed-loop. No concurrent profiling in comparative runs.',
                'runs': []}
    try:
        for repetition in range(1, args.reps+1):
            order = arms[(repetition-1)%len(arms):] + arms[:(repetition-1)%len(arms)]
            if repetition % 2 == 0:
                order.reverse()
            for case in cases:
                for arm,binary in order:
                    name = f'r{repetition}-{case["name"]}-{arm}'
                    result = run_arm(args,arm,binary,case,repetition,args.out/name)
                    metadata['runs'].append({'name': name, 'status': result['status']})
                    (args.out/'manifest.json').write_text(json.dumps(metadata,indent=2)+'\n')
                    print(name, 'passed', round(result.get('totals',{}).get('Ops/sec',0)), 'ops/s', flush=True)
        metadata['status'] = 'passed'
    except BaseException as exc:
        metadata.update(status='failed',failure=repr(exc))
        raise
    finally:
        (args.out/'manifest.json').write_text(json.dumps(metadata,indent=2)+'\n')


if __name__ == '__main__':
    main()
