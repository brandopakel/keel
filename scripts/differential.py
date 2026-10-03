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

With --protocol 3 both servers are connected with HELLO 3 and every reply is
compared with its RESP3 type: a map against a map, a double against a double,
a null against a null. That mode also covers the rest of the supported command
surface whose replies Redis gives deterministically, transactions among them,
including a HELLO queued inside one, and, with --redis-module naming
RedisBloom, the BF, CF and CMS commands. The same normalization applies and
nothing else is relaxed; replies whose content legitimately differs (HELLO,
INFO, MEMORY, module INFO) are compared by type and field names.
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

from resp3_client import RESP3Client
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

# RESP3 mode.
#
# Replies are the tagged tuples RESP3Client returns. Normalization is the RESP2
# mode's and no more: a set's members and a map's pairs are unordered by
# definition, and the arrays that mode sorts (HKEYS, HVALS, KEYS) are sorted
# here too, with SRANDMEMBER, which is only sent where it returns a whole set.

UNORDERED_ARRAYS = ('HKEYS', 'HVALS', 'KEYS', 'SRANDMEMBER')
# A set's members and a hash's pairs as RESP2 frames them, which a RESP3
# connection receives between a queued HELLO 2 and HELLO 3: the RESP2 mode's
# normalization for the same replies.
UNORDERED_RESP2 = ('SMEMBERS', 'SPOP')
ITEMS = [f'item:{i}' for i in range(32)]


def error_class3(text):
    return text.removeprefix(b'(error) ').split()[0] if text.strip() else b''


def hello_shape(value):
    """A HELLO reply as its protocol and field names: the values (server name,
    version, connection id, modules) are each server's own. RESP2 sends the
    map flattened into an array."""
    if value[0] == 'map':
        pairs = value[1]
    elif value[0] == 'array':
        pairs = list(zip(value[1][::2], value[1][1::2]))
    else:
        return value
    fields = dict(pairs)
    return ('hello', fields.get(('bulk', b'proto')), sorted(key for key, _ in pairs))


def normalize3(command, value):
    if command[0] == 'HELLO':
        return hello_shape(value)
    def walk(v):
        kind = v[0]
        if kind == 'error':
            return ('error', error_class3(v[1]))
        if kind in ('array', 'push'):
            return (kind, [walk(x) for x in v[1]])
        if kind == 'set':
            return ('set', sorted((walk(x) for x in v[1]), key=repr))
        if kind == 'map':
            return ('map', sorted(((walk(k), walk(x)) for k, x in v[1]), key=repr))
        return v
    value = walk(value)
    if command[0] in UNORDERED_ARRAYS + UNORDERED_RESP2 and value[0] == 'array':
        value = ('array', sorted(value[1], key=repr))
    if command[0] == 'HGETALL' and value[0] == 'array':
        assert len(value[1]) % 2 == 0, 'truncated HGETALL response'
        value = ('array', sorted(zip(value[1][::2], value[1][1::2]), key=repr))
    return value


def execute3(client, command):
    try:
        return ('ok', normalize3(command, client.call(*command)))
    except RuntimeError as exc:
        return ('error', str(exc).removeprefix('(error) ').split()[0])


def close_numbers(got, want, tolerance):
    """Geo replies carry floats each implementation computes for itself. The
    RESP2 mode's tolerances apply - .00011 for a four-decimal distance, 1e-12
    for a coordinate - and only to values both sides send as the same type."""
    if got[0] != want[0]:
        return False
    if got[0] in ('array', 'set'):
        return len(got[1]) == len(want[1]) and all(close_numbers(a, b, tolerance) for a, b in zip(got[1], want[1]))
    if got[0] == 'double':
        return math.isclose(float(got[1]), float(want[1]), rel_tol=0, abs_tol=1e-12)
    if got[0] == 'bulk' and got[1] != want[1]:
        try:
            return abs(float(got[1]) - float(want[1])) < tolerance
        except ValueError:
            return False
    return got == want


GEO_COMMANDS = ('GEODIST', 'GEOPOS', 'GEOSEARCH')


def same_reply(command, actual, expected):
    if command[0] in GEO_COMMANDS and actual[0] == expected[0] == 'ok':
        return close_numbers(actual[1], expected[1], .00011)
    return actual == expected


