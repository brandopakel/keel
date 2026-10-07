#!/usr/bin/env python3
"""Check that two builds replicate to each other, both ways, byte for byte.

Step 2.4 of the embedding plan (docs/embedding-plan.md) moves replication and
failover into the engine without changing anything a peer can see. This runs a
baseline build and a candidate build against each other over replication
protocols 1 and 2, and checks:

- wire bytes: each build, pulled from as a replica pulls, serves the same
  frames for the same writes - deltas, snapshots and protocol 1 key images -
  and the same KEEL.DUMP images and INFO replication fields, once epochs,
  snapshot identities and checksums are named rather than compared, and the
  records are normalized as check-log-compatibility.py normalizes a log
  (relative expiries, map order, a collection a rewrite cut in several) and
  each key's records in a protocol 1 frame, or in a run of opaque images, are
  put in key order;
- replication across builds, in both directions: a baseline primary feeding a
  candidate replica and a candidate primary feeding a baseline replica, over
  each protocol. Each pair takes a full sync (protocol 2: a snapshot), a delta
  stream of every family with MULTI/EXEC blocks, acknowledgements that reach
  the primary's stream end, a dropped connection resumed from the replica's
  cursor, a replica crash restarted on the other build (protocol 2: from the
  restart checkpoint the first build wrote), and a primary restart, which
  starts a new epoch. After each the replica's keyspace must be the
  primary's, and the same in every pair;
- the replica's log: under protocol 2 every pair's replica writes the same log
  once normalized as a protocol 2 frame is, and under both protocols each
  build replays every replica's log to that replica's keyspace;
- terms: term files each build writes load on the other, with the same
  replies to KEEL.PROMOTE, KEEL.FENCE and writes, the same INFO failover
  fields and the same file bytes; and a protocol 2 replica of either build
  learns a primary's term from its frames.

A failure leaves every log and server log under --out, with the report.
"""
import argparse
import base64
import importlib.util
import json
import platform
import subprocess
import time
from pathlib import Path

from validation_lib import Server, info, sha256

spec = importlib.util.spec_from_file_location(
    'check_log_compatibility', Path(__file__).with_name('check-log-compatibility.py'))
compat = importlib.util.module_from_spec(spec)
spec.loader.exec_module(compat)
_link_spec = importlib.util.spec_from_file_location(
    'check_replication_v2', Path(__file__).with_name('check-replication-v2.py'))
_v2 = importlib.util.module_from_spec(_link_spec)
_link_spec.loader.exec_module(_v2)

Error, call = compat.Error, compat.call
PROTOCOLS = [1, 2]
SIDES = ['baseline', 'candidate']
# Fields INFO replication reports that are random or are ages.
INFO_RANDOM = {'primary_epoch', 'replica_epoch'}
INFO_AGES = {'replica_last_update_ms', 'replication_acked_age_ms'}
OPAQUE = (b'KEEL.RESTORE',)


def other(side):
    return 'candidate' if side == 'baseline' else 'baseline'


def wait_for(what, check, timeout=30):
    deadline = time.monotonic() + timeout
    while True:
        result = check()
        if result:
            return result
        if time.monotonic() > deadline:
            raise TimeoutError(what)
        time.sleep(.02)


def caught_up(primary, replica, protocol, timeout=30):
    """The replica is ready, in the primary's epoch, at the primary's offset,
    with nothing the primary has not yet sealed (protocol 1)."""
    def check():
        p, r = info(primary.client, 'replication'), info(replica.client, 'replication')
        level = (r['replica_ready'] == '1' and p['primary_epoch'] == r['replica_epoch']
                 and p['primary_offset'] == r['replica_offset'])
        if protocol == 1:
            level = level and p['replication_pending_keys'] == '0'
        return (p, r) if level else None
    return wait_for(f'protocol {protocol} replica did not catch up', check, timeout)


def acknowledged(primary):
    """Protocol 2: a pull after catching up acknowledges the stream's end."""
    def check():
        p = info(primary.client, 'replication')
        if (p['replication_acked_offset'] == p['primary_offset'] and p['replication_lag_bytes'] == '0'
                and int(p['replication_acked_age_ms']) >= 0):
            return p
        return None
    return wait_for('the replica did not acknowledge the stream end', check)


