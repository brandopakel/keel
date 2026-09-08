#!/usr/bin/env python3
"""Bounded authenticated soak, restart/outage/promotion and real OS write failures."""
import argparse
import errno
import json
import math
import os
import platform
import signal
import statistics
import subprocess
import time
from collections import deque
from pathlib import Path

from validation_lib import Client, Server, info, rewrite, sha256
from progress_watchdog import ProgressWatchdog


def capture_failed_process(server):
    """Collect evidence before tearing down an already failed owned test run."""
    process = server.process
    if process is None:
        return {'running': False}
    evidence = {'pid': process.pid, 'exit_code': process.poll()}
    if process.poll() is not None:
        return evidence
    probe = None
    try:
        probe = Client('127.0.0.1', server.port)
        probe.socket.settimeout(.5)
        assert probe.call('AUTH', server.password) == b'OK'
        evidence['persistence'] = info(probe, 'persistence')
        evidence['replication'] = info(probe, 'replication')
    except Exception as exc:
        evidence['probe_failure'] = repr(exc)
    finally:
        if probe is not None:
            try:
                probe.close()
            except OSError as exc:
                evidence['probe_close_failure'] = repr(exc)
    try:
        evidence['process'] = subprocess.check_output(
            ['ps', '-o', 'pid,ppid,state,wchan,pcpu,rss', '-p', str(process.pid)],
            text=True, timeout=2)
    except (OSError, subprocess.SubprocessError) as exc:
        evidence['process_failure'] = repr(exc)
    try:
        # SIGQUIT writes Go goroutine stacks to the existing server log and
        # terminates this disposable process. Successful runs never take it.
        process.send_signal(signal.SIGQUIT)
        process.wait(timeout=3)
        evidence['stack_dump'] = str(server.directory / 'server.log')
    except (OSError, subprocess.SubprocessError) as exc:
        evidence['stack_failure'] = repr(exc)
    return evidence


def verify(client, expected, events):
    for key, value in expected.items():
        assert client.call('GET', key) == value, key
    assert client.call('LRANGE', 'events', 0, -1) == list(events)


def verify_collections(client, hashes, members, scores, large):
    raw = client.call('HGETALL', 'hash')
    assert len(raw) == 2 * len(hashes)
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


def write_failure(binary, root, worker, disk_root=None, concurrent=False):
    directory = (Path(disk_root) if disk_root else root) / f'fault-worker-{worker}'
    server = Server(binary, directory, async_append=worker,
                    file_limit=None if disk_root else 4096,
                    extra=['-aof-concurrent-append'] if worker and concurrent else ())
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
    return {'worker': worker, 'concurrent': worker and concurrent, 'fault': 'ENOSPC' if disk_root else 'RLIMIT_FSIZE', 'passed': True}


def process_sample(*pids):
    """RSS and CPU for the servers under test, as a diagnostic.

    Never raises. A measurement that can end the run it is measuring is worse
    than a missing measurement, and this one did: a 48-hour soak died at 8.1
    hours because ps took longer than its timeout on a loaded machine. The
    servers were healthy and the eight hours were thrown away.

    Failures of the servers themselves still end the run. The INFO calls beside
    this one are deliberately not guarded, because a primary that cannot answer
    INFO is a result rather than a missing sample.
    """
    try:
        return subprocess.check_output(
            ['ps', '-o', 'pid=,rss=,pcpu=', '-p', ','.join(str(pid) for pid in pids)],
            text=True, timeout=15)
    except (OSError, subprocess.SubprocessError) as exc:
        return f'unavailable: {exc!r}'


class GrowthBounds:
    """Fails a soak the moment something grows without bound.

    A long run was the only thing checking this, and it checked it badly: the
    September 6 ratchet - a replica whose log never compacted because restart
    reset its growth baseline - took two and a half hours to surface, and
    surfaced as a readiness timeout rather than as the growth it was. Nothing
    said "the log is not being compacted"; something eventually fell over.

    Bounding it directly is both faster and more specific. A ratchet fails here
    within a few cycles, and says which quantity ran away.

    The bound is on what a steady-state workload should hold flat across
    recovery cycles, not on any absolute size: this workload writes to a fixed
    key range, so the log, the resident set and the descriptor count should all
    settle. Growth proportional to elapsed time is the defect.
    """

    def __init__(self, tolerance):
        self.tolerance = tolerance
        self.settled = {}
        self.breaches = []

    def observe(self, cycle, values):
        """values: name -> current measurement. Returns a list of breach dicts."""
        # The first few cycles are the workload filling up, not steady state.
        if cycle < 4:
            for name, value in values.items():
                self.settled[name] = max(self.settled.get(name, 0), value)
            return []
        found = []
        for name, value in values.items():
            baseline = self.settled.get(name, 0)
            if baseline > 0 and value > baseline * self.tolerance:
                found.append({'quantity': name, 'settled': baseline, 'now': value,
                              'cycle': cycle, 'tolerance': self.tolerance})
        self.breaches.extend(found)
        return found


def growth_values(primary, replica):
    """What must stay flat: the two logs, and the descriptors each server holds.

    Resident set is deliberately absent. It moves with allocator and GC
    behaviour that is not a leak, so bounding it here would produce failures
    nobody can act on. The logs and descriptors have no such excuse.
    """
    values = {}
    for name, server in (('primary_aof', primary), ('replica_aof', replica)):
        log = server.directory / 'store.aof'
        try:
            values[name] = log.stat().st_size
        except OSError:
            continue
    for name, server in (('primary_tmpfiles', primary), ('replica_tmpfiles', replica)):
        try:
            values[name] = sum(1 for entry in server.directory.iterdir()
                               if entry.name.startswith('.') or entry.name.endswith('.rewrite'))
        except OSError:
            continue
    return values


