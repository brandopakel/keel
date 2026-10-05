#!/usr/bin/env python3
"""Check that two builds write the same log and replay each other's.

Step 2.3 of the embedding plan (docs/embedding-plan.md) moves the log, its
append worker and the rewrite into the engine without changing a byte of what
they write. This runs the same sequential workload on a baseline build and a
candidate build, under every fsync policy and append mode, and checks:

- the log each writes is the same bytes, and so is the log each rewrites it
  into, once the two normalizations of internal/core/persistence_golden_test.go
  are applied: a relative expiry, written as the instant it falls due, becomes
  the whole hours from the run's start, and the pairs of an HSET record, and of
  a ZADD record without options, are put in field or member order;
- upgrade: the candidate replays every log the baseline wrote, rewrote, and
  wrote while a rewrite ran and then crashed, to the baseline's keyspace;
- rollback: the baseline replays every log the candidate wrote the same way,
  to the candidate's keyspace;
- a log of BF and CF forms the build before RedisBloom parity wrote replays on
  both to the same filters, and both rewrite it to the same bytes.

A failure leaves every log and server log under --out, with the report.
"""
import argparse
import hashlib
import json
import platform
import shutil
import subprocess
import time
from pathlib import Path

from validation_lib import Client, Server, info, rewrite, sha256

HOUR_MS = 3600 * 1000
DAY_MS = 24 * HOUR_MS
ABSOLUTE = 4102444800  # 2100-01-01T00:00:00Z
POLICIES = ['always', 'everysec', 'no']
MODES = {'sync': (False, []), 'worker': (True, []),
         'concurrent': (True, ['-aof-concurrent-append'])}


class Error:
    """An error reply, kept rather than raised, so it can be compared."""
    def __init__(self, text):
        self.text = text

    def __eq__(self, other):
        return isinstance(other, Error) and other.text == self.text

    def __repr__(self):
        return f'Error({self.text!r})'


def call(client, *parts):
    """One command; an error reply, top level or inside EXEC, is returned."""
    parts = [p if isinstance(p, bytes) else str(p).encode() for p in parts]
    client.socket.sendall(b'*%d\r\n' % len(parts) + b''.join(b'$%d\r\n' % len(p) + p + b'\r\n' for p in parts))
    return read(client)


def read(client):
    line = client.stream.readline(65537)
    if not line.endswith(b'\r\n'):
        raise ValueError('invalid RESP header')
    kind, body = line[:1], line[1:-2]
    if kind == b'-':
        return Error(body.decode(errors='replace'))
    if kind == b'+':
        return body
    n = int(body)
    if kind == b':':
        return n
    if n == -1:
        return None
    if kind == b'$':
        value = client.stream.read(n + 2)
        if len(value) != n + 2:
            raise ValueError('short bulk reply')
        return value[:-2]
    if kind == b'*':
        return [read(client) for _ in range(n)]
    raise ValueError(f'unknown RESP kind {kind!r}')


def hours(n, unit_ms=1000):
    return n * HOUR_MS // unit_ms