def deltas(c):
    """Every operation a stream carries, the records written in a command's
    place, opaque images, a key reaped lazily, and transactions: writes with an
    image inside, reads only, a command that fails, and a discarded one."""
    replies = []
    def do(*parts):
        replies.append(call(c, *parts))
    do('SET', 's:plain', 'changed'); do('SET', 'd:new', 'v', 'EX', compat.hours(3))
    do('SETNX', 'd:nx', 'a'); do('MSET', 'm:1', 'c', 'm:3', 'd')
    do('INCR', 'c'); do('INCRBY', 'c', 5); do('DECR', 'c'); do('DECRBY', 'c', 2)
    do('EXPIRE', 'm:3', compat.hours(4)); do('PERSIST', 's:ex'); do('DEL', 's:pxat'); do('UNLINK', 's:exat')
    do('HSET', 'h', 'f9', 'v9'); do('HSETNX', 'h', 'f8', 'v8'); do('HDEL', 'h', 'f1'); do('HINCRBY', 'h', 'n', 7)
    do('LPUSH', 'l', 'p'); do('RPUSH', 'l', 'r'); do('LPOP', 'l'); do('RPOP', 'l'); do('LSET', 'l', 0, 'q')
    do('LTRIM', 'l', 0, 2); do('SADD', 'set', 'x', 'y'); do('SREM', 'set', 'a')
    do('SADD', 'd:one', 'only'); do('SPOP', 'd:one')
    do('ZADD', 'z', 7, 'n'); do('ZINCRBY', 'z', 2, 'a'); do('ZREM', 'z', 'b'); do('ZPOPMIN', 'z2')
    do('GEOADD', 'geo', '2.3522', '48.8566', 'paris')
    do('BF.ADD', 'bf', 'z'); do('CF.ADD', 'cf', 'z'); do('CMS.INCRBY', 'cms', 'a', 1)
    do('MORRIS.INCRBY', 'mor', 'hits', 10); do('PFADD', 'hll', 'z'); do('PFMERGE', 'hll3', 'hll', 'hll2')
    do('SET', 'd:brief', 'v', 'PX', 1)
    time.sleep(.01)
    do('GET', 'd:brief')
    do('MULTI'); do('SET', 'd:t1', 'a'); do('INCR', 'd:t2'); do('BF.ADD', 'bf', 'in-block'); do('EXEC')
    do('MULTI'); do('GET', 'd:t1'); do('EXEC')
    do('MULTI'); do('SET', 'd:t3', 'a'); do('LPUSH', 'd:t3', 'x'); do('INCR', 'd:t4'); do('EXEC')
    do('MULTI'); do('EXPIRE', 'd:t1', compat.hours(8)); do('SET', 'd:t5', 'v', 'EX', compat.hours(9)); do('EXEC')
    do('MULTI'); do('SET', 'd:t6', 'v'); do('DISCARD')
    return replies


def while_away(c, round_):
    """Writes a replica misses while it is disconnected or down, a transaction
    among them."""
    for i in range(50):
        call(c, 'SET', f'away:{round_}:{i % 10}', i)
        call(c, 'HSET', f'away:h:{round_}', f'f{i % 5}', i)
        call(c, 'INCR', f'away:count:{round_}')
    call(c, 'MULTI'); call(c, 'SET', f'away:tx:{round_}', 'v'); call(c, 'RPUSH', 'l', f'away:{round_}')
    call(c, 'PFADD', 'hll', f'away:{round_}'); call(c, 'EXEC')


def group_key(parts):
    return parts[1] if len(parts) > 1 else b''


def key_groups(records):
    """Protocol 1 seals keys from a map and walks the keyspace for a full
    frame: each key's records stay together and in order, a leading FLUSHDB
    stays first, and the keys are put in order."""
    head = []
    if records and records[0][0].upper() == b'FLUSHDB':
        head, records = records[:1], records[1:]
    groups = []
    for parts in records:
        if groups and group_key(groups[-1][0]) == group_key(parts) and parts[0].upper() != b'DEL':
            groups[-1].append(parts)
        else:
            groups.append([parts])
    groups.sort(key=lambda g: group_key(g[0]))
    return head + [parts for g in groups for parts in g]


