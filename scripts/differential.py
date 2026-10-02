#!/usr/bin/env python3
"""Replay deterministic mixed RESP2 operations against Keel and Redis.

Compare replies and final state for the supported common command contract,
including transactions: MULTI blocks of the same operations, with queueing
refusals, nested MULTI and DISCARD mixed in.
Unordered sets/hash fields are normalized; error text is compared by RESP error
class. Deliberate differences (Redis dumps/modules) are outside this test, not
silently accepted mismatches. String writes also land on names other types
hold, because SET, MSET, SETEX, PSETEX and SETNX replace or find those as
Redis's do.
"""
import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import random
import socket
import subprocess
import time

from validation_lib import Client, Server, rewrite, sha256


def normalize(command, value):
    if command[0] in ('SMEMBERS', 'HKEYS', 'HVALS', 'KEYS'):
        value = sorted(value)
    if command[0] == 'HGETALL':
        assert len(value) % 2 == 0, 'truncated HGETALL response'
        value = sorted(zip(value[::2], value[1::2]))
    return value


def error_class(message):
    return message.removeprefix('(error) ').split()[0]


def execute(client, command):
    try:
        return ('ok', normalize(command, client.call(*command)))
    except RuntimeError as exc:
        return ('error', error_class(str(exc)))


def read_tolerant(client, depth=0):
    """Read one reply, keeping an error inside an array as a value.

    The shared client raises on any error, which would abandon the rest of an
    EXEC array on the wire; a transaction's reply holds an error per command
    that failed, so each one is kept and compared by class instead.
    """
    if depth > 16:
        raise ValueError('RESP nesting limit')
    line = client.stream.readline(65537)
    if not line.endswith(b'\r\n'):
        raise ValueError('invalid RESP header')
    kind, body = line[:1], line[1:-2]
    if kind == b'-':
        return ('error', error_class(body.decode(errors='replace')))
    if kind == b'+':
        return body
    if kind == b':':
        return int(body)
    n = int(body)
    if n == -1 and kind in (b'$', b'*'):
        return None
    if kind == b'$':
        value = client.stream.read(n+2)
        if len(value) != n+2 or value[-2:] != b'\r\n':
            raise ValueError('invalid bulk reply')
        return value[:-2]
    if kind == b'*':
        return [read_tolerant(client, depth+1) for _ in range(n)]
    raise ValueError('unknown RESP kind')


def transaction(rng):
    """A MULTI block of ordinary operations, and the ways one goes wrong.

    Most blocks commit, some with runtime errors the operations produce on
    their own; some carry a command Redis refuses while queueing (unknown, or
    the wrong number of arguments), which aborts the block at EXEC; some nest a
    MULTI, which is refused without aborting; and some end in DISCARD.
    """
    commands = [operation(rng) for _ in range(rng.randrange(6))]
    shape = rng.random()
    refusal = None
    if shape < .08:
        refusal = ['NOSUCHCOMMAND', 'x']
    elif shape < .16:
        refusal = rng.choice([['GET'], ['HSET', 'hash:0'], ['SETEX', 'string:0', 10], ['LRANGE', 'list:0', 0]])
    elif shape < .22:
        refusal = ['MULTI']
    if refusal:
        commands.insert(rng.randrange(len(commands)+1), refusal)
    return commands, ['DISCARD'] if shape > .92 else ['EXEC']


def execute_transaction(client, commands, ending):
    replies = []
    for command in (['MULTI'], *commands, ending):
        parts = [p if isinstance(p, bytes) else str(p).encode() for p in command]
        client.socket.sendall(b'*%d\r\n' % len(parts)+b''.join(b'$%d\r\n' % len(p)+p+b'\r\n' for p in parts))
        replies.append(read_tolerant(client))
    result = replies[-1]
    if ending == ['EXEC'] and isinstance(result, list):
        queued = [command for command, reply in zip(commands, replies[1:-1]) if reply == b'QUEUED']
        assert len(queued) == len(result), ('EXEC reply length', commands, replies)
        replies[-1] = [value if isinstance(value, tuple) else normalize(command, value)
                       for command, value in zip(queued, result)]
    return replies


def snapshot(client):
    result = []
    for key in sorted(client.call('KEYS', '*')):
        kind = client.call('TYPE', key)
        command = {b'string': ['GET', key], b'hash': ['HGETALL', key],
                   b'list': ['LRANGE', key, 0, -1], b'set': ['SMEMBERS', key],
                   b'zset': ['ZRANGE', key, 0, -1, 'WITHSCORES']}[kind]
        result.append((key, kind, execute(client, command)))
    return result


