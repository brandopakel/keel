#!/usr/bin/env python3
"""Replay deterministic mixed RESP2 operations against Keel and Redis.

Compare replies and final state for the supported common command contract.
Unordered sets/hash fields are normalized; error text is compared by RESP error
class. Deliberate differences (SET over another type, Redis dumps/modules) are
outside this test, not silently accepted mismatches.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import random
import socket
import subprocess
import time

from validation_lib import Client, Server, rewrite, sha256


def execute(client, command):
    try:
        value = client.call(*command)
        if command[0] in ('SMEMBERS', 'HKEYS', 'HVALS', 'KEYS'):
            value = sorted(value)
        if command[0] == 'HGETALL':
            value = sorted(zip(value[::2], value[1::2]))
        return ('ok', value)
    except RuntimeError as exc:
        message = str(exc).removeprefix('(error) ')
        return ('error', message.split()[0])


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
        ])
    key = rng.choice(['string','hash','list','set','zset']) + ':' + str(rng.randrange(32))
    return rng.choice([['DEL', key], ['EXISTS', key, 'missing'], ['TYPE', key],
                       ['PERSIST', key], ['PEXPIREAT', key, 4102444800000]])


def run(args):
    os.umask(0o077)
    root = args.out.resolve()
    root.mkdir(parents=True, exist_ok=False)
    report = {'status': 'running', 'seed': args.seed, 'steps_requested': args.steps,
              'binary_sha256': sha256(args.bin), 'redis_sha256': sha256(args.redis),
              'harness_sha256': sha256(__file__), 'policy': args.policy,
              'reply_checks': 0, 'state_checks': 0, 'restarts': 0,
              'limits': 'Supported common RESP2 commands; unordered collections normalized; errors compared by class. No timing-based TTL differential or cross-type SET equivalence claim.'}
    server = Server(args.bin, root/'keel', policy=args.policy)
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
        rng = random.Random(args.seed)
        for index in range(args.steps):
            command = operation(rng)
            trace.write(json.dumps([{'hex':part.hex()} if isinstance(part,bytes) else part for part in command])+'\n')
            actual, expected = execute(server.client, command), execute(reference, command)
            assert actual == expected, (index, command, actual, expected)
            report['reply_checks'] += 1
            if index % 1000 == 999:
                assert snapshot(server.client) == snapshot(reference), ('state', index)
                report['state_checks'] += 1
                (root/'progress.json').write_text(json.dumps(report,indent=2)+'\n')
        expected = snapshot(reference)
        assert snapshot(server.client) == expected
        rewrite(server.client)
        for _ in range(2):
            server.stop(crash=True)
            server.start()
            assert snapshot(server.client) == expected, 'AOF state differs after crash/restart'
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