def opaque_runs(records):
    """An opaque command publishes the image of every key it names, from a
    map: in each run of consecutive images (DEL, KEEL.RESTORE and PEXPIREAT on
    one key), the keys are put in order."""
    out, run = [], []
    def flush():
        out.extend(key_groups(run))
        run.clear()
    i = 0
    while i < len(records):
        parts = records[i]
        if (parts[0].upper() == b'DEL' and len(parts) == 2 and i + 1 < len(records)
                and records[i + 1][0].upper() in OPAQUE and group_key(records[i + 1]) == parts[1]):
            group = [parts, records[i + 1]]
            i += 2
            if i < len(records) and records[i][0].upper() == b'PEXPIREAT' and group_key(records[i]) == parts[1]:
                group.append(records[i])
                i += 1
            run.extend(group)
            continue
        flush()
        out.append(parts)
        i += 1
    flush()
    return out


def encode(records):
    return b''.join(b'*%d\r\n' % len(p) + b''.join(b'$%d\r\n%s\r\n' % (len(x), x) for x in p) for p in records)


def normalize_body(body, start, kind):
    """A frame's body, normalized: 'snapshot' as a log, 'protocol1' with each
    key's records together, 'protocol2' with runs of opaque images in key
    order."""
    normalized = compat.normalize(body, start)
    if kind == 'snapshot':
        return normalized
    records = compat.records(normalized)
    return encode(key_groups(records) if kind == 'protocol1' else opaque_runs(records))


def settle_snapshot(chunks, snapshot, normalized):
    """The frames a protocol 2 snapshot was sent in, as one header that
    depends only on what the snapshot holds.

    A rewrite cuts a large collection into records at 256 elements, 64 KiB or
    a millisecond, so two runs of one build can send snapshots that differ in
    size while their normalized bodies are the same: run 37550776479 compared
    two identical binaries and failed on a snapshot_bytes 28 bytes apart, one
    record header. So each run's frames are first checked against the
    snapshot they carried: every frame names its whole length, each starts
    where the one before ended, all but the last are the same full size, and
    only the last says it is done. Then the frames are compared as one header
    whose snapshot_bytes is the normalized body's length, without the sizes
    and offsets of the raw cut."""
    total, offset = len(snapshot), 0
    for i, frame in enumerate(chunks):
        if frame.get('snapshot_bytes') != total:
            raise AssertionError(f"snapshot frame {i} says snapshot_bytes {frame.get('snapshot_bytes')}, "
                                 f'but the frames carried {total} bytes')
        if frame.get('snapshot_offset', 0) != offset:
            raise AssertionError(f"snapshot frame {i} starts at {frame.get('snapshot_offset', 0)}, "
                                 f'but the frames before it carried {offset} bytes')
        if i < len(chunks) - 1 and frame['body_bytes'] != chunks[0]['body_bytes']:
            raise AssertionError(f"snapshot frame {i} carries {frame['body_bytes']} bytes, "
                                 f"where the first carried {chunks[0]['body_bytes']}")
        if bool(frame.get('snapshot_done')) != (i == len(chunks) - 1):
            raise AssertionError(f'snapshot frame {i} of {len(chunks)} says snapshot_done '
                                 f"{bool(frame.get('snapshot_done'))}")
        offset += frame['body_bytes']
    if offset != total:
        raise AssertionError(f'the snapshot frames carried {offset} bytes of a {total}-byte snapshot')
    header = {k: v for k, v in chunks[0].items() if k not in ('body_bytes', 'snapshot_offset')}
    header.update(snapshot_bytes=len(normalized), snapshot_done=True)
    return header


class Names:
    """Random identities named by the order they first appear in."""
    def __init__(self):
        self.seen = {}

    def __call__(self, kind, value):
        if not value:
            return value
        key = (kind, value)
        if key not in self.seen:
            self.seen[key] = f'{kind}-{sum(1 for k in self.seen if k[0] == kind) + 1}'
        return self.seen[key]