def operation(rng):
    kind = rng.choice(['string', 'hash', 'list', 'set', 'zset', 'key'])
    key = kind + ':' + str(rng.randrange(32))
    value = rng.choice([b'', b'\x00\xff\r\n', b'x'*64, b'x'*1024,
                        str(rng.randrange(-100, 100)).encode(), b'007', b'+1',
                        b'-0', b'9223372036854775807', b'-9223372036854775808'])
    member, field = 'member:'+str(rng.randrange(16)), 'field:'+str(rng.randrange(8))
    if kind == 'string' and rng.random() < 0.15:
        other = rng.choice(['hash', 'list', 'set', 'zset']) + ':' + str(rng.randrange(32))
        return rng.choice([
            ['SET', other, value], ['SET', other, value, 'NX'], ['SET', other, value, 'XX'],
            ['SET', other, value, 'GET'], ['SET', other, value, 'KEEPTTL'],
            ['MSET', key, value, other, value], ['SETEX', other, 3600, value],
            ['PSETEX', other, 3600000, value], ['SETNX', other, value],
        ])
    if kind == 'string':
        return rng.choice([
            ['SET', key, value], ['SET', key, value, 'NX'], ['SET', key, value, 'XX', 'GET'],
            ['SET', key, value, 'GET', 'KEEPTTL'], ['GET', key], ['INCRBY', key, rng.randrange(-4,5)],
            ['DECRBY', key, rng.randrange(-4,5)], ['MGET', key, 'missing', 'hash:0'],
            ['MSET', key, value, 'string:other', value], ['INCR', key],
            ['SETEX', key, 3600, value], ['PSETEX', key, 3600000, value],
        ])
    if kind == 'hash':
        return rng.choice([
            ['HSET', key, field, value], ['HSETNX', key, field, value], ['HGET', key, field],
            ['HMGET', key, field, 'missing'], ['HDEL', key, field], ['HEXISTS', key, field],
            ['HLEN', key], ['HGETALL', key], ['HINCRBY', key, field, rng.randrange(-4,5)],
        ])
    if kind == 'list':
        return rng.choice([
            ['LPUSH', key, value, b'b'], ['RPUSH', key, value, b'b'], ['LPOP', key], ['RPOP', key],
            ['LRANGE', key, -10, 10], ['LTRIM', key, -32, -1], ['LLEN', key],
            ['LINDEX', key, -1], ['LSET', key, 0, value],
        ])
    if kind == 'set':
        return rng.choice([
            ['SADD', key, member, 'common'], ['SREM', key, member], ['SISMEMBER', key, member],
            ['SMISMEMBER', key, member, 'missing'], ['SCARD', key], ['SMEMBERS', key],
        ])
    if kind == 'zset':
        score = rng.randrange(-20, 21)
        return rng.choice([
            ['ZADD', key, score, member], ['ZADD', key, 'NX', score, member],
            ['ZADD', key, 'XX', 'CH', score, member], ['ZREM', key, member],
            ['ZSCORE', key, member], ['ZRANK', key, member], ['ZCARD', key],
            ['ZRANGE', key, -10, 10, 'WITHSCORES'], ['ZRANGE', key, 0, 10, 'REV'],
            ['ZCOUNT', key, '(0', '+inf'], ['ZRANGEBYSCORE', key, -10, 10, 'WITHSCORES', 'LIMIT', 1, 5],
            ['ZREVRANGEBYSCORE', key, '+inf', '-inf', 'LIMIT', 0, 3],
            ['ZINCRBY', key, rng.randrange(-3,4), member], ['ZPOPMIN', key, rng.randrange(4)],
            ['ZPOPMAX', key, rng.randrange(4)],
        ])
    key = rng.choice(['string','hash','list','set','zset']) + ':' + str(rng.randrange(32))
    return rng.choice([['DEL', key], ['EXISTS', key, 'missing'], ['TYPE', key],
                       ['PERSIST', key], ['PEXPIREAT', key, 4102444800000]])


def scan_all(client, *options):
    cursor, keys = b'0', set()
    for _ in range(1000000):
        cursor, batch = client.call('SCAN', cursor, *options)
        assert isinstance(cursor, bytes) and isinstance(batch, list)
        keys.update(batch)
        if cursor == b'0':
            return sorted(keys)
    raise AssertionError('SCAN did not terminate')


def check_scan(candidate, reference):
    options = [(), ('MATCH', ''), ('TYPE', ''), ('TYPE', 'STRING'),
               ('TYPE', 'SeT'), ('MATCH', 'string:*', 'COUNT', '1'),
               ('MATCH', '*[0-9]', 'COUNT', '10'), ('TYPE', 'unknown')]
    for option in options:
        assert scan_all(candidate,*option) == scan_all(reference,*option), ('SCAN', option)
    return len(options)


