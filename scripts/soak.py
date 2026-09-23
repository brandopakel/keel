#!/usr/bin/env python3
"""Bounded authenticated soak, restart/outage/promotion and real OS write failures."""
import argparse
import errno
import json
import math
import os
import platform
import shutil
import signal
import statistics
import subprocess
import time
import sys
import traceback
from collections import deque
from pathlib import Path

from validation_lib import Client, Server, info, rewrite, sha256
from progress_watchdog import ProgressWatchdog


# The server closes a connection whose request it has not answered after
# thirty seconds, checked about once a second, and logs its state as it does.
# Unread bytes must be seen by two sweeps a second apart, so allow for that.
STALLED_CLIENT_TIMEOUT = 30
STALL_SWEEP_SLACK = 8
SWEEP_COUNTERS = ('clients_closed_unanswered', 'clients_closed_unread', 'clients_closed_unreplied',
                  'clients_closed_slow')


def wait_for_stall_sweep(probe, evidence):
    """Give the server its chance to say what it forgot, before it is killed.

    A harness request that timed out three seconds ago is still outstanding
    on the harness's own connection. If the server has stopped serving that
    connection, its sweep will close it and log the connection's state -
    parsed commands, held or deferred run, queued continuation, registered or
    not - within thirty-odd seconds. Both times this happened in September
    2026 the harness had SIGQUITed the server four seconds in, and a goroutine
    dump of an idle loop said nothing. So the harness now waits, polling the
    server's own count of such closures over a fresh connection, and records
    what changed. A server that stops answering the probe too is recorded as
    that."""
    before = info(probe, 'clients')
    evidence['clients_before_sweep'] = before
    deadline = time.monotonic() + STALLED_CLIENT_TIMEOUT + STALL_SWEEP_SLACK
    while time.monotonic() < deadline:
        time.sleep(1)
        now = info(probe, 'clients')
        if any(now.get(field) != before.get(field) for field in SWEEP_COUNTERS):
            evidence['clients_after_sweep'] = now
            for field in SWEEP_COUNTERS:
                evidence['sweep_' + field.removeprefix('clients_')] = (
                    int(now.get(field, 0)) - int(before.get(field, 0)))
            return
    evidence['clients_after_sweep'] = info(probe, 'clients')
    for field in SWEEP_COUNTERS:
        evidence['sweep_' + field.removeprefix('clients_')] = 0


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
        probe.socket.settimeout(2)
        assert probe.call('AUTH', server.password) == b'OK'
        evidence['persistence'] = info(probe, 'persistence')
        evidence['replication'] = info(probe, 'replication')
        try:
            wait_for_stall_sweep(probe, evidence)
        except Exception as exc:
            evidence['sweep_wait_failure'] = repr(exc)
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