def frame_of(reply):
    if isinstance(reply, Error):
        raise AssertionError(f'pull refused: {reply.text}')
    return json.loads(reply)


def describe(frame, names, start, kind):
    """A frame as sent, its identities named, without its checksum and body;
    and its body."""
    body = base64.b64decode(frame.get('body') or b'')
    shown = {k: v for k, v in frame.items() if k not in ('body', 'checksum')}
    shown['epoch'] = names('epoch', frame.get('epoch', ''))
    if 'snapshot_id' in frame:
        shown['snapshot_id'] = names('snapshot', frame['snapshot_id'])
    shown['body_bytes'] = len(body)
    return shown, body


def info_fields(client, names):
    out = {}
    for k, v in info(client, 'replication').items():
        if k in INFO_AGES:
            continue
        out[k] = names('epoch', v) if k in INFO_RANDOM else v
    return out


def dumps(c):
    """KEEL.DUMP of every key but a hash, whose image follows its map's order;
    a hash's fields are compared through the keyspace instead."""
    return {key.decode(errors='replace'): compat.sha256_of(call(c, 'KEEL.DUMP', key))
            for key in sorted(call(c, 'KEYS', '*')) if call(c, 'TYPE', key) != b'hash'}


def wire(root, binary, protocol):
    """What one build serves a replica that pulls from it by hand: every frame
    over the workload, a snapshot (protocol 2) or full frame (protocol 1), the
    deltas after it, the images, and INFO replication at each step."""
    names = Names()
    out = {'frames': [], 'info': [], 'replies': None, 'dumps': None}
    bodies = []
    with Server(binary, root, policy='everysec', async_append=True,
                extra=['-replication-feed', '-replication-protocol', str(protocol)]) as s:
        c = s.client
        start = int(time.time() * 1000)
        compat.workload(c)

        def keep(frame):
            """The frame without its body, which is returned."""
            shown, body = describe(frame, names, start, None)
            out['frames'].append(shown)
            return body

        if protocol == 2:
            epoch = info(c, 'replication')['primary_epoch']
            def pull(*parts):
                return frame_of(call(c, 'KEEL.REPL.PULL2', *parts))
            def drain(offset):
                # A record can span frames, so the body is the frames' bodies
                # joined.
                body = b''
                while True:
                    f = pull(epoch, offset, '', 0)
                    body += keep(f)
                    offset = f['to']
                    if f.get('caught_up'):
                        bodies.append(('protocol2', body))
                        return offset
            drain(0)
            out['info'].append(info_fields(c, names))
            first = pull('', 0, '', 0)
            keep(first)
            if not first.get('pending'):
                raise AssertionError('a snapshot starts with a pending frame')
            # How many pulls wait for the rewrite depends on the machine, so
            # only the first is kept.
            chunk = wait_for('the snapshot did not start', lambda: (
                lambda f: None if f.get('pending') else f)(pull('', 0, '', 0)))
            snapshot, part, chunks = b'', 0, []
            while True:
                shown, body = describe(chunk, names, start, None)
                chunks.append(shown)
                snapshot += body
                if chunk.get('snapshot_done'):
                    break
                part += len(body)
                chunk = pull('', 0, chunk['snapshot_id'], part)
            out['frames'].append(settle_snapshot(chunks, snapshot, normalize_body(snapshot, start, 'snapshot')))
            bodies.append(('snapshot', snapshot))
            out['info'].append(info_fields(c, names))
            out['replies'] = compat.sha256_of(deltas(c))
            drain(chunk['to'])
        else:
            full = frame_of(call(c, 'KEEL.REPL.PULL', '', 0))
            bodies.append(('protocol1', keep(full)))
            out['info'].append(info_fields(c, names))
            out['replies'] = compat.sha256_of(deltas(c))
            delta = frame_of(call(c, 'KEEL.REPL.PULL', full['epoch'], full['to']))
            bodies.append(('protocol1', keep(delta)))
        out['info'].append(info_fields(c, names))
        out['dumps'] = dumps(c)
    out['bodies'] = [(kind, normalize_body(body, start, kind)) for kind, body in bodies]
    return out


