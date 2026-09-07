#!/usr/bin/env python3
"""Matched rewrite interference with independent scheduled-arrival traffic."""
import argparse
import json
import os
from pathlib import Path
import platform
import re
import socket
import subprocess
import sys
import threading
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'scripts'))
from validation_lib import Client, Server, info, sha256


def require_flags(help_text):
    required = ('host', 'port', 'appendonly', 'appendfsync', 'appendfilename',
                'aof-async-append', 'aof-concurrent-append', 'auto-aof-rewrite-percentage')
    missing = [flag for flag in required if not re.search(r'(?m)^\s+-'+re.escape(flag)+r'(?:\s|$)', help_text)]
    if missing: raise ValueError('incompatible runtime flags: '+', '.join(missing))


def require_persistence_fields(stats):
    for field in ('aof_rewrites', 'aof_rewrite_in_progress'):
        if field not in stats or not stats[field].isdigit():
            raise ValueError('incompatible INFO persistence field: '+field)


def compatibility(binary, name, root):
    help_result = subprocess.run([str(binary), '-help'], capture_output=True, text=True, timeout=5)
    help_text = help_result.stdout+help_result.stderr
    (root/(name+'-help.log')).write_text(help_text)
    require_flags(help_text)
    with Server(binary, root/('compatibility-'+name), async_append=True,
                password='rewrite-probe-fixture',
                extra=['-aof-concurrent-append', '-auto-aof-rewrite-percentage', '0']) as server:
        stats = info(server.client, 'persistence')
        require_persistence_fields(stats)
    return dict(binary_sha256=sha256(binary), persistence=stats)


def pin(command, cpus):
    return ['taskset', '-c', cpus, *command] if cpus else command