def verify_served(client):
    """The loop closed nobody for a request it never answered.

    A connection the server has stopped serving is what the stalled 48-hour
    run looked like from outside. The server now closes such a connection
    within thirty seconds and logs its state; this turns that log line into a
    failed soak at the next checkpoint instead of a silence found hours later.
    Two shapes are asserted: a request parsed and never answered, and a request
    whose bytes were never read at all. Slow readers are the client's doing and
    are reported, not asserted."""
    stats = info(client, 'clients')
    for field, what in (('clients_closed_unanswered', 'an unanswered request'),
                        ('clients_closed_unread', 'a request it never read'),
                        ('clients_closed_unreplied', 'a request that ran and produced no reply')):
        # The soak always runs the binary it just built, so a missing counter
        # means the check is not there, and that is not a pass.
        assert field in stats, f"INFO clients has no {field}"
        assert stats[field] == '0', (
            f"server closed {stats[field]} connection(s) with {what}; its server.log names them")
    return {'closed_slow': int(stats['clients_closed_slow']),
            'closed_unanswered': int(stats['clients_closed_unanswered']),
            'closed_unread': int(stats['clients_closed_unread']),
            'closed_unreplied': int(stats['clients_closed_unreplied'])}


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

    A log has one more property, which the first nine nightly runs taught by
    failing every one of them here. It is not flat below the size the server
    lets it reach before compacting (see compaction_ceiling). Under protocol 2
    the replica keeps its log across restarts, so at the 64 MiB default it
    climbed straight through three times a 15 MiB baseline without the server
    ever having promised otherwise; a baseline recorded below the ceiling is the
    point the workload had reached, not anything it settles to. The primary
    showed the other half of the same problem: its log swings between the
    compacted size after each checkpoint rewrite and that plus thirty seconds
    of appends before the next, the early cycles sampled the trough, later ones
    the peak, and 3.0 was the ratio between them.

    So each quantity has a floor under its settled value. For the logs it is
    the compaction ceiling, measured from the live data as the run goes; the
    soak sets the minimum size low enough that compaction actually happens
    within the run, hundreds of times rather than never. The swing between
    rewrites sits well below the ceiling, and a log that stops compacting still
    crosses three times it within a few cycles. Temporary files have a small
    fixed floor: a quantity that settles at zero would otherwise never be
    judged at all, which is what the first version did with them.
    """

    def __init__(self, tolerance, floors=(), warmup_cycles=3, settled=None, informational=()):
        self.tolerance = tolerance
        self.floors = dict(floors)
        self.warmup_cycles = warmup_cycles
        self.settled = dict(settled or {})
        # Recorded alongside the rest but never judged.
        self.informational = frozenset(informational)
        self.breaches = []
        # Cycles judged against the bound. A run that never gets past warm-up has
        # not checked growth, whatever its report says.
        self.judged_cycles = 0

    def baseline(self, name):
        return max(self.settled.get(name, 0), self.floors.get(name, 0))

    def raise_floor(self, name, value):
        """Floors only rise: a ceiling that fell with the live data would let a
        later segment accept growth an earlier one would have failed."""
        self.floors[name] = max(self.floors.get(name, 0), value)

    def observe(self, cycle, values):
        """values: name -> current measurement. Returns a list of breach dicts."""
        # The first few cycles are the workload filling up, not steady state.
        if cycle <= self.warmup_cycles:
            for name, value in values.items():
                self.settled[name] = max(self.settled.get(name, 0), value)
            return []
        self.judged_cycles += 1
        found = []
        for name, value in values.items():
            if name in self.informational:
                continue
            baseline = self.baseline(name)
            if baseline > 0 and value > baseline * self.tolerance:
                found.append({'quantity': name, 'settled': self.settled.get(name, 0),
                              'floor': self.floors.get(name, 0), 'baseline': baseline,
                              'now': value, 'cycle': cycle, 'tolerance': self.tolerance})
        self.breaches.extend(found)
        return found

    def describe(self):
        return {'tolerance': self.tolerance, 'floors': self.floors, 'warmup_cycles': self.warmup_cycles,
                'informational': sorted(self.informational), 'settled': self.settled,
                'judged_cycles': self.judged_cycles, 'breaches': self.breaches}


def parse_size(text):
    """Bytes for a size the server accepts on its command line, such as 8mb."""
    lowered = str(text).strip().lower()
    for suffix, unit in (('kb', 1 << 10), ('mb', 1 << 20), ('gb', 1 << 30)):
        if lowered.endswith(suffix):
            lowered = lowered[:-len(suffix)].strip()
            break
    else:
        unit = 1
    if not lowered.isdigit():
        raise ValueError(f'unparseable size {text!r}')
    return int(lowered) * unit


TORN_TAIL_PREFIX = '.keel-torn-tail-'
TORN_TAIL_COUNTS = ('primary_torn_tails', 'replica_torn_tails')
# What a healthy server may have beside its log at once: one rewrite in flight
# and one checkpoint being renamed into place.
TMPFILE_FLOOR = 2


def compaction_ceiling(client, min_size):
    """The largest a log may legitimately be before the server compacts it.

    Compaction starts once the log reaches -auto-aof-rewrite-min-size and has
    doubled against its base. After a restart that base is not the compacted
    size but a conservative estimate of three times the live data
    (internal/core/aof.go), so a server restarted often - this soak restarts
    the replica every cycle - waits for max(min size, six times live) before
    compacting. Below that, growth is the contract rather than a defect.
    """
    used = int(info(client, 'memory')['used_memory'])
    return max(min_size, 2 * 3 * used)


def growth_values(primary, replica):
    """What must stay flat: the two logs, and the temporary files beside them.

    Resident set is deliberately absent. It moves with allocator and GC
    behaviour that is not a leak, so bounding it here would produce failures
    nobody can act on. The logs and temporary files have no such excuse.

    Torn-tail backups are counted but not bounded. The server keeps one beside
    the log for each crash that tore its last record, as documented; they are
    evidence a crash left, and this soak crashes servers on purpose.
    """
    values = {}
    for prefix, server in (('primary', primary), ('replica', replica)):
        try:
            values[f'{prefix}_aof'] = (server.directory / 'store.aof').stat().st_size
        except OSError:
            pass
        try:
            names = [entry.name for entry in server.directory.iterdir()]
        except OSError:
            continue
        values[f'{prefix}_torn_tails'] = sum(1 for name in names if name.startswith(TORN_TAIL_PREFIX))
        values[f'{prefix}_tmpfiles'] = sum(
            1 for name in names
            if (name.startswith('.') and not name.startswith(TORN_TAIL_PREFIX)) or name.endswith('.rewrite'))
    return values


HANDOFF = 'handoff.json'
SERVER_DIRECTORIES = ('primary', 'replica')


def encode_state(expected, events, hashes, members, scores, large):
    """The harness's model of the keyspace, as JSON can carry it.

    Everything the workload writes is ASCII, so Latin-1 round-trips the bytes
    exactly and the file stays readable."""
    text = lambda value: value.decode('latin-1')
    return {'expected': {key: text(value) for key, value in expected.items()},
            'events': [text(event) for event in events],
            'hashes': {text(field): text(value) for field, value in hashes.items()},
            'members': sorted(text(member) for member in members),
            'scores': {text(member): score for member, score in scores.items()},
            'large': [text(item) for item in large]}


def decode_state(state):
    raw = lambda value: value.encode('latin-1')
    return ({key: raw(value) for key, value in state['expected'].items()},
            deque((raw(event) for event in state['events']), maxlen=128),
            {raw(field): raw(value) for field, value in state['hashes'].items()},
            {raw(member) for member in state['members']},
            {raw(member): score for member, score in state['scores'].items()},
            deque((raw(item) for item in state['large']), maxlen=128))


def load_handoff(args):
    """The previous segment's state, checked before anything is built on it."""
    previous = Path(args.resume).resolve()
    handoff = json.loads((previous / HANDOFF).read_text())
    if not handoff.get('passed'):
        raise AssertionError(f'{previous / HANDOFF} does not record a passed segment')
    if handoff['segment'] != args.segment - 1 or handoff['segments'] != args.segments:
        raise AssertionError(f"handoff is segment {handoff['segment']} of {handoff['segments']}, "
                             f"not the predecessor of segment {args.segment} of {args.segments}")
    for field in ('replication_protocol', 'concurrent', 'primary_crash_every', 'auto_rewrite_min_size'):
        if handoff[field] != getattr(args, field):
            raise AssertionError(f'handoff {field}={handoff[field]!r} differs from this segment')
    # The whole run validates one binary, or it validates nothing in particular.
    if handoff['binary_sha256'] != sha256(args.bin):
        raise AssertionError('this segment would run a different binary from the one handed off')
    return previous, handoff