def check_geo(candidate, reference, *, populate=False):
    """Compare bounded selection, option encoding, and huge-radius membership."""
    if populate:
        rng = random.Random(741)
        for index in range(1000):
            span = (10, 10) if index < 700 else (360, 170)
            lon, lat = [(rng.random()-.5)*width for width in span]
            command = ['GEOADD', 'geo:admission', lon, lat, f'member:{index}']
            assert candidate.call(*command) == reference.call(*command)
    checks = 0
    for radius in (50, 1000, 40000):
        for order in ('', 'ASC', 'DESC'):
            for count in (0, 1, 7, 100, 10000000):
                for flags in range(8):
                    command = ['GEOSEARCH', 'geo:admission', 'FROMLONLAT', 0, 0,
                               'BYRADIUS', radius, 'km']
                    if order:
                        command.append(order)
                    if count:
                        command += ['COUNT', count]
                    for bit, option in enumerate(('WITHDIST', 'WITHHASH', 'WITHCOORD')):
                        if flags & (1 << bit):
                            command.append(option)
                    actual, expected = candidate.call(*command), reference.call(*command)
                    if not order and not count:
                        actual, expected = sorted(actual), sorted(expected)
                    assert len(actual) == len(expected), (command, len(actual), len(expected))
                    for got, want in zip(actual, expected, strict=True):
                        if not flags:
                            assert got == want, (command, got, want)
                            continue
                        assert isinstance(got, list) and len(got) == len(want) == 1 + flags.bit_count(), (command, got, want)
                        assert got[0] == want[0], (command, got, want)
                        offset = 1
                        if flags & 1:
                            assert abs(float(got[offset])-float(want[offset])) < .00011, (command, got, want)
                            offset += 1
                        if flags & 2:
                            assert got[offset] == want[offset], (command, got, want)
                            offset += 1
                        if flags & 4:
                            assert isinstance(got[offset], list) and len(got[offset]) == len(want[offset]) == 2, (command, got, want)
                            assert all(math.isclose(float(a), float(b), rel_tol=0, abs_tol=1e-12)
                                       for a,b in zip(got[offset], want[offset], strict=True)), (command, got, want)
                    checks += 1
        # ANY may select a different subset on each implementation. Validate
        # each against the full reference membership and exact option shape.
        for flags in range(8):
            base = ['GEOSEARCH', 'geo:admission', 'FROMLONLAT', 0, 0,
                    'BYRADIUS', radius, 'km']
            for bit, option in enumerate(('WITHDIST', 'WITHHASH', 'WITHCOORD')):
                if flags & (1 << bit):
                    base.append(option)
            all_rows = reference.call(*base)
            expected = {(row[0] if flags else row): row for row in all_rows}
            for count in (1, 7, 10000000):
                command = base + ['COUNT', count, 'ANY']
                for client in (candidate, reference):
                    actual = client.call(*command)
                    assert isinstance(actual, list) and len(actual) == min(count, len(expected)), (command, actual)
                    members = [row[0] if flags else row for row in actual]
                    assert len(members) == len(set(members)), (command, members)
                    for got, member in zip(actual, members, strict=True):
                        assert member in expected, (command, got)
                        if not flags:
                            continue
                        want = expected[member]
                        assert isinstance(got, list) and len(got) == len(want) == 1 + flags.bit_count(), (command, got, want)
                        offset = 1
                        if flags & 1:
                            assert abs(float(got[offset])-float(want[offset])) < .00011, (command, got, want)
                            offset += 1
                        if flags & 2:
                            assert got[offset] == want[offset], (command, got, want)
                            offset += 1
                        if flags & 4:
                            assert isinstance(got[offset], list) and len(got[offset]) == len(want[offset]) == 2, (command, got, want)
                            assert all(math.isclose(float(a), float(b), rel_tol=0, abs_tol=1e-12)
                                       for a,b in zip(got[offset], want[offset], strict=True)), (command, got, want)
                checks += 1
    return checks