def operation3(rng, modules):
    kinds = ['base'] * 4 + ['string', 'key', 'set', 'zset', 'geo', 'hll', 'connection']
    if modules:
        kinds += ['bloom', 'cuckoo', 'cms']
    kind = rng.choice(kinds)
    if kind == 'base':
        return operation(rng)
    value = rng.choice([b'', b'\x00\xff\r\n', b'x'*64, b'ohmytext', b'mynewtext', b'007', b'-0'])
    if kind == 'string':
        key, other = ['string:' + str(rng.randrange(32)) for _ in range(2)]
        return rng.choice([
            ['SETNX', key, value], ['UNLINK', key, 'missing'], ['DECR', key], ['GET', key],
            ['SET', key, value, 'EX', 100000], ['SET', key, value, 'PXAT', 4102444800000],
            ['SET', key, value, 'KEEPTTL'], ['SET', key, value, 'NX', 'GET'],
            ['LCS', key, other], ['LCS', key, other, 'LEN'], ['LCS', key, other, 'IDX'],
            ['LCS', key, other, 'IDX', 'WITHMATCHLEN', 'MINMATCHLEN', rng.randrange(4)],
            ['EXPIRE', key, 100000], ['EXPIRE', key, 100000, 'NX'], ['PEXPIRE', key, 100000000, 'GT'],
            ['EXPIREAT', key, 4102444800, 'XX'], ['EXPIRE', key, -1],
        ])
    if kind == 'key':
        key = rng.choice(['string', 'hash', 'list', 'set', 'zset']) + ':' + str(rng.randrange(32))
        hash_key = 'hash:' + str(rng.randrange(32))
        return rng.choice([['DBSIZE'], ['KEYS', 'string:1*'], ['EXISTS', key, key, 'missing'],
                           ['HKEYS', hash_key], ['HVALS', hash_key], ['HKEYS', 'missing'],
                           ['TYPE', key], ['UNLINK', key], ['TTL', 'missing'], ['PTTL', 'missing'],
                           ['EXPIRE', 'missing', 10], ['PERSIST', 'missing']])
    if kind == 'set':
        key, single = 'set:' + str(rng.randrange(32)), 'single:' + str(rng.randrange(4))
        # SPOP and SRANDMEMBER pick at random, so they are sent only where the
        # pick is determined: a whole set, nothing, or a set of one member.
        return rng.choice([
            ['SPOP', key, 100], ['SPOP', 'missing'], ['SPOP', 'missing', 2], ['SPOP', key, 0],
            ['SRANDMEMBER', key, 100], ['SRANDMEMBER', key, 0], ['SRANDMEMBER', 'missing'],
            ['SRANDMEMBER', 'missing', 3], ['SADD', single, 'x'], ['SPOP', single], ['SPOP', single, 1],
            ['SRANDMEMBER', single], ['SRANDMEMBER', single, -3], ['SMEMBERS', single], ['SCARD', single],
        ])
    if kind == 'zset':
        key, member = 'zset:' + str(rng.randrange(32)), 'member:' + str(rng.randrange(16))
        # Scores a binary fraction can hold exactly print the same in every
        # Redis release; others need 7.2's shortest form, which Keel follows.
        score = rng.choice(['2.5', '-0.25', '0.125', '1.75', 'inf', '-inf'])
        return rng.choice([
            ['ZADD', key, score, member], ['ZINCRBY', key, rng.choice(['0.5', '-1.25', 'inf']), member],
            ['ZPOPMIN', key], ['ZPOPMAX', key], ['ZPOPMIN', key, 1], ['ZPOPMAX', key, 2],
            ['ZRANGEBYSCORE', key, '-inf', '+inf', 'WITHSCORES'], ['ZREVRANGEBYSCORE', key, '+inf', '(0', 'WITHSCORES'],
            ['ZRANGE', key, 0, -1, 'REV', 'WITHSCORES'], ['ZSCORE', key, member], ['ZRANK', key, member],
            ['ZCOUNT', key, '-inf', '(1'],
        ])
    if kind == 'geo':
        key = 'geo:' + str(rng.randrange(4))
        member, other = ['place:' + str(rng.randrange(24)) for _ in range(2)]
        lon, lat = (f'{(rng.random()-.5)*20:.6f}' for _ in range(2))
        unit = rng.choice(['m', 'km', 'mi', 'ft'])
        flags = [option for option in ('WITHCOORD', 'WITHDIST', 'WITHHASH') if rng.random() < .5]
        return rng.choice([
            ['GEOADD', key, lon, lat, member], ['GEOADD', key, 'NX', lon, lat, member],
            ['GEOADD', key, 'XX', 'CH', lon, lat, member], ['GEODIST', key, member, other, unit],
            ['GEODIST', key, member, other], ['GEOHASH', key, member, 'missing'], ['GEOPOS', key, member, 'missing'],
            ['GEOPOS', 'missing', member], ['GEOSEARCH', key, 'FROMMEMBER', member, 'BYRADIUS', 800, 'km', 'ASC', *flags],
            ['GEOSEARCH', key, 'FROMLONLAT', lon, lat, 'BYBOX', 1500, 1500, 'km', 'DESC', 'COUNT', 3, *flags],
            ['ZREM', key, member], ['ZCARD', key],
        ])
    if kind == 'hll':
        key, other = ['hll:' + str(rng.randrange(4)) for _ in range(2)]
        # Few distinct elements, so a register collision - the two hash
        # differently - cannot make the estimates differ.
        elements = [f'e{rng.randrange(4)}' for _ in range(rng.randrange(3))]
        return rng.choice([['PFADD', key, *elements], ['PFCOUNT', key], ['PFCOUNT', key, other],
                           ['PFMERGE', key, other]])
    if kind == 'connection':
        return rng.choice([['PING'], ['PING', value], ['ECHO', value], ['SELECT', 0], ['CLIENT', 'GETNAME'],
                           ['CLIENT', 'SETNAME', 'differential'], ['CLIENT', 'SETINFO', 'LIB-NAME', 'resp3-differential']])
    key, item, other = 'bf:' + str(rng.randrange(4)), rng.choice(ITEMS), rng.choice(ITEMS)
    if kind == 'bloom':
        # Filters reserved large and tight at setup: the two implementations
        # hash differently, so a false positive would differ between them.
        return rng.choice([['BF.ADD', key, item], ['BF.MADD', key, item, other], ['BF.EXISTS', key, item],
                           ['BF.MEXISTS', key, item, other], ['BF.EXISTS', 'bf:missing', item],
                           ['BF.RESERVE', key, 0.01, 100]])
    if kind == 'cuckoo':
        key = 'cf:' + str(rng.randrange(4))
        return rng.choice([['CF.ADD', key, item], ['CF.ADDNX', key, item], ['CF.EXISTS', key, item],
                           ['CF.MEXISTS', key, item, other], ['CF.DEL', key, item], ['CF.COUNT', key, item],
                           ['CF.EXISTS', 'cf:missing', item]])
    key = 'cms:' + str(rng.randrange(4))
    # A sketch sized by probability is only queried: how each sizes it from
    # the same error and probability is not the reply being compared.
    sized = 'cms:p' + str(rng.randrange(2))
    return rng.choice([['CMS.INCRBY', key, item, rng.randrange(1, 5), other, 1], ['CMS.QUERY', key, item, other],
                       ['CMS.INCRBY', 'cms:missing', item, 1], ['CMS.INITBYDIM', key, 10, 5],
                       ['CMS.INITBYPROB', sized, 0.001, 0.01], ['CMS.QUERY', sized, item]])