def compare_wire(a, b, what):
    for i, (x, y) in enumerate(zip(a['frames'], b['frames'])):
        if x != y:
            raise AssertionError(f'{what}: frame {i} differs: baseline {x} candidate {y}')
    if len(a['frames']) != len(b['frames']):
        raise AssertionError(f"{what}: {len(a['frames'])} frames against {len(b['frames'])}")
    for i, ((kind, x), (_, y)) in enumerate(zip(a['bodies'], b['bodies'])):
        if x != y:
            raise AssertionError(f'{what}: body {i} ({kind}) differs: {compat.first_difference(x, y)}')
    for field in ['info', 'replies', 'dumps']:
        if a[field] != b[field]:
            if isinstance(a[field], dict):
                diff = sorted(k for k in set(a[field]) | set(b[field]) if a[field].get(k) != b[field].get(k))
                raise AssertionError(f'{what}: {field} differ: {diff[:10]}')
            raise AssertionError(f'{what}: {field} differ: baseline {a[field]} candidate {b[field]}')


class Pair:
    """A primary of one build and a replica of another, through a relay that
    can drop the connection."""
    def __init__(self, root, protocol, binaries, primary, replica):
        self.root, self.protocol, self.binaries = root, protocol, binaries
        self.sides = {'primary': primary, 'replica': replica}
        self.common = ['-replication-protocol', str(protocol)]
        self.checks, self.states = [], {}
        self.primary = Server(binaries[primary], root / 'primary', policy='everysec', async_append=True,
                              extra=['-replication-feed', *self.common])
        self.link = self.replica = None

    def replica_server(self, side):
        return Server(self.binaries[side], self.root / 'replica', policy='everysec', async_append=True,
                      password=self.primary.password,
                      extra=['-replicaof', f'127.0.0.1:{self.link.port}',
                             '-primary-password-env', 'KEEL_VALIDATION_PASSWORD', *self.common])

    def level(self, step):
        """The replica has caught up and holds the primary's keyspace."""
        p, r = caught_up(self.primary, self.replica, self.protocol)
        want = compat.snapshot(self.primary.client, self.start)
        got = compat.snapshot(self.replica.client, self.start)
        if got != want:
            diff = sorted(k for k in set(got) | set(want) if got.get(k) != want.get(k))
            raise AssertionError(f'{step}: the replica differs from its primary: {diff[:10]}')
        if self.protocol == 2:
            acknowledged(self.primary)
        self.states[step] = want
        self.checks.append(f'{step}: caught up at offset {p["primary_offset"]}, keyspaces identical')
        return p, r

    def run(self):
        try:
            self.primary.start()
            self.start = int(time.time() * 1000)
            compat.workload(self.primary.client)
            self.link = _v2.Link(self.primary.port)
            self.link.disconnect_after = 0
            self.replica = self.replica_server(self.sides['replica']).start()
            self.level('full sync')
            deltas(self.primary.client)
            self.level('deltas')

            # A dropped connection: the replica resumes from its cursor.
            _, before = caught_up(self.primary, self.replica, self.protocol)
            self.link.offline.set()
            time.sleep(.3)
            while_away(self.primary.client, 1)
            time.sleep(.3)
            if info(self.replica.client, 'replication')['replica_offset'] != before['replica_offset']:
                raise AssertionError('the replica moved while disconnected')
            self.link.offline.clear()
            self.level('reconnect')

            # The replica crashes and restarts on the other build.
            self.replica.stop(crash=True)
            while_away(self.primary.client, 2)
            restarted = other(self.sides['replica'])
            self.replica = self.replica_server(restarted).start()
            _, r = self.level('replica restarted on the other build')
            if self.protocol == 2 and r['replica_checkpoint_resumed'] != 'true':
                raise AssertionError(f"the {restarted} did not resume from the {self.sides['replica']}'s checkpoint")
            if self.protocol == 2:
                self.checks.append(f"the {restarted} resumed from the {self.sides['replica']}'s checkpoint")

            # The primary restarts: a new epoch, and a full sync.
            epoch = info(self.primary.client, 'replication')['primary_epoch']
            self.primary.stop()
            self.primary.start()
            if info(self.primary.client, 'replication')['primary_epoch'] == epoch:
                raise AssertionError('a restarted primary kept its epoch')
            while_away(self.primary.client, 3)
            self.level('primary restarted')
            self.replica.stop()
            log = (self.root / 'replica' / 'store.aof').read_bytes()
        finally:
            if self.replica is not None:
                self.replica.stop(check=False)
            if self.link is not None:
                self.link.close()
            self.primary.stop(check=False)
        return log

    def replay(self, log):
        """Each build replays the replica's log, as a primary, to its keyspace."""
        want = self.states['primary restarted']
        for side in SIDES:
            data = self.root / f'{side}-replays-replica'
            data.mkdir()
            (data / 'store.aof').write_bytes(log)
            with Server(self.binaries[side], data) as s:
                got = compat.snapshot(s.client, self.start)
            if got != want:
                diff = sorted(k for k in set(got) | set(want) if got.get(k) != want.get(k))
                raise AssertionError(f'{side} replaying the replica log: keys differ: {diff[:10]}')
        self.checks.append("each build replays the replica's log to its keyspace")