def inherit_directories(previous, root):
    """Carry both servers' on-disk state into this segment's evidence directory.

    Everything but the previous server logs: those belong to the segment that
    wrote them and are in its artifact."""
    for name in SERVER_DIRECTORIES:
        source, target = previous / name, root / name
        target.mkdir(parents=True)
        for entry in source.iterdir():
            if entry.name == 'server.log' or entry.is_dir():
                continue
            shutil.copy2(entry, target / entry.name)


def run(args, report, watchdog):
    root = Path(args.out).resolve()
    final = args.segment == args.segments
    resumed = args.segment > 1
    # Both logs compact at this floor rather than the 64 MiB default, so a
    # multi-hour run exercises compaction hundreds of times instead of never,
    # and the growth bound has a floor the server actually promises.
    floor = parse_size(args.auto_rewrite_min_size)
    shared_flags = ['-replication-protocol', str(args.replication_protocol),
                    '-auto-aof-rewrite-min-size', args.auto_rewrite_min_size]
    if args.concurrent:
        shared_flags.append('-aof-concurrent-append')
    ports = {}
    if resumed:
        previous, handoff = load_handoff(args)
        inherit_directories(previous, root)
        # A segment boundary is a planned restart of both servers, not a
        # reconfiguration, so the ports carry over. The replica still takes a
        # full snapshot here: its checkpoint names the primary's epoch, and the
        # primary's history does not survive its own restart.
        ports = handoff['ports']
    primary = Server(args.bin, root / 'primary', async_append=True, port=ports.get('primary'),
                     extra=['-replication-feed'] + shared_flags)
    password = primary.password
    replica = Server(args.bin, root / 'replica', async_append=True, password=password, port=ports.get('replica'),
                     extra=['-replicaof', f'127.0.0.1:{primary.port}',
                            '-primary-password-env', 'KEEL_VALIDATION_PASSWORD'] + shared_flags)
    for server in [primary, replica]:
        server.env['GODEBUG'] = 'gctrace=1'
    floors = {'primary_aof': floor, 'replica_aof': floor,
              'primary_tmpfiles': TMPFILE_FLOOR, 'replica_tmpfiles': TMPFILE_FLOOR}
    if resumed:
        expected, events, hashes, members, scores, large = decode_state(handoff['state'])
        i, cycles = handoff['i'], handoff['cycles']
        # The baseline was settled in the first segment. Settling again here
        # would accept whatever the log had grown to, which is the ratchet the
        # bound exists to catch.
        growth = GrowthBounds(args.growth_tolerance, {**floors, **handoff['growth_floors']},
                              settled=handoff['growth_settled'], informational=TORN_TAIL_COUNTS)
        before = handoff['cumulative']
    else:
        expected, events = {}, deque(maxlen=128)
        hashes, members, scores = {}, set(), {}
        large = deque([b'L' * 4096] * 128, maxlen=128)
        i, cycles = 0, 0
        growth = GrowthBounds(args.growth_tolerance, floors, informational=TORN_TAIL_COUNTS)
        before = {'segments_completed': 0, 'elapsed_seconds': 0, 'acknowledged_writes': 0,
                  'checkpoint_count': 0, 'primary_crash_recoveries': 0,
                  'replica_crash_recoveries': 0, 'replica_compactions': 0}
    pair_latencies = []
    started = time.monotonic()
    next_cycle = started + args.cycle_seconds
    next_check = started + min(30, args.cycle_seconds)

    def roll_up(completed=False):
        if args.segments > 1:
            report['cumulative'] = {
                'segments_completed': before['segments_completed'] + int(completed),
                'elapsed_seconds': before['elapsed_seconds'] + time.monotonic() - started,
                **{name: before[name] + report[name] for name in
                   ('acknowledged_writes', 'checkpoint_count', 'primary_crash_recoveries',
                    'replica_crash_recoveries', 'replica_compactions')}}

    # The replica's rewrite counter describes its open log and restarts with
    # it, so the harness keeps the running total: compaction that happened is
    # a number in the report, not an inference from a log that stayed small.
    replica_rewrites_seen = 0

    def note_replica_rewrites():
        nonlocal replica_rewrites_seen
        current = int(info(replica.client, 'persistence')['aof_rewrites'])
        report['replica_compactions'] += max(0, current - replica_rewrites_seen)
        replica_rewrites_seen = current

    try:
        primary.start()
        if not resumed:
            assert primary.client.call('SET', 'large-value', b'L' * 1048576) == b'OK'
            assert primary.client.call('RPUSH', 'large-list', *large) == len(large)
        replica.start()
        try:
            replica.client.call('SET', 'forbidden', 'write')
        except RuntimeError as exc:
            assert 'READONLY' in str(exc)
        else:
            raise AssertionError('replica accepted a client write')
        if resumed:
            # Everything the previous segment acknowledged has to be on both
            # servers, on this machine, before anything is written on top of it.
            handoff_start = time.monotonic()
            verify(primary.client, expected, events)
            verify_collections(primary.client, hashes, members, scores, large)
            synchronized(primary, replica)
            verify(replica.client, expected, events)
            verify_collections(replica.client, hashes, members, scores, large)
            report['handoff_recovery'] = {'seconds': time.monotonic() - handoff_start,
                                          'replica': info(replica.client, 'replication')}
            roll_up()
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
                served = {'primary': verify_served(primary.client), 'replica': verify_served(replica.client)}
                note_replica_rewrites()
                ordered = sorted(pair_latencies)
                sample = {'seconds': now-started, 'writes': report['acknowledged_writes'],
                          'cache_set_get_pair_ms': {'count': len(ordered), 'p50': statistics.median(ordered),
                              'p99': ordered[min(len(ordered)-1, int(len(ordered)*.99))], 'max': ordered[-1]},
                          'maintenance_seconds': time.monotonic()-maintenance_start,
                          'primary': info(primary.client, 'persistence'),
                          'replica': info(replica.client, 'replication'),
                          'replica_persistence': info(replica.client, 'persistence'),
                          'clients_closed': served,
                          'growth': growth_values(primary, replica),
                          'ps': process_sample(primary.process.pid, replica.process.pid)}
                roll_up()
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
                    note_replica_rewrites()
                    replica.stop(crash=True)
                    replica.start()
                    replica_rewrites_seen = 0
                    report['replica_crash_recoveries'] += 1
                synchronized(primary, replica)
                verify(replica.client, expected, events)
                verify_collections(replica.client, hashes, members, scores, large)
                verify_served(primary.client)
                verify_served(replica.client)
                values = growth_values(primary, replica)
                ceiling = compaction_ceiling(primary.client, floor)
                for name in ('primary_aof', 'replica_aof'):
                    growth.raise_floor(name, ceiling)
                with (root/'recoveries.jsonl').open('a') as output:
                    output.write(json.dumps({'cycle': cycles, 'primary_crash': crash_primary,
                        'seconds': time.monotonic()-recovery_start, 'acknowledged_cache_values_lost': 0,
                        'growth': values})+'\n')
                breaches = growth.observe(cycles, values)
                report['growth'] = growth.describe()
                if breaches:
                    raise AssertionError(
                        'unbounded growth across recovery cycles: ' + '; '.join(
                            f"{b['quantity']} is {b['now']} at cycle {b['cycle']}, over {b['tolerance']}x "
                            f"its baseline {b['baseline']} (settled {b['settled']}, floor {b['floor']})"
                            for b in breaches))
                next_cycle = time.monotonic() + args.cycle_seconds
                watchdog.beat("workload after recovery")
            time.sleep(.002)
        watchdog.beat("final verification and promotion" if final else "segment handoff")
        # Quiesce and fence the primary before promoting the fully applied replica.
        time.sleep(.08)
        assert primary.client.call('GET', 'expiring') is None
        synchronized(primary, replica)
        verify(replica.client, expected, events)
        verify_collections(replica.client, hashes, members, scores, large)
        note_replica_rewrites()
        report['growth'] = growth.describe()
        if growth.tolerance > 0 and growth.judged_cycles == 0:
            # A pass that never judged a cycle has not checked growth. The
            # first version of the bound was validated by exactly such a run.
            raise AssertionError(
                f'growth bounds never engaged: {cycles} recovery cycles ran and the first '
                f'{growth.warmup_cycles} are warm-up; lengthen --seconds or shorten --cycle-seconds')
        ports = {'primary': primary.port, 'replica': replica.port}
        primary.stop()
        replica.stop()
        handoff = None
        if final:
            with Server(args.bin, root / 'replica', async_append=True) as promoted:
                verify(promoted.client, expected, events)
                verify_collections(promoted.client, hashes, members, scores, large)
                assert promoted.client.call('SET', 'promotion', 'writable') == b'OK'
            with Server(args.bin, root / 'replica', async_append=True) as restarted:
                assert restarted.client.call('GET', 'promotion') == b'writable'
                verify(restarted.client, expected, events)
                verify_collections(restarted.client, hashes, members, scores, large)
            report['manual_promotion'] = True
        else:
            handoff = {'segment': args.segment, 'segments': args.segments, 'i': i, 'cycles': cycles,
                       'ports': ports, 'growth_settled': growth.settled, 'growth_floors': growth.floors,
                       'replication_protocol': args.replication_protocol, 'concurrent': args.concurrent,
                       'primary_crash_every': args.primary_crash_every,
                       'auto_rewrite_min_size': args.auto_rewrite_min_size,
                       'binary_sha256': report['binary_sha256'],
                       'state': encode_state(expected, events, hashes, members, scores, large)}
        report['elapsed_seconds'] = time.monotonic() - started
        roll_up(completed=True)
        if handoff is not None:
            handoff['cumulative'] = report['cumulative']
        return handoff
    except BaseException:
        # Timestamp before diagnostic queries/SIGQUIT, which observe a later state.
        report['failure_observed_unix_seconds'] = time.time()
        report['elapsed_seconds'] = time.monotonic() - started
        roll_up()
        report['failure_stack'] = [
            {'file': Path(frame.filename).name, 'line': frame.lineno, 'function': frame.name}
            for frame in traceback.extract_tb(sys.exc_info()[2])[-32:]
        ]
        # Each capture may wait out the server's stall sweep; keep the
        # watchdog from ending the diagnostics it exists to produce.
        watchdog.beat("failure diagnostics, primary")
        diagnostics = {'primary': capture_failed_process(primary)}
        watchdog.beat("failure diagnostics, replica")
        diagnostics['replica'] = capture_failed_process(replica)
        report['failure_diagnostics'] = diagnostics
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
    parser.add_argument('--auto-rewrite-min-size', default='8mb',
                        help='-auto-aof-rewrite-min-size for both servers, and the floor under a '
                             "log's settled value; low enough that compaction happens within the run")
    parser.add_argument('--segment', type=int, default=1,
                        help='this run is segment N of a longer run continued across machines')
    parser.add_argument('--segments', type=int, default=1, help='segments in the whole run')
    parser.add_argument('--resume', help="the previous segment's evidence directory; required after segment 1")
    parser.add_argument('--fault-only', action='store_true')
    parser.add_argument('--disk-root')
    args = parser.parse_args()
    if (not math.isfinite(args.seconds) or not math.isfinite(args.cycle_seconds) or
            args.seconds <= 0 or args.cycle_seconds <= 0 or args.primary_crash_every < 0 or
            not math.isfinite(args.progress_timeout) or args.progress_timeout <= 0):
        parser.error('durations must be finite and positive; primary-crash-every must be nonnegative')
    if not 1 <= args.segment <= args.segments:
        parser.error('--segment must be between 1 and --segments')
    if (args.segment > 1) != (args.resume is not None):
        parser.error('--resume is required after segment 1 and meaningless for the first')
    try:
        parse_size(args.auto_rewrite_min_size)
    except ValueError as exc:
        parser.error(str(exc))
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
              'auto_rewrite_min_size': args.auto_rewrite_min_size,
              'segment': args.segment, 'segments': args.segments, 'resumed_from': args.resume,
              'checkpoint_count': 0, 'replica_compactions': 0,
              'acknowledged_writes': 0, 'primary_crash_recoveries': 0,
              'replica_crash_recoveries': 0, 'checkpoints': [], 'faults': [], 'passed': False}
    (root / 'progress.json').write_text(json.dumps(report, indent=2) + '\n')
    handoff = None
    try:
        with (root/'watchdog.log').open('w') as trace, ProgressWatchdog(args.progress_timeout, trace) as watchdog:
            if not args.fault_only:
                handoff = run(args, report, watchdog)
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
        # The next segment builds on this one only if everything here passed,
        # including the storage-fault checks that ran after the soak itself.
        if handoff is not None and report['passed']:
            handoff['passed'] = True
            (root / HANDOFF).write_text(json.dumps(handoff) + '\n')
        print(json.dumps({key: report.get(key) for key in (
            'status', 'segment', 'segments', 'acknowledged_writes', 'primary_crash_recoveries',
            'replica_crash_recoveries', 'replica_compactions', 'cumulative')}), flush=True)