def workload(c):
    """Every family, the records logged in a command's place, expiries of each
    kind, SET over other types and transactions; deterministic and sequential,
    so both builds receive exactly the same commands in the same order."""
    replies = []
    def do(*parts):
        replies.append(call(c, *parts))
    do('SET', 'f:1', 'v')
    do('MULTI'); do('SET', 'f:2', 'v'); do('FLUSHDB'); do('SET', 'f:3', 'v'); do('EXEC')
    do('SET', 's:plain', 'v1'); do('SET', 's:plain', 'v2', 'XX'); do('SET', 's:plain', 'v3', 'GET')
    do('SETNX', 's:nx', 'a'); do('SETNX', 's:nx', 'b'); do('SET', 's:nx2', 'a', 'NX')
    do('MSET', 'm:1', 'a', 'm:2', 'b')
    do('INCR', 'c'); do('INCRBY', 'c', 41); do('DECR', 'c'); do('DECRBY', 'c', 2); do('INCR', 's:plain')
    do('SET', 's:ex', 'v', 'EX', hours(1)); do('SET', 's:px', 'v', 'PX', hours(2, 1))
    do('SET', 's:exat', 'v', 'EXAT', ABSOLUTE); do('SET', 's:pxat', 'v', 'PXAT', f'{ABSOLUTE}123')
    do('SETEX', 's:setex', hours(3), 'v'); do('PSETEX', 's:psetex', hours(4, 1), 'v')
    do('SET', 's:ex', 'kept', 'KEEPTTL')
    do('SET', 's:large', b'0123456789abcdef' * (1 << 17))  # 2 MiB
    do('EXPIRE', 's:plain', hours(5)); do('PEXPIRE', 'm:1', hours(6, 1))
    do('EXPIREAT', 'm:2', ABSOLUTE); do('PEXPIREAT', 'c', f'{ABSOLUTE}999'); do('PERSIST', 'm:2')
    do('PEXPIREAT', 's:nx', 1000); do('DEL', 's:setex', 'missing'); do('UNLINK', 's:psetex')
    do('SET', 's:brief', 'v', 'PX', 1)
    time.sleep(.01)
    do('GET', 's:brief')
    do('HSET', 'h', 'f1', 'v1', 'f2', 'v2', 'f3', 'v3'); do('HSETNX', 'h', 'f4', 'v4')
    do('HDEL', 'h', 'f2'); do('HINCRBY', 'h', 'n', 5)
    do('RPUSH', 'l', 'a', 'b', 'c', 'd'); do('LPUSH', 'l', 'z'); do('LPOP', 'l'); do('RPOP', 'l')
    do('LPOP', 'l', 1); do('LSET', 'l', 0, 'q'); do('LTRIM', 'l', 0, 1)
    do('SADD', 'set', 'a', 'b', 'c', 'd'); do('SREM', 'set', 'd'); do('SADD', 'set:one', 'only'); do('SPOP', 'set:one')
    do('ZADD', 'z', 1, 'a', 2, 'b', 3, 'c'); do('ZADD', 'z', 'XX', 'CH', 10, 'b'); do('ZINCRBY', 'z', 2.5, 'a')
    do('ZREM', 'z', 'c'); do('ZADD', 'z2', 1, 'x', 2, 'y'); do('ZPOPMIN', 'z2')
    do('GEOADD', 'geo', '13.361389', '38.115556', 'palermo', '15.087269', '37.502669', 'catania')
    do('BF.RESERVE', 'bf', '0.01', 1000); do('BF.MADD', 'bf', 'a', 'b'); do('BF.ADD', 'bf:auto', 'x')
    do('CF.RESERVE', 'cf', 1000); do('CF.ADD', 'cf', 'a'); do('CF.ADDNX', 'cf', 'b'); do('CF.DEL', 'cf', 'a')
    do('CMS.INITBYDIM', 'cms', 100, 5); do('CMS.INCRBY', 'cms', 'a', 3)
    do('MORRIS.INITBYDIM', 'mor', 200, 5); do('MORRIS.INCRBY', 'mor', 'hits', 500)
    do('PFADD', 'hll', 'a', 'b', 'c'); do('PFADD', 'hll2', 'c', 'd'); do('PFMERGE', 'hll3', 'hll', 'hll2')
    image = call(c, 'KEEL.DUMP', 'l')
    do('KEEL.RESTORE', 'l:copy', image)
    do('MEMKV.RESTORE', 'bf:copy', call(c, 'KEEL.DUMP', 'bf'))
    do('HSET', 'x:hash', 'f', 'v'); do('SET', 'x:hash', 'string now')
    do('ZADD', 'x:z', 1, 'm'); do('SET', 'x:z', 'v', 'XX')
    do('MULTI'); do('SET', 't:1', 'a'); do('INCR', 't:2'); do('HSET', 't:3', 'f', 'v'); do('EXEC')
    do('MULTI'); do('GET', 't:1'); do('EXEC')
    do('MULTI'); do('SET', 't:4', 'v'); do('DISCARD')
    do('SADD', 't:pop', 'only')
    do('MULTI'); do('EXPIRE', 't:1', hours(8)); do('SET', 't:5', 'v', 'EX', hours(9)); do('SPOP', 't:pop'); do('EXEC')
    # What a rewrite streams over several records.
    do('RPUSH', 'big:list', *[chr(97 + i % 26).encode() * 4096 + str(i).encode() for i in range(40)])
    do('SADD', 'big:set', *[f'm:{i}' for i in range(300)])
    do('ZADD', 'big:z', *[x for i in range(300) for x in (i * 3, f'm:{i}')])
    do('BF.RESERVE', 'big:bf', '0.001', 100000); do('BF.MADD', 'big:bf', 'a', 'b')
    do('PEXPIRE', 'big:bf', hours(3, 1))
    do('CMS.INITBYDIM', 'big:cms', 10000, 4); do('CMS.INCRBY', 'big:cms', 'a', 3)
    return replies