def pairs(root, binaries, protocol):
    """Every primary and replica build, each replica restarting on the other."""
    results, logs, states = [], {}, {}
    for primary in SIDES:
        for replica in SIDES:
            name = f'protocol{protocol}-{primary}-primary-{replica}-replica'
            case = root / name
            case.mkdir()
            pair = Pair(case, protocol, binaries, primary, replica)
            log = pair.run()
            pair.replay(log)
            # Normalized as a protocol 2 frame is: a log's, and opaque images in key order.
            logs[name] = normalize_body(log, pair.start, 'protocol2')
            states[name] = pair.states
            results.append({'name': name, 'passed': True, 'checks': pair.checks,
                            'replica_log_sha256': sha256(case / 'replica' / 'store.aof')})
            print(name, 'passed:', '; '.join(pair.checks), flush=True)
    first = next(iter(states))
    for name in states:
        if states[name] != states[first]:
            steps = [s for s in states[first] if states[first][s] != states[name].get(s)]
            raise AssertionError(f'{name}: keyspaces differ from {first} at {steps}')
    note = f'protocol {protocol}: every pair holds the same keyspace at every step'
    if protocol == 2:
        for name in logs:
            if logs[name] != logs[first]:
                raise AssertionError(f"{name}: the replica's log differs from {first}'s: "
                                     f'{compat.first_difference(logs[first], logs[name])}')
        note += f" and writes the same replica log ({len(logs[first])} bytes normalized)"
    return results, note


def term_file(root):
    path = root / 'store.aof.term'
    return path.read_bytes() if path.exists() else None


def failover_fields(c):
    return {k: v for k, v in info(c, 'replication').items() if k.startswith('failover_') or k == 'writable'}