def transaction3(rng, modules):
    """transaction()'s blocks, of RESP3-mode operations. Some queue HELLO 2 and,
    later in the same block, HELLO 3: Redis runs a queued HELLO in its place at
    EXEC, so the replies between the two are RESP2 inside a RESP3 EXEC, and the
    connection is back on RESP3 when the block ends."""
    commands = [operation3(rng, modules) for _ in range(rng.randrange(6))]
    shape = rng.random()
    refusal = None
    if shape < .08:
        refusal = ['NOSUCHCOMMAND', 'x']
    elif shape < .16:
        refusal = rng.choice([['GET'], ['HSET', 'hash:0'], ['SETEX', 'string:0', 10], ['LRANGE', 'list:0', 0]])
    elif shape < .22:
        refusal = ['MULTI']
    elif shape < .4:
        # Between the two HELLOs replies are RESP2, in which the BF and CF
        # commands answer as RedisBloom's do too.
        down = rng.randrange(len(commands)+1)
        commands.insert(down, ['HELLO', 2])
        commands.insert(rng.randrange(down+1, len(commands)+1), ['HELLO', 3])
    if refusal:
        commands.insert(rng.randrange(len(commands)+1), refusal)
    return commands, ['DISCARD'] if shape > .92 else ['EXEC']


