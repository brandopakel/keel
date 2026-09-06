#!/usr/bin/env python3
"""Bounded authenticated soak, restart/outage/promotion and real OS write failures."""
import argparse
import errno
import json
import os
import platform
import statistics
import subprocess
import time
from collections import deque
from pathlib import Path

from validation_lib import Server, info, rewrite, sha256


def verify(client, expected, events):
    for key, value in expected.items():
        assert client.call('GET', key) == value, key
    assert client.call('LRANGE', 'events', 0, -1) == list(events)


def verify_collections(client, hashes, members, scores, large):
    raw = client.call('HGETALL', 'hash')
    assert dict(zip(raw[::2], raw[1::2])) == hashes
    assert set(client.call('SMEMBERS', 'set')) == members
    ranked = sorted(scores, key=lambda member: (scores[member], member))
    assert client.call('ZRANGE', 'ranking', 0, -1) == ranked
    for member, score in scores.items():
        assert client.call('ZSCORE', 'ranking', member) == str(score).encode()
    assert client.call('LRANGE', 'large-list', 0, -1) == list(large)
    assert client.call('GET', 'large-value') == b'L' * 1048576


def checkpoint(root, report, sample):
    # Append the complete series; keep only a bounded recent window in status.
    with (root/'checkpoints.jsonl').open('a') as output:
        output.write(json.dumps(sample)+'\n')
    report['checkpoints'].append(sample)
    report['checkpoints'] = report['checkpoints'][-120:]
    report['checkpoint_count'] += 1
    progress = root/'progress.tmp'
    progress.write_text(json.dumps(report, indent=2)+'\n')
    progress.replace(root/'progress.json')


def synchronized(primary, replica):
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        p = info(primary.client, 'replication')
        r = info(replica.client, 'replication')
        if (p['replication_pending_keys'] == '0' and
                p['replication_epoch_invalidated'] == 'false' and
                r['replica_ready'] == '1' and
                p['primary_epoch'] == r['replica_epoch'] and
                p['primary_offset'] == r['replica_offset']):
            return
        time.sleep(.02)
    raise TimeoutError('replica failed to catch up')


def write_failure(binary, root, worker, disk_root=None):
    directory = (Path(disk_root) if disk_root else root) / f'fault-worker-{worker}'
    server = Server(binary, directory, async_append=worker,
                    file_limit=None if disk_root else 4096)
    filler = directory / 'owned-filler'
    try:
        server.start()
        assert server.client.call('SET', 'committed', 'survives') == b'OK'
        if disk_root:
            # The workflow supplies a private, size-limited tmpfs mount.
            # Refuse an unexpectedly large filesystem before exhausting it.
            import os
            stat = os.statvfs(directory)
            assert stat.f_blocks * stat.f_frsize <= 32 * 1024 * 1024, 'fault mount must be <= 32 MiB'
            try:
                with filler.open('xb', buffering=0) as file:
                    while True:
                        file.write(b'x' * 65536)
            except OSError as exc:
                assert exc.errno == errno.ENOSPC, exc
        failed = False
        try:
            response = server.client.call('SET', 'must-not-ack', 'x' * 8192)
            assert response != b'OK', 'storage failure received a success acknowledgement'
        except (OSError, RuntimeError, ValueError, EOFError):
            failed = True
        assert failed, 'expected a write error or disconnected client'
    finally:
        server.stop(check=False)
        filler.unlink(missing_ok=True)
    # Recovery must preserve previously acknowledged data over two restarts.
    for _ in range(2):
        with Server(binary, directory, async_append=worker) as recovered:
            assert recovered.client.call('GET', 'committed') == b'survives'
            assert recovered.client.call('GET', 'must-not-ack') is None
    return {'worker': worker, 'fault': 'ENOSPC' if disk_root else 'RLIMIT_FSIZE', 'passed': True}