def terms(root, binaries):
    """Term files written by one build load on the other, with the same replies,
    INFO fields and file bytes; a replica of the other build learns a primary's
    term from its frames."""
    seen = {}
    for first in SIDES:
        second = other(first)
        case = root / f'terms-{first}-then-{second}'
        case.mkdir()
        trace = []
        def step(s, *parts):
            trace.append([list(map(str, parts)), repr(call(s.client, *parts))])
        def note(s, what):
            trace.append([what, failover_fields(s.client), repr(term_file(case / 'primary'))])
        common = ['-replication-protocol', '2']
        primary = Server(binaries[first], case / 'primary', async_append=True,
                         extra=['-replication-feed', *common])
        replica = None
        try:
            primary.start()
            note(primary, 'fresh')
            step(primary, 'KEEL.PROMOTE', 3)
            step(primary, 'SET', 'k', 'v')
            note(primary, 'promoted')
            replica = Server(binaries[second], case / 'replica', async_append=True, password=primary.password,
                             extra=['-replicaof', f'127.0.0.1:{primary.port}',
                                    '-primary-password-env', 'KEEL_VALIDATION_PASSWORD', *common]).start()
            caught_up(primary, replica, 2)
            r = failover_fields(replica.client)
            if r['failover_term'] != '3' or term_file(case / 'replica') != b'3':
                raise AssertionError(f'the {second} replica did not learn term 3: {r}')
            trace.append(['replica learned', r, repr(term_file(case / 'replica'))])
            replica.stop()
            replica = None
            step(primary, 'KEEL.FENCE', 5)
            step(primary, 'SET', 'k', 'w')
            note(primary, 'fenced')
            primary.stop()
            # The other build reads the term file back, and starts fenced.
            primary = Server(binaries[second], case / 'primary', async_append=True,
                             extra=['-replication-feed', *common])
            primary.start()
            note(primary, 'restarted on the other build')
            step(primary, 'SET', 'k', 'x')
            step(primary, 'KEEL.PROMOTE', 5)
            step(primary, 'KEEL.PROMOTE', 6)
            step(primary, 'SET', 'k', 'y')
            note(primary, 'promoted on the other build')
            primary.stop()
            primary = Server(binaries[first], case / 'primary', async_append=True,
                             extra=['-replication-feed', *common])
            primary.start()
            note(primary, 'back on the first build')
            step(primary, 'GET', 'k')
        finally:
            if replica is not None:
                replica.stop(check=False)
            primary.stop(check=False)
        seen[first] = trace
    if seen['baseline'] != seen['candidate']:
        for x, y in zip(seen['baseline'], seen['candidate']):
            if x != y:
                raise AssertionError(f'terms differ: baseline {x} candidate {y}')
        raise AssertionError('terms differ in length')
    return {'name': 'terms', 'passed': True, 'trace': seen['baseline']}


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--baseline', required=True, help='the build before the change')
    parser.add_argument('--candidate', required=True, help='the build with the change')
    parser.add_argument('--out', required=True, help='a fresh directory for logs and the report')
    parser.add_argument('--protocols', default='1,2')
    parser.add_argument('--parts', default='wire,pairs,terms', help='which checks to run')
    args = parser.parse_args()
    root = Path(args.out).resolve()
    root.mkdir(parents=True, exist_ok=False)
    binaries = {side: str(Path(getattr(args, side)).resolve()) for side in SIDES}
    report = {
        'platform': platform.platform(), 'harness_sha256': sha256(__file__),
        **{side: {'sha256': sha256(binaries[side]),
                  'version': subprocess.check_output([binaries[side], '-version'], text=True).strip()}
           for side in SIDES},
        'cases': [], 'passed': False,
    }
    parts = args.parts.split(',')
    try:
        for protocol in [int(p) for p in args.protocols.split(',')]:
            if 'wire' in parts:
                served = {}
                for side in SIDES:
                    served[side] = wire(root / f'wire-protocol{protocol}-{side}', binaries[side], protocol)
                compare_wire(served['baseline'], served['candidate'], f'protocol {protocol}')
                result = {'name': f'wire-protocol{protocol}', 'passed': True,
                          'frames': len(served['baseline']['frames']),
                          'bodies_normalized_bytes': sum(len(b) for _, b in served['baseline']['bodies'])}
                report['cases'].append(result)
                print(result['name'], 'passed:', result['frames'], 'frames,',
                      result['bodies_normalized_bytes'], 'body bytes identical', flush=True)
            if 'pairs' in parts:
                results, note = pairs(root, binaries, protocol)
                report['cases'].extend(results)
                report['cases'].append({'name': f'pairs-protocol{protocol}', 'passed': True, 'checks': [note]})
                print(note, flush=True)
        if 'terms' in parts:
            report['cases'].append(terms(root, binaries))
            print('terms passed', flush=True)
        report['passed'] = True
    except BaseException as exc:
        report['failure'] = repr(exc)
        raise
    finally:
        (root / 'report.json').write_text(json.dumps(report, indent=2, default=repr) + '\n')


if __name__ == '__main__':
    main()