def execute_transaction3(client, commands, ending):
    """Every reply of a MULTI block, typed: the OK, each QUEUED or refusal, and
    the EXEC or DISCARD reply, each element of an EXEC normalized as its
    command's reply would be on its own."""
    replies = []
    for command in (['MULTI'], *commands, ending):
        parts = [p if isinstance(p, bytes) else str(p).encode() for p in command]
        client.socket.sendall(b'*%d\r\n' % len(parts)+b''.join(b'$%d\r\n' % len(p)+p+b'\r\n' for p in parts))
        replies.append(client.read())
    result = replies[-1]
    if ending == ['EXEC'] and result[0] == 'array':
        queued = [command for command, reply in zip(commands, replies[1:-1]) if reply == ('simple', b'QUEUED')]
        assert len(queued) == len(result[1]), ('EXEC reply length', commands, replies)
        replies[-1] = ('array', [normalize3(command, value) for command, value in zip(queued, result[1])])
    return [normalize3(['MULTI'], reply) for reply in replies[:-1]] + [replies[-1] if replies[-1][0] == 'array' else normalize3(ending, replies[-1])]


def setup_modules3(candidate, reference):
    # Each reservation twice: the second is refused, in RedisBloom's words.
    checks = 0
    for i in range(4):
        for command in (['BF.RESERVE', f'bf:{i}', 0.000001, 100000], ['CF.RESERVE', f'cf:{i}', 100000],
                        ['CMS.INITBYDIM', f'cms:{i}', 2000, 5], ['BF.RESERVE', f'bf:{i}', 0.000001, 100000],
                        ['CF.RESERVE', f'cf:{i}', 100000]):
            actual, expected = execute3(candidate, command), execute3(reference, command)
            assert actual == expected, (command, actual, expected)
            checks += 1
    return checks


def snapshot3(client):
    """Every key, and each one's contents read back in RESP3. HyperLogLogs and
    module keys are read through their own commands: Redis keeps an HLL in a
    string and a filter in a module type, and TYPE names neither as Keel does."""
    keys = sorted(item[1] for item in client.call('KEYS', '*')[1])
    result = [('keys', keys)]
    for key in keys:
        prefix = key.split(b':', 1)[0]
        if prefix == b'hll':
            command = ['PFCOUNT', key]
        elif prefix == b'bf':
            command = ['BF.MEXISTS', key, *ITEMS]
        elif prefix == b'cf':
            command = ['CF.MEXISTS', key, *ITEMS]
        elif prefix == b'cms':
            command = ['CMS.QUERY', key, *ITEMS]
        else:
            kind = client.call('TYPE', key)[1]
            command = {b'string': ['GET', key], b'hash': ['HGETALL', key],
                       b'list': ['LRANGE', key, 0, -1], b'set': ['SMEMBERS', key],
                       b'zset': ['ZRANGE', key, 0, -1, 'WITHSCORES']}[kind]
        result.append((key, command[0], execute3(client, command)))
    return result


def scan_all3(client, *options):
    cursor, keys = b'0', set()
    for _ in range(1000000):
        reply = client.call('SCAN', cursor, *options)
        assert reply[0] == 'array' and len(reply[1]) == 2, reply
        (kind, cursor), (batch_kind, batch) = reply[1]
        assert kind == 'bulk' and batch_kind == 'array', reply
        keys.update(item[1] for item in batch)
        if cursor == b'0':
            return sorted(keys)
    raise AssertionError('SCAN did not terminate')