def run(args):
    os.umask(0o077)
    root = args.out.resolve()
    root.mkdir(parents=True, exist_ok=False)
    report = {'status': 'running', 'seed': args.seed, 'steps_requested': args.steps,
              'binary_sha256': sha256(args.bin), 'redis_sha256': sha256(args.redis),
              'harness_sha256': sha256(__file__), 'policy': args.policy, 'concurrent': args.concurrent,
              'reply_checks': 0, 'transaction_checks': 0, 'state_checks': 0, 'restarts': 0,
              'limits': 'Supported common RESP2 commands, alone and in MULTI/EXEC/DISCARD blocks; unordered collections normalized; errors compared by class, inside EXEC replies too. No timing-based TTL differential or WATCH claim.'}
    server = Server(args.bin, root/'keel', policy=args.policy, async_append=args.concurrent,
                    extra=['-aof-concurrent-append'] if args.concurrent else ())
    server.env = {key: value for key,value in server.env.items() if key in ('PATH','HOME','TMPDIR','KEEL_VALIDATION_PASSWORD')}
    redis = reference = None
    log = (root/'redis.log').open('w')
    trace = (root/'commands.jsonl').open('w')
    try:
        server.start()
        with socket.socket() as reservation:
            reservation.bind(('127.0.0.1',0))
            port = reservation.getsockname()[1]
        # Debian's redis-server may be a symlink to a multicall executable;
        # preserve argv[0], which selects server rather than RDB-check mode.
        redis = subprocess.Popen([str(args.redis.absolute()), '-'], stdin=subprocess.PIPE, stdout=log, stderr=log,
                                 env={key:value for key,value in server.env.items() if key != 'KEEL_VALIDATION_PASSWORD'})
        redis.stdin.write((f'bind 127.0.0.1\nport {port}\nsave ""\nappendonly no\n'
                           f'requirepass {server.password}\n').encode())
        redis.stdin.close()
        deadline = time.monotonic()+10
        while True:
            if redis.poll() is not None:
                raise RuntimeError('Redis startup failed')
            try:
                reference = Client('127.0.0.1',port,server.password)
                assert reference.call('PING') == b'PONG'
                break
            except OSError:
                if time.monotonic() > deadline:
                    raise
                time.sleep(.02)
        for client in (server.client,reference):
            assert client.call('SET', b'', b'empty-key') == b'OK'
        report['scan_checks'] = check_scan(server.client, reference)
        rng = random.Random(args.seed)
        encode = lambda command: [{'hex':part.hex()} if isinstance(part,bytes) else part for part in command]
        for index in range(args.steps):
            if rng.random() < .1:
                commands, ending = transaction(rng)
                trace.write(json.dumps({'transaction': [encode(c) for c in commands], 'end': ending})+'\n')
                actual = execute_transaction(server.client, commands, ending)
                expected = execute_transaction(reference, commands, ending)
                assert actual == expected, (index, commands, ending, actual, expected)
                report['transaction_checks'] += 1
            else:
                command = operation(rng)
                trace.write(json.dumps(encode(command))+'\n')
                actual, expected = execute(server.client, command), execute(reference, command)
                assert actual == expected, (index, command, actual, expected)
            report['reply_checks'] += 1
            if index % 1000 == 999:
                assert snapshot(server.client) == snapshot(reference), ('state', index)
                report['state_checks'] += 1
                (root/'progress.json').write_text(json.dumps(report,indent=2)+'\n')
        report['scan_checks'] += check_scan(server.client, reference)
        report['geo_checks'] = check_geo(server.client, reference, populate=True)
        expected = snapshot(reference)
        assert snapshot(server.client) == expected
        rewrite(server.client)
        for _ in range(2):
            server.stop(crash=True)
            server.start()
            assert snapshot(server.client) == expected, 'AOF state differs after crash/restart'
            report['geo_checks'] += check_geo(server.client, reference)
            report['restarts'] += 1
        report.update(status='passed', final_keys=len(expected),
                      state_sha256=hashlib.sha256(repr(expected).encode()).hexdigest())
    except BaseException as exc:
        report.update(status='failed',failure=repr(exc))
        raise
    finally:
        server.stop(check=False)
        if reference:
            reference.close()
        if redis is not None and redis.poll() is None:
            redis.terminate()
            try:
                redis.wait(timeout=5)
            except subprocess.TimeoutExpired:
                redis.kill()
                redis.wait()
        log.close()
        trace.close()
        report['commands_sha256'] = sha256(root/'commands.jsonl')
        (root/'report.json').write_text(json.dumps(report,indent=2)+'\n')
    print(json.dumps(report,indent=2))


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--concurrent', action='store_true')
    parser.add_argument('--bin',type=Path,required=True)
    parser.add_argument('--redis',type=Path,required=True)
    parser.add_argument('--out',type=Path,required=True)
    parser.add_argument('--seed',type=int,default=20260906)
    parser.add_argument('--steps',type=int,default=10000)
    parser.add_argument('--policy',choices=['always','everysec','no'],default='always')
    args=parser.parse_args()
    if not 1 <= args.steps <= 10000000:
        parser.error('steps must be 1..10000000')
    run(args)