def more_writes(c, round_):
    """Writes sent while a rewrite runs and after it: their order is fixed, so
    the keyspace they leave is too, though where the rewrite saw them is not."""
    for i in range(200):
        call(c, 'SET', f'during:{round_}:{i % 50}', i)
        call(c, 'HSET', f'during:h:{round_}', f'f{i % 7}', i)
        call(c, 'RPUSH', 'big:list', f'during:{round_}:{i}')
        call(c, 'INCR', f'during:count:{round_}')


def expiry(c, key, start, now):
    ttl = call(c, 'PTTL', key)
    if ttl == -1:
        return None
    at = now + ttl
    if 0 <= at - start <= DAY_MS:
        return f'in {round((at - start) / HOUR_MS)}h'
    return f'at {round(at / 1000)}s'


def snapshot(c, start):
    """The keyspace, key by key: type, expiry and value, in key order."""
    now = int(time.time() * 1000)
    state = {}
    for key in sorted(call(c, 'KEYS', '*')):
        kind = call(c, 'TYPE', key).decode()
        if kind == 'string':
            value = call(c, 'GET', key)
        elif kind == 'hash':
            pairs = call(c, 'HGETALL', key)
            value = sorted(zip(pairs[::2], pairs[1::2]))
        elif kind == 'list':
            value = call(c, 'LRANGE', key, 0, -1)
        elif kind == 'set':
            value = sorted(call(c, 'SMEMBERS', key))
        elif kind == 'zset':
            value = call(c, 'ZRANGE', key, 0, -1, 'WITHSCORES')
        else:
            value = call(c, 'KEEL.DUMP', key)
        state[key.decode(errors='replace')] = [kind, expiry(c, key, start, now), sha256_of(value)]
    return state


def sha256_of(value):
    return hashlib.sha256(repr(value).encode()).hexdigest()


def records(body):
    """A log's records, each as the list of its parts."""
    out, i = [], 0
    while i < len(body):
        if body[i:i + 1] != b'*':
            raise ValueError(f'record header at byte {i}')
        j = body.index(b'\r\n', i)
        count, i = int(body[i + 1:j]), j + 2
        parts = []
        for _ in range(count):
            if body[i:i + 1] != b'$':
                raise ValueError(f'bulk header at byte {i}')
            k = body.index(b'\r\n', i)
            n = int(body[i + 1:k])
            parts.append(body[k + 2:k + 2 + n])
            i = k + 4 + n
        out.append(parts)
    return out


def is_score(value):
    try:
        float(value)
        return True
    except ValueError:
        return False