def check_scan3(candidate, reference, *, typed_strings=True):
    # Redis's TYPE string includes its HyperLogLogs, which Keel types as hll,
    # so that filter is left out once they exist.
    options = [(), ('MATCH', ''), ('TYPE', ''), ('TYPE', 'SeT'), ('TYPE', 'hash'),
               ('MATCH', 'string:*', 'COUNT', '1'), ('MATCH', '*[0-9]', 'COUNT', '10'), ('TYPE', 'unknown')]
    if typed_strings:
        options.append(('TYPE', 'STRING'))
    for option in options:
        assert scan_all3(candidate, *option) == scan_all3(reference, *option), ('SCAN', option)
    return len(options)


def check_geo3(candidate, reference, *, populate=False):
    """check_geo's selection and option coverage, compared with RESP3 types."""
    if populate:
        rng = random.Random(741)
        for index in range(1000):
            span = (10, 10) if index < 700 else (360, 170)
            lon, lat = [(rng.random()-.5)*width for width in span]
            command = ['GEOADD', 'geo:admission', lon, lat, f'member:{index}']
            assert candidate.call(*command) == reference.call(*command)
    checks = 0
    member = lambda row: row[1][0] if row[0] == 'array' else row
    for radius in (50, 1000, 40000):
        for order in ('', 'ASC', 'DESC'):
            for count in (0, 1, 7, 100, 10000000):
                for flags in range(8):
                    command = ['GEOSEARCH', 'geo:admission', 'FROMLONLAT', 0, 0, 'BYRADIUS', radius, 'km']
                    if order:
                        command.append(order)
                    if count:
                        command += ['COUNT', count]
                    for bit, option in enumerate(('WITHDIST', 'WITHHASH', 'WITHCOORD')):
                        if flags & (1 << bit):
                            command.append(option)
                    actual, expected = candidate.call(*command), reference.call(*command)
                    assert actual[0] == expected[0] == 'array', (command, actual[0], expected[0])
                    if not order and not count:
                        actual = ('array', sorted(actual[1], key=lambda row: repr(member(row))))
                        expected = ('array', sorted(expected[1], key=lambda row: repr(member(row))))
                    assert close_numbers(actual, expected, .00011), (command, actual, expected)
                    checks += 1
        for flags in range(8):
            base = ['GEOSEARCH', 'geo:admission', 'FROMLONLAT', 0, 0, 'BYRADIUS', radius, 'km']
            for bit, option in enumerate(('WITHDIST', 'WITHHASH', 'WITHCOORD')):
                if flags & (1 << bit):
                    base.append(option)
            rows = {repr(member(row)): row for row in reference.call(*base)[1]}
            for count in (1, 7, 10000000):
                command = base + ['COUNT', count, 'ANY']
                for client in (candidate, reference):
                    actual = client.call(*command)
                    assert actual[0] == 'array' and len(actual[1]) == min(count, len(rows)), (command, actual)
                    names = [repr(member(row)) for row in actual[1]]
                    assert len(names) == len(set(names)), (command, names)
                    for row, name in zip(actual[1], names, strict=True):
                        assert name in rows and close_numbers(row, rows[name], .00011), (command, row)
                checks += 1
    return checks


def field_types(value):
    """A map's field names, each with its value's type: the shape compared
    where the values themselves differ between the two servers."""
    assert value[0] == 'map', value
    return sorted((key, item[0]) for key, item in value[1])