def run(args, report):
    root = Path(args.out).resolve()
    primary = Server(args.bin, root / 'primary', async_append=True, extra=['-replication-feed'])
    password = primary.password
    replica = Server(args.bin, root / 'replica', async_append=True, password=password,
                     extra=['-replicaof', f'127.0.0.1:{primary.port}',
                            '-primary-password-env', 'KEEL_VALIDATION_PASSWORD'])
    for server in [primary, replica]:
        server.env['GODEBUG'] = 'gctrace=1'
    expected, events = {}, deque(maxlen=128)
    hashes, members, scores = {}, set(), {}
    large = deque([b'L' * 4096] * 128, maxlen=128)
    pair_latencies = []
    i = 0
    started = time.monotonic()
    next_cycle = started + args.cycle_seconds
    next_check = started + min(30, args.cycle_seconds)
    cycles = 0
    try:
        primary.start()
        assert primary.client.call('SET', 'large-value', b'L' * 1048576) == b'OK'
        assert primary.client.call('RPUSH', 'large-list', *large) == len(large)
        replica.start()
        try:
            replica.client.call('SET', 'forbidden', 'write')
        except RuntimeError as exc:
            assert 'READONLY' in str(exc)
        else:
            raise AssertionError('replica accepted a client write')
        while time.monotonic() - started < args.seconds:
            key = f'cache:{i % 1000}'
            value = f'{i}:'.encode() + b'v' * 256
            request_start = time.monotonic()
            assert primary.client.call('SET', key, value) == b'OK'
            expected[key] = value
            assert primary.client.call('GET', key) == value
            pair_latencies.append((time.monotonic()-request_start)*1000)
            report['acknowledged_writes'] += 1
            if i % 20 == 0:
                event = str(i).encode()
                primary.client.call('RPUSH', 'events', event)
                primary.client.call('LTRIM', 'events', -128, -1)
                events.append(event)
                primary.client.call('SET', 'expiring', 'temporary', 'PX', 50)
                field = f'f:{i % 31}'.encode()
                member = f'm:{i % 127}'.encode()
                primary.client.call('HSET', 'hash', field, value)
                hashes[field] = value
                primary.client.call('SADD', 'set', member)
                members.add(member)
                removed = f'm:{(i+11) % 127}'.encode()
                primary.client.call('SREM', 'set', removed)
                members.discard(removed)
                primary.client.call('ZADD', 'ranking', i, member)
                scores[member] = i
            if i % 500 == 0:
                item = str(i).encode() + b'L' * 4096
                primary.client.call('RPUSH', 'large-list', item)
                primary.client.call('LPOP', 'large-list')
                large.append(item)
            i += 1
            now = time.monotonic()
            if now >= next_check:
                maintenance_start = time.monotonic()
                rewrite(primary.client)
                synchronized(primary, replica)
                verify(primary.client, expected, events)
                verify(replica.client, expected, events)
                verify_collections(primary.client, hashes, members, scores, large)
                verify_collections(replica.client, hashes, members, scores, large)
                ordered = sorted(pair_latencies)
                sample = {'seconds': now-started, 'writes': report['acknowledged_writes'],
                          'cache_set_get_pair_ms': {'count': len(ordered), 'p50': statistics.median(ordered),
                              'p99': ordered[min(len(ordered)-1, int(len(ordered)*.99))], 'max': ordered[-1]},
                          'maintenance_seconds': time.monotonic()-maintenance_start,
                          'primary': info(primary.client, 'persistence'),
                          'replica': info(replica.client, 'replication'),
                          'ps': subprocess.check_output(['ps', '-o', 'pid=,rss=,pcpu=', '-p',
                                f'{primary.process.pid},{replica.process.pid}'], text=True)}
                checkpoint(root, report, sample)
                pair_latencies.clear()
                next_check = time.monotonic() + min(30, args.cycle_seconds)
                print('checkpoint', round(now-started, 1), report['acknowledged_writes'], flush=True)
            if now >= next_cycle:
                recovery_start = time.monotonic()
                cycles += 1
                if cycles % 3 == 0:
                    primary.stop(crash=True)
                    time.sleep(6)
                    try:
                        replica.client.call('GET', key)
                    except RuntimeError as exc:
                        assert 'MASTERDOWN' in str(exc)
                    else:
                        raise AssertionError('replica served stale state after primary outage')
                    primary.start()
                    verify(primary.client, expected, events)
                    verify_collections(primary.client, hashes, members, scores, large)
                    report['primary_crash_recoveries'] += 1
                else:
                    replica.stop(crash=True)
                    replica.start()
                    report['replica_crash_recoveries'] += 1
                synchronized(primary, replica)
                verify(replica.client, expected, events)
                verify_collections(replica.client, hashes, members, scores, large)
                with (root/'recoveries.jsonl').open('a') as output:
                    output.write(json.dumps({'cycle': cycles, 'primary_crash': cycles % 3 == 0,
                        'seconds': time.monotonic()-recovery_start, 'acknowledged_cache_values_lost': 0})+'\n')
                next_cycle = time.monotonic() + args.cycle_seconds
            time.sleep(.002)
        # Quiesce and fence the primary before promoting the fully applied replica.
        time.sleep(.08)
        assert primary.client.call('GET', 'expiring') is None
        synchronized(primary, replica)
        verify(replica.client, expected, events)
        verify_collections(replica.client, hashes, members, scores, large)
        primary.stop()
        replica.stop()
        with Server(args.bin, root / 'replica', async_append=True) as promoted:
            verify(promoted.client, expected, events)
            verify_collections(promoted.client, hashes, members, scores, large)
            assert promoted.client.call('SET', 'promotion', 'writable') == b'OK'
        with Server(args.bin, root / 'replica', async_append=True) as restarted:
            assert restarted.client.call('GET', 'promotion') == b'writable'
            verify(restarted.client, expected, events)
            verify_collections(restarted.client, hashes, members, scores, large)
        report['manual_promotion'] = True
        report['elapsed_seconds'] = time.monotonic() - started
    finally:
        primary.stop(check=False)
        replica.stop(check=False)


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--bin', required=True)
    parser.add_argument('--out', required=True)
    parser.add_argument('--seconds', type=float, default=900)
    parser.add_argument('--cycle-seconds', type=float, default=60)
    parser.add_argument('--fault-only', action='store_true')
    parser.add_argument('--disk-root')
    args = parser.parse_args()
    if args.seconds <= 0 or args.cycle_seconds <= 0:
        parser.error('durations must be positive')
    os.umask(0o077)
    root = Path(args.out).resolve()
    assert not root.exists(), 'use a fresh evidence directory'
    root.mkdir(parents=True)
    report = {'status': 'running', 'platform': platform.platform(), 'binary_sha256': sha256(args.bin),
              'harness_sha256': sha256(__file__), 'seconds_requested': args.seconds,
              'checkpoint_count': 0,
              'acknowledged_writes': 0, 'primary_crash_recoveries': 0,
              'replica_crash_recoveries': 0, 'checkpoints': [], 'faults': [], 'passed': False}
    try:
        if not args.fault_only:
            run(args, report)
        for worker in [False, True]:
            report['faults'].append(write_failure(args.bin, root, worker, args.disk_root))
        report['passed'] = True
        report['status'] = 'passed'
    except BaseException as exc:
        report['failure'] = repr(exc)
        report['status'] = 'failed'
        raise
    finally:
        (root / 'report.json').write_text(json.dumps(report, indent=2) + '\n')
        (root / 'progress.json').write_text(json.dumps(report, indent=2) + '\n')