def normalize(body, start):
    """The two normalizations persistence_golden_test.go applies, and only those."""
    out = []
    for parts in records(body):
        name = parts[0].upper()
        if name == b'PEXPIREAT' and len(parts) == 3:
            at = int(parts[2])
            if 0 <= at - start <= DAY_MS:
                parts[2] = b'run+%dh' % ((at - start) // HOUR_MS)
        elif name == b'HSET' and len(parts) % 2 == 0:
            pairs = sorted(zip(parts[2::2], parts[3::2]), key=lambda p: p[0])
            parts[2:] = [x for p in pairs for x in p]
        elif name == b'ZADD' and len(parts) % 2 == 0 and all(is_score(s) for s in parts[2::2]):
            pairs = sorted(zip(parts[2::2], parts[3::2]), key=lambda p: p[1])
            parts[2:] = [x for p in pairs for x in p]
        out.append(b'*%d\r\n' % len(parts) + b''.join(b'$%d\r\n%s\r\n' % (len(p), p) for p in parts))
    return b''.join(out)


def first_difference(a, b):
    ra, rb = records(a), records(b)
    for i, (x, y) in enumerate(zip(ra, rb)):
        if x != y:
            return f'record {i}: baseline {short(x)} candidate {short(y)}'
    return f'{len(ra)} records against {len(rb)}'


def short(parts):
    return [p[:40] + (b'...' if len(p) > 40 else b'') for p in parts]


def legacy_log():
    """BF and CF forms the build before RedisBloom parity accepted and wrote;
    the same commands as legacyBloomLog in redisbloom_persistence_test.go."""
    commands = [
        ['CF.RESERVE', 'cf:1', '1'], ['CF.RESERVE', 'cf:7', '007'], ['CF.ADD', 'cf:1', 'a'],
        ['CF.DEL', 'missing', 'x'], ['CF.DEL', 'cf:1', 'never'],
        ['BF.RESERVE', 'bf:007', '0.01', '007'], ['BF.RESERVE', 'bf:plus', '+0.01', '100'],
        ['BF.RESERVE', 'bf:hex', '0x1p-7', '100'], ['BF.RESERVE', 'bf:wide', '0.01', '10', 'EXPANSION', '100000'],
        ['BF.MADD', 'bf:wide', *'abcdefghijkl'], ['BF.RESERVE', 'nonscaling', '0.01', '2'],
        ['BF.MADD', 'nonscaling', 'a', 'b', 'c', 'd'], ['CF.RESERVE', 'expansion', '1000'],
        ['CF.ADD', 'expansion', 'a'],
    ]
    return b''.join(b'*%d\r\n' % len(c) + b''.join(b'$%d\r\n%s\r\n' % (len(p), p.encode()) for p in c) for c in commands)


class Case:
    def __init__(self, root, policy, mode, binaries):
        self.policy, self.mode, self.binaries = policy, mode, binaries
        self.root = root / f'{policy}-{mode}'
        self.root.mkdir()
        self.worker, self.extra = MODES[mode]
        self.starts = 0

    def server(self, side, data):
        self.starts += 1
        return Server(self.binaries[side], data, policy=self.policy, async_append=self.worker, extra=self.extra)

    def keep(self, data, name):
        shutil.copy2(data / 'store.aof', self.root / name)
        return (self.root / name).read_bytes()

    def write(self, side):
        """The workload, a restart, a rewrite, writes during a second rewrite,
        and a crash; the log after each step, and the keyspace it holds."""
        data = self.root / side / 'data'
        out = {}
        with self.server(side, data) as s:
            start = int(time.time() * 1000)
            out['replies'] = sha256_of(workload(s.client))
            out['written_state'] = snapshot(s.client, start)
        out['written'] = self.keep(data, f'{side}-written.aof')
        with self.server(side, data) as s:
            if snapshot(s.client, start) != out['written_state']:
                raise AssertionError(f'{side}: restart changed the keyspace')
            rewrite(s.client)
            out['rewritten_state'] = snapshot(s.client, start)
        out['rewritten'] = self.keep(data, f'{side}-rewritten.aof')
        s = self.server(side, data).start()
        try:
            before = int(info(s.client, 'persistence')['aof_rewrites'])
            for attempt in range(100):
                # A worker append still in flight is a retry, as BGREWRITEAOF answers.
                if not isinstance(call(s.client, 'BGREWRITEAOF'), Error):
                    break
                time.sleep(.01)
            more_writes(s.client, 1)
            deadline = time.monotonic() + 30
            while int(info(s.client, 'persistence')['aof_rewrites']) == before:
                if time.monotonic() > deadline:
                    raise TimeoutError(f'{side}: the rewrite did not finish')
                time.sleep(.01)
            if info(s.client, 'persistence').get('aof_last_bgrewrite_status', 'ok') != 'ok':
                raise AssertionError(f'{side}: the rewrite failed')
            more_writes(s.client, 2)
            out['crashed_state'] = snapshot(s.client, start)
        finally:
            s.stop(crash=True)  # acknowledged writes must survive kill -9
        out['crashed'] = self.keep(data, f'{side}-crashed.aof')
        out['start'] = start
        return out

    def replay(self, side, log, name, start, want):
        data = self.root / f'{side}-replays-{name}'
        data.mkdir()
        (data / 'store.aof').write_bytes(log)
        with self.server(side, data) as s:
            got = snapshot(s.client, start)
        if got != want:
            diff = sorted(k for k in set(got) | set(want) if got.get(k) != want.get(k))
            raise AssertionError(f'{side} replaying {name}: keys differ: {diff[:10]}')

    def run(self):
        base, cand = self.write('baseline'), self.write('candidate')
        checks = []
        if base['replies'] != cand['replies']:
            raise AssertionError('the workload got different replies')
        for step in ['written', 'rewritten']:
            a, b = normalize(base[step], base['start']), normalize(cand[step], cand['start'])
            if a != b:
                raise AssertionError(f'{step} logs differ: {first_difference(a, b)}')
            checks.append(f'{step} logs identical ({len(a)} bytes normalized)')
        for step in ['written', 'rewritten', 'crashed']:
            if base[f'{step}_state'] != cand[f'{step}_state']:
                raise AssertionError(f'{step} keyspaces differ')
            self.replay('candidate', base[step], f'baseline-{step}', base['start'], base[f'{step}_state'])
            self.replay('baseline', cand[step], f'candidate-{step}', cand['start'], cand[f'{step}_state'])
            checks.append(f'{step}: each build replays the other\'s log to its keyspace')
        return {'name': self.root.name, 'passed': True, 'server_starts': self.starts, 'checks': checks,
                'logs': {f'{side}-{step}': sha256(self.root / f'{side}-{step}.aof')
                         for side in ['baseline', 'candidate'] for step in ['written', 'rewritten', 'crashed']}}


def legacy(root, binaries):
    """The legacy filter log replays on both builds to the same filters, and both
    rewrite it to the same bytes, which each replays."""
    case = root / 'legacy-filters'
    case.mkdir()
    states, logs = {}, {}
    for side in ['baseline', 'candidate']:
        data = case / side
        data.mkdir()
        (data / 'store.aof').write_bytes(legacy_log())
        with Server(binaries[side], data) as s:
            states[side] = snapshot(s.client, 0)
            rewrite(s.client)
            if snapshot(s.client, 0) != states[side]:
                raise AssertionError(f'{side}: the rewrite changed the legacy filters')
        logs[side] = (data / 'store.aof').read_bytes()
    if states['baseline'] != states['candidate']:
        raise AssertionError('the legacy filters replay differently')
    if normalize(logs['baseline'], 0) != normalize(logs['candidate'], 0):
        raise AssertionError(f"legacy rewrites differ: {first_difference(logs['baseline'], logs['candidate'])}")
    for side, other in [('baseline', 'candidate'), ('candidate', 'baseline')]:
        data = case / f'{side}-replays-{other}'
        data.mkdir()
        (data / 'store.aof').write_bytes(logs[other])
        with Server(binaries[side], data) as s:
            if snapshot(s.client, 0) != states[other]:
                raise AssertionError(f"{side} replays the {other}'s rewritten legacy log differently")
    return {'name': 'legacy-filters', 'passed': True, 'rewritten_sha256': sha256(case / 'baseline' / 'store.aof')}


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--baseline', required=True, help='the build before the change')
    parser.add_argument('--candidate', required=True, help='the build with the change')
    parser.add_argument('--out', required=True, help='a fresh directory for logs and the report')
    parser.add_argument('--policies', default=','.join(POLICIES))
    parser.add_argument('--modes', default=','.join(MODES))
    args = parser.parse_args()
    root = Path(args.out).resolve()
    root.mkdir(parents=True, exist_ok=False)
    binaries = {'baseline': str(Path(args.baseline).resolve()), 'candidate': str(Path(args.candidate).resolve())}
    report = {
        'platform': platform.platform(),
        'baseline': {'sha256': sha256(binaries['baseline']),
                     'version': subprocess.check_output([binaries['baseline'], '-version'], text=True).strip()},
        'candidate': {'sha256': sha256(binaries['candidate']),
                      'version': subprocess.check_output([binaries['candidate'], '-version'], text=True).strip()},
        'cases': [], 'passed': False,
    }
    try:
        report['cases'].append(legacy(root, binaries))
        print('legacy-filters passed', flush=True)
        for policy in args.policies.split(','):
            for mode in args.modes.split(','):
                result = Case(root, policy, mode, binaries).run()
                report['cases'].append(result)
                print(result['name'], 'passed:', '; '.join(result['checks']), flush=True)
        report['passed'] = True
    except BaseException as exc:
        report['failure'] = repr(exc)
        raise
    finally:
        (root / 'report.json').write_text(json.dumps(report, indent=2) + '\n')


if __name__ == '__main__':
    main()