def check_shapes3(candidate, reference, modules):
    """Replies whose content legitimately differs between the two servers -
    server name and version, connection ids, memory figures - compared by type
    and field name. Each is otherwise the reply a RESP3 client decodes."""
    checks = 0
    for command in (['HELLO', 3], ['HELLO']):
        got, want = candidate.call(*command), reference.call(*command)
        assert field_types(got) == field_types(want), (command, got, want)
        assert dict(got[1])[('bulk', b'proto')] == dict(want[1])[('bulk', b'proto')] == ('int', 3), (command, got, want)
        checks += 1
    for command, needle in ((['INFO'], None), (['INFO', 'server'], b'resp_version:3\r\n'), (['CLIENT', 'INFO'], b' resp=3 ')):
        got, want = candidate.call(*command), reference.call(*command)
        assert got[:2] == want[:2] == ('verbatim', b'txt'), (command, got[:2], want[:2])
        if needle is not None:
            assert needle in got[2], (command, got)
        if command[0] == 'CLIENT':
            assert needle in want[2], (command, want)
        checks += 1
    for client in (candidate, reference):
        client.call('SET', 'shape:ttl', 'v', 'EX', 1000)
    for command, kind in ((['CLIENT', 'ID'], 'int'), (['MEMORY', 'USAGE', 'shape:ttl'], 'int'),
                          (['TTL', 'shape:ttl'], 'int'), (['PTTL', 'shape:ttl'], 'int'),
                          (['MEMORY', 'STATS'], 'map')):
        got, want = candidate.call(*command), reference.call(*command)
        assert got[0] == want[0] == kind, (command, got, want)
        checks += 1
    for client in (candidate, reference):
        client.call('DEL', 'shape:ttl')
    if modules:
        for i in range(4):
            got, want = candidate.call('BF.INFO', f'bf:{i}'), reference.call('BF.INFO', f'bf:{i}')
            assert field_types(got) == field_types(want), (got, want)
            values = lambda reply: {key: item for key, item in reply[1] if key != ('simple', b'Size')}
            assert values(got) == values(want), (got, want)
            # CF.INFO's fields are RedisBloom's, in its order. The memory
            # figures are each implementation's own, and so is the geometry:
            # Keel's filters have BUCKETSIZE 4, MAXITERATIONS 500 and
            # EXPANSION 0, and these were reserved with RedisBloom's defaults,
            # under which RedisBloom grows a filter where Keel's refuses an
            # item, and compacts it later, resetting its count of deletions.
            got, want = candidate.call('CF.INFO', f'cf:{i}'), reference.call('CF.INFO', f'cf:{i}')
            assert [(key, item[0]) for key, item in got[1]] == [(key, item[0]) for key, item in want[1]], (got, want)
            own = {b'Size', b'Number of buckets', b'Bucket size', b'Max iterations', b'Expansion rate',
                   b'Number of filters', b'Number of items deleted'}
            values = lambda reply: [(key, item) for key, item in reply[1] if key[1] not in own]
            assert values(got) == values(want), (got, want)
            checks += 2
    return checks


def run3(args, server, reference_port, report, trace, root):
    """The RESP3 differential, on servers run() has started."""
    connect = lambda port: RESP3Client('127.0.0.1', port, server.password)
    candidate, reference = connect(server.port), connect(reference_port)
    try:
        for command in (['FLUSHDB'], ['CLIENT', 'GETNAME'], ['CLIENT', 'MAINT_NOTIFICATIONS', 'ON'],
                        ['TTL', 'missing'], ['PTTL', 'missing'], ['MEMORY', 'USAGE', 'missing'], ['DBSIZE']):
            actual, expected = execute3(candidate, command), execute3(reference, command)
            assert actual == expected, (command, actual, expected)
            report['reply_checks'] += 1
        modules = args.redis_module is not None
        report['shape_checks'] = check_shapes3(candidate, reference, False)
        for client in (candidate, reference):
            assert client.call('SET', b'', b'empty-key') == ('simple', b'OK')
        report['scan_checks'] = check_scan3(candidate, reference)
        if modules:
            report['reply_checks'] += setup_modules3(candidate, reference)
            report['shape_checks'] += check_shapes3(candidate, reference, True)
        covered = set()
        report['transaction_checks'] = 0
        rng = random.Random(args.seed)
        encode = lambda command: [{'hex':part.hex()} if isinstance(part,bytes) else part for part in command]
        name = lambda command: ' '.join(str(part) for part in command[:2]) if command[0] in ('CLIENT', 'MEMORY') else command[0]
        for index in range(args.steps):
            if rng.random() < .1:
                commands, ending = transaction3(rng, modules)
                covered.update(name(command) for command in (['MULTI'], *commands, ending))
                trace.write(json.dumps({'transaction': [encode(c) for c in commands], 'end': ending})+'\n')
                actual = execute_transaction3(candidate, commands, ending)
                expected = execute_transaction3(reference, commands, ending)
                geo = any(command[0] in GEO_COMMANDS for command in commands)
                assert (close_numbers(('array', actual), ('array', expected), .00011) if geo else actual == expected), \
                    (index, commands, ending, actual, expected)
                report['transaction_checks'] += 1
            else:
                command = operation3(rng, modules)
                covered.add(name(command))
                trace.write(json.dumps(encode(command))+'\n')
                actual, expected = execute3(candidate, command), execute3(reference, command)
                assert same_reply(command, actual, expected), (index, command, actual, expected)
            report['reply_checks'] += 1
            if index % 1000 == 999:
                assert snapshot3(candidate) == snapshot3(reference), ('state', index)
                report['state_checks'] += 1
                (root/'progress.json').write_text(json.dumps(report,indent=2)+'\n')
        report['commands_covered'] = sorted(covered)
        report['scan_checks'] += check_scan3(candidate, reference, typed_strings=False)
        report['geo_checks'] = check_geo3(candidate, reference, populate=True)
        if modules:
            report['shape_checks'] += check_shapes3(candidate, reference, True)
        expected = snapshot3(reference)
        assert snapshot3(candidate) == expected
        rewrite(server.client)
        for _ in range(2):
            candidate.close()
            server.stop(crash=True)
            server.start()
            candidate = connect(server.port)
            assert snapshot3(candidate) == expected, 'AOF state differs after crash/restart'
            report['geo_checks'] += check_geo3(candidate, reference)
            report['restarts'] += 1
        return expected, len(expected[0][1])
    finally:
        candidate.close()
        reference.close()