def run(args, report, watchdog):
    root = Path(args.out).resolve()
    replication_flags = ['-replication-protocol', str(args.replication_protocol)]
    primary = Server(args.bin, root / 'primary', async_append=True,
                     extra=['-replication-feed'] + replication_flags + (['-aof-concurrent-append'] if args.concurrent else []))
    password = primary.password
    replica = Server(args.bin, root / 'replica', async_append=True, password=password,
                     extra=['-replicaof', f'127.0.0.1:{primary.port}',
                            '-primary-password-env', 'KEEL_VALIDATION_PASSWORD'] + replication_flags +
                           (['-aof-concurrent-append'] if args.concurrent else []))
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
    growth = GrowthBounds(args.growth_tolerance)
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
                          'ps': process_sample(primary.process.pid, replica.process.pid)}
                checkpoint(root, report, sample)
                watchdog.beat("workload after checkpoint")
                pair_latencies.clear()
                next_check = time.monotonic() + min(30, args.cycle_seconds)
                print('checkpoint', round(now-started, 1), report['acknowledged_writes'], flush=True)
            if now >= next_cycle:
                recovery_start = time.monotonic()
                cycles += 1
                crash_primary = args.primary_crash_every > 0 and cycles % args.primary_crash_every == 0
                if crash_primary:
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
                    output.write(json.dumps({'cycle': cycles, 'primary_crash': crash_primary,
                        'seconds': time.monotonic()-recovery_start, 'acknowledged_cache_values_lost': 0})+'\n')
                breaches = growth.observe(cycles, growth_values(primary, replica))
                if breaches:
                    report['growth_breaches'] = growth.breaches
                    raise AssertionError(
                        'unbounded growth across recovery cycles: ' + '; '.join(
                            f"{b['quantity']} settled at {b['settled']} and is now {b['now']} "
                            f"at cycle {b['cycle']}" for b in breaches))
                report['growth_settled'] = growth.settled
                next_cycle = time.monotonic() + args.cycle_seconds
                watchdog.beat("workload after recovery")
            time.sleep(.002)
        watchdog.beat("final verification and promotion")
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
    except BaseException:
        report['failure_diagnostics'] = {
            'primary': capture_failed_process(primary),
            'replica': capture_failed_process(replica),
        }
        raise
    finally:
        primary.stop(check=False)
        replica.stop(check=False)


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--concurrent', action='store_true', help='exercise bounded concurrent appends on both servers')
    parser.add_argument('--replication-protocol', type=int, choices=[1, 2], default=1)
    parser.add_argument('--bin', required=True)
    parser.add_argument('--out', required=True)
    parser.add_argument('--seconds', type=float, default=900)
    parser.add_argument('--cycle-seconds', type=float, default=60)
    parser.add_argument('--primary-crash-every', type=int, default=3,
                        help='crash primary every N recovery cycles; 0 keeps it alive to measure long-uptime growth')
    parser.add_argument('--progress-timeout', type=float, default=120,
                        help='fail with diagnostics if checkpoints/recovery stop progressing')
    parser.add_argument('--growth-tolerance', type=float, default=3.0,
                        help='fail if a log or descriptor count exceeds this multiple of its '
                             'settled value across recovery cycles; 0 disables the check')
    parser.add_argument('--fault-only', action='store_true')
    parser.add_argument('--disk-root')
    args = parser.parse_args()
    if (not math.isfinite(args.seconds) or not math.isfinite(args.cycle_seconds) or
            args.seconds <= 0 or args.cycle_seconds <= 0 or args.primary_crash_every < 0 or
            not math.isfinite(args.progress_timeout) or args.progress_timeout <= 0):
        parser.error('durations must be finite and positive; primary-crash-every must be nonnegative')
    os.umask(0o077)
    root = Path(args.out).resolve()
    assert not root.exists(), 'use a fresh evidence directory'
    root.mkdir(parents=True)
    report = {'status': 'running', 'platform': platform.platform(), 'binary_sha256': sha256(args.bin),
              'harness_sha256': sha256(__file__), 'seconds_requested': args.seconds,
              'started_utc': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()),
              'primary_crash_every': args.primary_crash_every, 'concurrent': args.concurrent,
              'replication_protocol': args.replication_protocol,
              'progress_timeout_seconds': args.progress_timeout,
              'checkpoint_count': 0,
              'acknowledged_writes': 0, 'primary_crash_recoveries': 0,
              'replica_crash_recoveries': 0, 'checkpoints': [], 'faults': [], 'passed': False}
    (root / 'progress.json').write_text(json.dumps(report, indent=2) + '\n')
    try:
        with (root/'watchdog.log').open('w') as trace, ProgressWatchdog(args.progress_timeout, trace) as watchdog:
            if not args.fault_only:
                run(args, report, watchdog)
            for worker in [False, True]:
                watchdog.beat(f'write failure worker={worker}')
                report['faults'].append(write_failure(args.bin, root, worker, args.disk_root, args.concurrent))
        report['passed'] = True
        report['status'] = 'passed'
    except BaseException as exc:
        report['failure'] = repr(exc)
        report['status'] = 'failed'
        raise
    finally:
        (root / 'report.json').write_text(json.dumps(report, indent=2) + '\n')
        (root / 'progress.json').write_text(json.dumps(report, indent=2) + '\n')