def arm(args, binary, name, policy, writes, repetition, root):
    root.mkdir()
    report = dict(status='running', arm=name, policy=policy, writes_percent=writes,
                  repetition=repetition, binary_sha256=sha256(binary), rewrites=[])
    (root/'report.json').write_text(json.dumps(report, indent=2)+'\n')
    env = {key: os.environ[key] for key in ('PATH', 'HOME', 'TMPDIR') if key in os.environ}
    with socket.socket() as reservation:
        reservation.bind(('127.0.0.1', 0))
        port = reservation.getsockname()[1]
    command = pin([str(binary), '-host', '127.0.0.1', '-port', str(port),
                   '-appendonly', '-aof-async-append', '-aof-concurrent-append',
                   '-appendfsync', policy, '-appendfilename', str(root/'store.aof'),
                   '-auto-aof-rewrite-percentage', '0'], args.server_cpus)
    server = load = client = monitor = None
    files, samples = [], []
    stop = threading.Event()
    try:
        log = (root/'server.log').open('wb'); files.append(log)
        server = subprocess.Popen(command, env=env, stdout=log, stderr=log)
        deadline = time.monotonic()+10
        while time.monotonic() < deadline:
            if server.poll() is not None:
                raise RuntimeError('owned server exited during startup')
            try:
                client = Client('127.0.0.1', port)
                client.socket.settimeout(5)
                assert client.call('PING') == b'PONG'
                break
            except OSError:
                if client is not None: client.close(); client = None
                time.sleep(.02)
        if client is None: raise TimeoutError('owned server did not listen')
        # 64-key MSETs keep preparation bounded even under appendfsync always.
        payload = b'x'*4096
        keys = args.dataset_mib*256
        for first in range(0, keys, 64):
            parts = [part for index in range(first, min(first+64, keys))
                     for part in (f'snapshot:{index}', payload)]
            assert client.call('MSET', *parts) == b'OK'
        load_command = pin([str(args.load), '-address', f'127.0.0.1:{port}',
                            '-rate', str(args.rate), '-seconds', str(args.seconds),
                            '-connections', '8', '-queue', '64', '-keys', '1000',
                            '-size', '64', '-writes', str(writes)], args.client_cpus)
        subprocess.run([*load_command, '-preload'], env=env, check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, timeout=180)
        report['before'] = info(client, 'persistence')
        common_start = time.time_ns()+2_000_000_000
        started = time.monotonic()+2
        out = (root/'traffic.json').open('wb'); files.append(out)
        err = (root/'traffic.log').open('wb'); files.append(err)
        load = subprocess.Popen([*load_command, '-start-ns', str(common_start)],
                                env=env, stdout=out, stderr=err)
        def telemetry():
            while not stop.wait(.25):
                try:
                    raw = subprocess.check_output(['ps', '-o', 'pid=,rss=,pcpu=', '-p',
                                                   f'{server.pid},{load.pid}'], text=True, timeout=2)
                    samples.append(dict(unix_ns=time.time_ns(), processes=raw))
                except (OSError, subprocess.SubprocessError) as exc:
                    samples.append(dict(unix_ns=time.time_ns(), error=repr(exc)))
        monitor = threading.Thread(target=telemetry, daemon=True); monitor.start()
        targets = [args.seconds*fraction for fraction in (.2, .45, .7)]
        active = None
        deadline = started+args.seconds+20
        while load.poll() is None or active is not None:
            now = time.monotonic()
            if now > deadline: raise TimeoutError('traffic/rewrite did not finish')
            if server.poll() is not None: raise RuntimeError('owned server exited during traffic')
            stats = info(client, 'persistence')
            if active is not None:
                active['pending_sync_observations'] += int(stats.get('aof_rewrite_pending_sync', 0))
                if int(stats['aof_rewrites']) > active['before_count']:
                    active['elapsed_seconds'] = time.monotonic()-active.pop('monotonic_start')
                    report['rewrites'].append(active)
                    active = None
                elif stats['aof_rewrite_in_progress'] == '0':
                    raise RuntimeError('rewrite aborted without completion')
            if active is None and targets and now-started >= targets[0]:
                attempt = time.monotonic()
                try:
                    assert client.call('BGREWRITEAOF') == b'Background append only file rewriting started'
                except RuntimeError as exc:
                    if 'pending append' not in str(exc): raise
                else:
                    active = dict(requested_at_seconds=attempt-started,
                                  monotonic_start=attempt, before_count=int(stats['aof_rewrites']),
                                  pending_sync_observations=0)
                    targets.pop(0)
            time.sleep(.01)
        assert load.wait(timeout=1) == 0, 'arrival generator failed'
        out.flush(); err.flush()
        traffic = json.loads((root/'traffic.json').read_text())
        assert traffic['scheduled'] == int(args.rate*args.seconds)
        assert traffic['scheduled'] == sum(traffic[key] for key in
                                           ('completed', 'failed', 'queue_dropped', 'queue_expired'))
        assert traffic['failed'] == 0
        assert len(report['rewrites']) == 3, 'all three rewrites must complete during measured traffic'
        assert all(row['requested_at_seconds']+row['elapsed_seconds'] < args.seconds
                   for row in report['rewrites']), 'rewrite outlasted measured traffic'
        report['traffic'] = traffic
        report['after'] = info(client, 'persistence')
        # Verify the entire untouched snapshot dataset, outside measurement.
        for first in range(0, keys, 64):
            count = min(64, keys-first)
            assert client.call('MGET', *(f'snapshot:{i}' for i in range(first, first+count))) == [payload]*count
        report['status'] = 'completed'
    except BaseException as exc:
        report.update(status='failed', failure=repr(exc))
        raise
    finally:
        stop.set()
        if monitor is not None: monitor.join(timeout=3)
        if client is not None: client.close()
        for process in (load, server):
            if process is not None:
                if process.poll() is None: process.terminate()
                try: process.wait(timeout=8)
                except subprocess.TimeoutExpired: process.kill(); process.wait()
        for file in files: file.close()
        report['telemetry'] = samples
        (root/'report.json').write_text(json.dumps(report, indent=2)+'\n')
    return report


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    for binary in ('candidate', 'baseline', 'load'):
        parser.add_argument('--'+binary, type=Path, required=True)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--dataset-mib', type=int, default=32)
    parser.add_argument('--seconds', type=float, default=15)
    parser.add_argument('--rate', type=int, default=2000)
    parser.add_argument('--reps', type=int, default=3)
    parser.add_argument('--policies', default='no,everysec,always')
    parser.add_argument('--writes', default='0,20')
    parser.add_argument('--server-cpus')
    parser.add_argument('--client-cpus')
    args = parser.parse_args()
    policies, writes = args.policies.split(','), [int(value) for value in args.writes.split(',')]
    if not (1 <= args.dataset_mib <= 512 and 5 <= args.seconds <= 120 and
            1 <= args.rate <= 100000 and 1 <= args.reps <= 10 and
            set(policies) <= {'no', 'everysec', 'always'} and all(0 <= value <= 100 for value in writes)):
        parser.error('workload outside bounded validation limits')
    for name in ('candidate', 'baseline', 'load', 'out'):
        setattr(args, name, getattr(args, name).resolve())
    args.out.mkdir(parents=True, exist_ok=False)
    report = dict(status='running', platform=platform.platform(),
                  arguments={key: str(value) if isinstance(value, Path) else value for key, value in vars(args).items()},
                  harness_sha256=sha256(__file__), generator_sha256=sha256(args.load), runs=[])
    try:
        report['compatibility'] = {name: compatibility(binary, name, args.out)
                                   for name, binary in [('baseline', args.baseline), ('candidate', args.candidate)]}
        for repetition in range(args.reps):
            arms = [('baseline', args.baseline), ('candidate', args.candidate)]
            if repetition % 2: arms.reverse()
            for policy in policies:
                for write in writes:
                    for name, binary in arms:
                        directory = args.out/f'r{repetition+1}-{policy}-writes{write}-{name}'
                        try:
                            report['runs'].append(arm(args, binary, name, policy, write, repetition+1, directory))
                        except Exception as exc:
                            report.setdefault('arm_failures', []).append(dict(arm=directory.name, failure=repr(exc)))
                            path = directory/'report.json'
                            failed = json.loads(path.read_text()) if path.exists() else dict(arm=name)
                            failed.update(status='failed', failure=repr(exc))
                            report['runs'].append(failed)
                        (args.out/'report.json').write_text(json.dumps(report, indent=2)+'\n')
        if report.get('arm_failures'):
            raise RuntimeError(f"{len(report['arm_failures'])} rewrite validation arms failed")
        report['status'] = 'completed'
    except BaseException as exc:
        report.update(status='failed', failure=repr(exc))
        raise
    finally:
        (args.out/'report.json').write_text(json.dumps(report, indent=2)+'\n')