def run2(args, server, reference, report, trace, root):
    """The RESP2 differential, on servers run() has started."""
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
    return expected, len(expected)


def run(args):
    os.umask(0o077)
    root = args.out.resolve()
    root.mkdir(parents=True, exist_ok=False)
    report = {'status': 'running', 'seed': args.seed, 'steps_requested': args.steps,
              'binary_sha256': sha256(args.bin), 'redis_sha256': sha256(args.redis),
              'harness_sha256': sha256(__file__), 'policy': args.policy, 'concurrent': args.concurrent,
              'reply_checks': 0, 'transaction_checks': 0, 'state_checks': 0, 'restarts': 0,
              'limits': 'Supported common RESP2 commands, alone and in MULTI/EXEC/DISCARD blocks; unordered collections normalized; errors compared by class, inside EXEC replies too. No timing-based TTL differential or WATCH claim.'}
    if args.protocol == 3:
        report.update(protocol=3, redis_module=str(args.redis_module) if args.redis_module else None,
                      redis_module_sha256=sha256(args.redis_module) if args.redis_module else None,
                      limits='Supported commands over RESP3 (HELLO 3 AUTH on both servers), replies compared with their '
                             'RESP3 types; sets, maps and the RESP2 mode\'s unordered arrays normalized; errors compared '
                             'by class; geo floats within the RESP2 mode\'s tolerances. HELLO, INFO, CLIENT INFO/ID, MEMORY, '
                             'TTL/PTTL of a live key and module INFO compared by type and field names. BF/CF/CMS only with '
                             '--redis-module. Not compared: Keel-only commands (KEEL.*, MEMKV.*, MORRIS.*, SRAND), '
                             'BGREWRITEAOF, SELECT of another database, and random picks other than those determined by the set. '
                             'Transactions as in the RESP2 mode, with HELLO 2 and HELLO 3 queued inside some; no WATCH claim. '
                             'BF and CF replies and errors are compared byte for byte by scripts/redisbloom-parity.py.')
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
        module = f'loadmodule {args.redis_module.absolute()}\n' if args.redis_module else ''
        redis.stdin.write((f'bind 127.0.0.1\nport {port}\nsave ""\nappendonly no\n'
                           f'requirepass {server.password}\n{module}').encode())
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
        if args.protocol == 3:
            expected, final_keys = run3(args, server, port, report, trace, root)
        else:
            expected, final_keys = run2(args, server, reference, report, trace, root)
        report.update(status='passed', final_keys=final_keys,
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
    parser.add_argument('--protocol',type=int,choices=[2,3],default=2,
                        help='3 negotiates HELLO 3 on both servers and compares RESP3 replies')
    parser.add_argument('--redis-module',type=Path,
                        help='RedisBloom module for the reference; adds BF, CF and CMS to --protocol 3')
    args=parser.parse_args()
    if args.redis_module and args.protocol != 3:
        parser.error('--redis-module applies to --protocol 3')
    if not 1 <= args.steps <= 10000000:
        parser.error('steps must be 1..10000000')
    run(args)
