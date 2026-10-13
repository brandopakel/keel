#!/usr/bin/env python3
"""Compare how Keel's INFO counters move with Redis's, command by command.

Each case runs on both servers the same way, on one connection: FLUSHDB, the
same base keys, CONFIG RESETSTAT, the case's commands, then INFO stats and
errorstats. What the case moved is compared field by field:
total_commands_processed, total_error_replies, keyspace_hits,
keyspace_misses, expired_keys, every errorstat_ line, and each cmdstat_
line's calls, rejected_calls and failed_calls (docs/info-compatibility.md,
parts c and h). The time a command took is not compared, being the time it
took; replies are not compared either: scripts/error-parity.py does that.

The expiry cases run on a second pair of servers whose expiry cycle is off,
Keel's by -active-expire-samples 0 and Redis's by DEBUG SET-ACTIVE-EXPIRE 0,
so that a key whose TTL has passed is removed only by the command the case
sends, and each server counts the same removals whenever its cycle would
have run. A case's ['SLEEP', ms] waits instead of sending anything.

A field Keel does not report yet is listed under not_reported rather than as
a mismatch, so the script can run ahead of the part that adds the field.
Commands only Keel has (KEEL.*, MEMKV.*, MORRIS.*, SRAND) have nothing to
compare against. The BF, CF and CMS cases run only when --redis-module names
RedisBloom.

Exit status: 0 when every compared field matched, 1 otherwise.
"""

import argparse
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
import time

HERE = Path(__file__).resolve().parent
_spec = importlib.util.spec_from_file_location('error_parity', HERE / 'error-parity.py')
parity = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(parity)

FIELDS = ('total_commands_processed', 'total_error_replies', 'keyspace_hits', 'keyspace_misses', 'expired_keys')

BASE = [['SET', 'str', 'abc'], ['SET', 'num', '10'], ['SET', 'other', 'abd'], ['HSET', 'hash', 'f', 'v', 'n', '1'],
        ['RPUSH', 'list', 'a', 'b', 'c'], ['SADD', 'set', 'a', 'b'], ['ZADD', 'zset', '1', 'a', '2', 'b'],
        ['PFADD', 'hll', 'a', 'b'], ['GEOADD', 'geo', '13.361389', '38.115556', 'palermo', '15.087269', '37.502669', 'catania']]
MODULE_BASE = [['BF.RESERVE', 'bf', '0.01', '100'], ['BF.ADD', 'bf', 'a'], ['CF.RESERVE', 'cf', '100'], ['CF.ADD', 'cf', 'a'],
               ['CMS.INITBYDIM', 'cms', '10', '5']]

# Each case is a list of commands sent in turn on one connection.
CASES = [[c] for c in [
    # Strings: present, missing, the wrong type, and the writes.
    ['GET', 'str'], ['GET', 'missing'], ['GET', 'hash'], ['SET', 'k', 'v'], ['SET', 'str', 'x', 'GET'],
    ['SET', 'missing', 'x', 'GET'], ['SET', 'hash', 'x', 'GET'], ['SET', 'str', 'x', 'NX'], ['SET', 'str', 'x', 'XX'],
    ['SETNX', 'str', 'x'], ['SETNX', 'new', 'x'], ['INCR', 'num'], ['INCR', 'missing'], ['INCR', 'str'],
    ['INCRBY', 'num', '5'], ['DECR', 'num'], ['DECRBY', 'num', '2'], ['MGET', 'str', 'missing', 'num', 'hash'],
    ['MSET', 'a', '1', 'b', '2'], ['SETEX', 'k', '10', 'v'], ['PSETEX', 'k', '1000', 'v'], ['LCS', 'str', 'other'],
    ['LCS', 'str', 'missing'], ['STRLEN', 'str'], ['STRLEN', 'missing'], ['STRLEN', 'hash'],
    ['GETRANGE', 'str', '0', '1'], ['GETRANGE', 'missing', '0', '1'], ['SUBSTR', 'str', '0', '1'], ['GETEX', 'str'],
    # A read whose handler parses arguments before it looks its key up
    # counts nothing for a malformed argument, and a hit for another type.
    ['GETRANGE', 'str', 'x', '1'], ['GETRANGE', 'missing', '0', 'y'], ['GETRANGE', 'hash', '0', '1'],
    ['GETRANGE', 'hash', 'x', '1'], ['SUBSTR', 'str', '0', 'y'], ['SUBSTR', 'hash', '0', '1'], ['GETEX', 'str', 'BAD'],
    ['GETEX', 'missing', 'EX'], ['GETEX', 'hash', 'BAD'], ['GETEX', 'hash', 'EX', '1'], ['GETEX', 'str', 'EX', 'x'],
    ['GETEX', 'missing'], ['GETEX', 'str', 'EX', '100'], ['GETEX', 'hash'], ['GETDEL', 'str'], ['GETDEL', 'missing'],
    ['GETDEL', 'hash'], ['GETSET', 'str', 'x'], ['GETSET', 'missing', 'x'], ['APPEND', 'str', 'x'],
    ['APPEND', 'missing', 'x'], ['SETRANGE', 'str', '0', 'x'], ['INCRBYFLOAT', 'num', '1.5'],
    ['INCRBYFLOAT', 'missing', '1.5'], ['MSETNX', 'str', '1', 'n2', '2'], ['MSETNX', 'n3', '1', 'n4', '2'],
    ['DIGEST', 'str'], ['DIGEST', 'missing'], ['DIGEST', 'hash'], ['DELEX', 'str'], ['DELEX', 'missing'],
    ['DELEX', 'str', 'IFEQ', 'abc'], ['DELEX', 'str', 'IFNE', 'abc'], ['DELEX', 'missing', 'IFEQ', 'x'],
    ['DELEX', 'hash', 'IFEQ', 'x'], ['SET', 'str', 'x', 'IFEQ', 'abc'], ['SET', 'str', 'x', 'IFEQ', 'nomatch'],
    ['SET', 'missing', 'x', 'IFEQ', 'y'], ['SET', 'missing', 'x', 'IFNE', 'y'], ['SET', 'str', 'x', 'GET', 'IFEQ', 'abc'],
    ['SET', 'hash', 'x', 'IFEQ', 'y'], ['MSETEX', '1', 'str', 'y', 'EX', '100'], ['MSETEX', '2', 'str', 'y', 'n5', 'z', 'NX'],
    ['INCREX', 'num'], ['INCREX', 'missing', 'BYFLOAT', '1.5'], ['INCREX', 'hash'],
    # The keyspace.
    ['DEL', 'str', 'missing'], ['UNLINK', 'str'], ['EXISTS', 'str', 'missing', 'str'], ['TYPE', 'str'],
    ['TYPE', 'missing'], ['KEYS', '*'], ['SCAN', '0'], ['TTL', 'str'], ['TTL', 'missing'], ['PTTL', 'str'],
    ['EXPIRETIME', 'str'], ['EXPIRETIME', 'missing'], ['PEXPIRETIME', 'hash'], ['TOUCH', 'str', 'missing', 'hash', 'str'],
    ['RENAME', 'str', 'renamed'], ['RENAME', 'missing', 'x'], ['RENAMENX', 'str', 'num'], ['COPY', 'str', 'copied'],
    ['COPY', 'missing', 'copied'], ['COPY', 'str', 'str'], ['COPY', 'hash', 'num', 'REPLACE'], ['RANDOMKEY'],
    ['DEL', 'missing'], ['PERSIST', 'missing'],
    ['EXPIRE', 'str', '100'], ['EXPIRE', 'missing', '100'], ['PEXPIRE', 'str', '100000'],
    ['EXPIREAT', 'str', '9999999999'], ['PEXPIREAT', 'str', '9999999999000'], ['PERSIST', 'str'], ['DBSIZE'],
    ['MEMORY', 'USAGE', 'str'], ['MEMORY', 'USAGE', 'missing'], ['CONFIG', 'GET', 'maxmemory'], ['ECHO', 'x'],
    ['PING'], ['SELECT', '0'], ['UNWATCH'], ['FLUSHDB'],
    # Hashes.
    ['HSET', 'hash', 'g', '2'], ['HSETNX', 'hash', 'f', 'x'], ['HGET', 'hash', 'f'], ['HGET', 'hash', 'nofield'],
    ['HGET', 'missing', 'f'], ['HGET', 'str', 'f'], ['HMGET', 'hash', 'f', 'g'], ['HDEL', 'hash', 'f'],
    ['HEXISTS', 'hash', 'f'], ['HLEN', 'hash'], ['HKEYS', 'hash'], ['HVALS', 'hash'], ['HGETALL', 'hash'],
    ['HGETALL', 'missing'], ['HINCRBY', 'hash', 'n', '1'],
    # Lists.
    ['LPUSH', 'list', 'x'], ['RPUSH', 'list', 'x'], ['LPOP', 'list'], ['RPOP', 'missing'], ['LTRIM', 'list', '0', '1'],
    ['LLEN', 'list'], ['LINDEX', 'list', '0'], ['LSET', 'list', '0', 'x'], ['LRANGE', 'list', '0', '-1'],
    ['LRANGE', 'missing', '0', '-1'],
    # A list read that reads its arguments before its key counts nothing for a
    # malformed one (lrangeCommand), and one that looks first counts either way
    # (lindexCommand); a pop looks to write, and counts nothing.
    ['LRANGE', 'list', 'x', '1'], ['LRANGE', 'missing', '0', 'x'], ['LRANGE', 'str', 'x', '1'],
    ['LRANGE', 'str', '0', '-1'], ['LINDEX', 'list', 'x'], ['LINDEX', 'missing', 'x'], ['LINDEX', 'str', '0'],
    ['LLEN', 'missing'], ['LLEN', 'str'], ['LPOP', 'missing', '2'], ['LPOP', 'str'], ['LPOP', 'list', 'x'],
    ['RPOP', 'list', '0'], ['LTRIM', 'missing', '0', '1'], ['LTRIM', 'str', 'x', '1'], ['LSET', 'missing', '0', 'x'],
    ['RPUSH', 'str', 'x'],
    # The list commands Keel added: LPOS reads its options before its key;
    # the rest look up to write, and count nothing.
    ['LPOS', 'list', 'a'], ['LPOS', 'missing', 'a'], ['LPOS', 'list', 'a', 'RANK', '0'], ['LPOS', 'missing', 'a', 'COUNT', 'x'],
    ['LPOS', 'str', 'a'], ['LPOS', 'str', 'a', 'RANK', '0'], ['LPOS', 'list', 'b', 'COUNT', '0'],
    ['LPUSHX', 'list', 'x'], ['RPUSHX', 'missing', 'x'], ['LINSERT', 'list', 'BEFORE', 'a', 'x'],
    ['LINSERT', 'missing', 'BEFORE', 'a', 'x'], ['LREM', 'list', '0', 'a'], ['LREM', 'missing', '0', 'a'],
    ['LMOVE', 'list', 'dst', 'LEFT', 'RIGHT'], ['LMOVE', 'missing', 'str', 'LEFT', 'RIGHT'], ['RPOPLPUSH', 'list', 'dst'],
    ['LMOVEM', 'list', 'dst', 'LEFT', 'RIGHT', 'COUNT', '2', 'BULK'], ['LMOVEM', 'missing', 'dst', 'LEFT', 'RIGHT'],
    ['LMPOP', '2', 'missing', 'list', 'LEFT', 'COUNT', '2'], ['LMPOP', '2', 'missing', 'str', 'LEFT'],
    ['LMPOP', '1', 'missing', 'RIGHT'],
    # Sets.
    ['SADD', 'set', 'c'], ['SREM', 'set', 'a'], ['SCARD', 'set'], ['SMEMBERS', 'set'], ['SISMEMBER', 'set', 'a'],
    ['SISMEMBER', 'missing', 'a'], ['SMISMEMBER', 'set', 'a', 'z'], ['SPOP', 'set'], ['SRANDMEMBER', 'set'],
    ['SRANDMEMBER', 'missing'],
    # Sorted sets.
    ['ZCOUNT', 'zset', '0', '10'], ['ZRANGEBYSCORE', 'zset', '0', '10'], ['ZREVRANGEBYSCORE', 'zset', '10', '0'],
    ['ZINCRBY', 'zset', '1', 'a'], ['ZPOPMIN', 'zset'], ['ZPOPMAX', 'zset'], ['ZRANGE', 'zset', '0', '-1'],
    ['ZADD', 'zset', '3', 'c'], ['ZRANK', 'zset', 'a'], ['ZREM', 'zset', 'a'], ['ZSCORE', 'zset', 'a'],
    ['ZSCORE', 'missing', 'a'], ['ZCARD', 'zset'],
    # Geo and HyperLogLog.
    ['GEOADD', 'geo', '1', '1', 'x'], ['GEODIST', 'geo', 'palermo', 'catania'], ['GEOHASH', 'geo', 'palermo'],
    ['GEOSEARCH', 'geo', 'FROMMEMBER', 'palermo', 'BYRADIUS', '200', 'km', 'ASC'], ['GEOPOS', 'geo', 'palermo', 'none'],
    ['GEOSEARCH', 'missing', 'FROMLONLAT', '15', '37', 'BYRADIUS', '10', 'km'], ['PFADD', 'hll', 'c'],
    ['PFCOUNT', 'hll'], ['PFCOUNT', 'hll', 'missing'], ['PFMERGE', 'dest', 'hll'],
    # Where a wrong type stops a command that reads several keys.
    ['LCS', 'hash', 'str'], ['LCS', 'str', 'hash'], ['PFCOUNT', 'hll', 'str', 'missing'],
    ['PFMERGE', 'dest', 'str', 'hll'], ['HMGET', 'missing', 'f'], ['ZRANK', 'missing', 'a'],
    # Refusals and the connection's own commands.
    ['NOSUCH'], ['GET'], ['CONFIG', 'nosuch'], ['CLIENT', 'ID'], ['CLIENT', 'SETNAME', 'a b'], ['AUTH', 'x'],
    ['LATENCY', 'LATEST'], ['LATENCY', 'HISTORY'], ['LATENCY', 'GRAPH', 'x'], ['LATENCY', 'nosuch'],
    ['SLOWLOG', 'LEN'], ['SLOWLOG', 'GET', '0'], ['SLOWLOG', 'GET', '-2'], ['SLOWLOG', 'GET', '1', '2'], ['SLOWLOG', 'nosuch'],
]] + [
    # Transactions: what runs, what is refused while queued, and EXEC's errors.
    [['MULTI'], ['GET', 'str'], ['INCR', 'str'], ['GET', 'missing'], ['EXEC']],
    [['MULTI'], ['GET'], ['EXEC']],
    [['MULTI'], ['SET', 'k', 'v'], ['DISCARD']],
    [['EXEC']], [['DISCARD']], [['MULTI'], ['MULTI'], ['EXEC']],
    # Several commands on one connection.
    [['GET', 'str'], ['GET', 'missing'], ['HGET', 'hash', 'f'], ['NOSUCH']],
    [['MGET', 'str', 'missing'], ['SET', 'str', 'y', 'GET'], ['EXISTS', 'hash', 'missing'], ['TTL', 'list']],
]


def expired(keys, *commands):
    """A case that gives keys a TTL of a millisecond, lets it pass, then
    sends commands."""
    return [*(['PEXPIRE', key, '1'] for key in keys), ['SLEEP', '5'], *commands]


# Each is a case on the servers whose expiry cycle is off: which commands
# remove a key whose TTL has passed, and count it in expired_keys, as Redis's
# lookups do (db.c, expireIfNeeded), and which only report it as gone (KEYS,
# DBSIZE). SET with an instant already past stores nothing and counts the key
# as expired; EXPIRE and GETEX with one delete the key and count nothing
# (t_string.c, expire.c).
EXPIRY_CASES = [expired(keys, *commands) for keys, *commands in [
    (['str'], ['GET', 'str']), (['str'], ['EXISTS', 'str']), (['str'], ['TYPE', 'str']), (['str'], ['TTL', 'str']),
    (['str'], ['PTTL', 'str']), (['str'], ['STRLEN', 'str']), (['str'], ['GETRANGE', 'str', '0', '1']),
    (['str'], ['APPEND', 'str', 'x']), (['str'], ['SET', 'str', 'x']), (['str'], ['SET', 'str', 'x', 'NX']),
    (['str'], ['SET', 'str', 'x', 'XX']), (['str'], ['SETNX', 'str', 'x']), (['num'], ['INCR', 'num']),
    (['str'], ['GETDEL', 'str']), (['str'], ['GETEX', 'str']), (['str'], ['GETSET', 'str', 'x']),
    (['str'], ['DEL', 'str']), (['str'], ['UNLINK', 'str']), (['str'], ['EXPIRE', 'str', '100']),
    (['str'], ['PERSIST', 'str']), (['str'], ['MEMORY', 'USAGE', 'str']), (['str'], ['SADD', 'str', 'a']),
    (['str', 'num'], ['MGET', 'str', 'num', 'other']), (['str', 'num'], ['EXISTS', 'str', 'num']),
    (['str', 'num'], ['DEL', 'str', 'num', 'other']), (['str'], ['MSET', 'str', '1', 'n2', '2']),
    (['str'], ['MSETNX', 'str', '1', 'n2', '2']), (['str'], ['LCS', 'str', 'other']),
    (['str'], ['DBSIZE']), (['str'], ['KEYS', '*']), (['str'], ['SCAN', '0']), (['str'], ['SCAN', '0', 'TYPE', 'string']),
    (['hash'], ['HGET', 'hash', 'f']), (['hash'], ['HLEN', 'hash']), (['hash'], ['HSET', 'hash', 'g', '1']),
    (['hash'], ['HGETALL', 'hash']), (['hash'], ['HEXISTS', 'hash', 'f']), (['hash'], ['HDEL', 'hash', 'f']),
    (['hash'], ['SET', 'hash', 'x']), (['hash'], ['TYPE', 'hash']),
    (['list'], ['LLEN', 'list']), (['list'], ['LRANGE', 'list', '0', '-1']), (['list'], ['LPUSH', 'list', 'x']),
    (['list'], ['LPOP', 'list']), (['list'], ['LINDEX', 'list', '0']),
    (['set'], ['SCARD', 'set']), (['set'], ['SISMEMBER', 'set', 'a']), (['set'], ['SADD', 'set', 'z']),
    (['set'], ['SMEMBERS', 'set']), (['set'], ['SPOP', 'set']),
    (['zset'], ['ZCARD', 'zset']), (['zset'], ['ZSCORE', 'zset', 'a']), (['zset'], ['ZADD', 'zset', '3', 'c']),
    (['zset'], ['ZRANGE', 'zset', '0', '-1']), (['zset'], ['ZINCRBY', 'zset', '1', 'a']),
    (['hll'], ['PFCOUNT', 'hll']), (['hll'], ['PFADD', 'hll', 'z']), (['hll'], ['PFMERGE', 'dest', 'hll']),
    (['geo'], ['GEOPOS', 'geo', 'palermo']), (['geo'], ['GEOADD', 'geo', '1', '1', 'x']),
    (['str'], ['MULTI'], ['GET', 'str'], ['EXEC']),
    (['str'], ['DIGEST', 'str']), (['str'], ['DELEX', 'str']), (['str'], ['DELEX', 'str', 'IFEQ', 'abc']),
    (['str'], ['SET', 'str', 'x', 'IFEQ', 'abc']), (['num'], ['INCREX', 'num', 'BYINT', '1']),
    (['str'], ['MSETEX', '1', 'str', 'x']),
]] + [
    [['SET', 'absent', 'v', 'PXAT', '1']], [['SET', 'str', 'v', 'PXAT', '1']], [['SET', 'str', 'v', 'EXAT', '1', 'GET']],
    [['SET', 'hash', 'v', 'PXAT', '1']], [['SET', 'str', 'v', 'NX', 'PXAT', '1']],
    [['SET', 'absent', 'v', 'XX', 'PXAT', '1']], [['SET', 'absent', 'v', 'NX', 'PXAT', '1']],
    [['PEXPIREAT', 'str', '1']], [['EXPIREAT', 'hash', '1']], [['GETEX', 'str', 'PXAT', '1']],
    [['SET', 'k', 'v', 'PXAT', '1'], ['GET', 'k']],
    # Redis 8's string writes with an instant already past: SET's conditions
    # and INCREX delete the key and count it as expired, as SET does; MSETEX
    # stores its keys with the passed expiry, which their next lookup counts.
    [['SET', 'str', 'x', 'IFEQ', 'abc', 'PXAT', '1']], [['SET', 'str', 'x', 'IFEQ', 'no', 'PXAT', '1']],
    [['INCREX', 'num', 'BYINT', '1', 'PXAT', '1']], [['INCREX', 'absent', 'BYINT', '1', 'PXAT', '1']],
    [['INCREX', 'num', 'BYINT', '1', 'PXAT', '1', 'ENX']],
    [['MSETEX', '1', 'k', 'v', 'PXAT', '1']], [['MSETEX', '2', 'k', 'v', 'str', 'w', 'PXAT', '1'], ['GET', 'k'], ['EXISTS', 'str']],
]
MODULE_CASES = [[c] for c in [
    ['BF.ADD', 'bf', 'b'], ['BF.EXISTS', 'bf', 'a'], ['BF.EXISTS', 'missing', 'a'], ['BF.MADD', 'bf', 'c', 'd'],
    ['BF.MEXISTS', 'bf', 'a', 'z'], ['BF.INFO', 'bf'], ['CF.ADD', 'cf', 'b'], ['CF.ADDNX', 'cf', 'a'],
    ['CF.EXISTS', 'cf', 'a'], ['CF.EXISTS', 'missing', 'a'], ['CF.MEXISTS', 'cf', 'a', 'z'], ['CF.DEL', 'cf', 'a'],
    ['CF.COUNT', 'cf', 'a'], ['CF.INFO', 'cf'], ['CMS.INCRBY', 'cms', 'a', '1'], ['CMS.QUERY', 'cms', 'a'],
    ['CMS.QUERY', 'missing', 'a'],
]]


# The marker counters records for INFO's Commandstats section.
COMMANDSTATS = '_commandstats'


def counters(info):
    """INFO's counters, from a bulk or verbatim reply."""
    text = info.decode()
    fields = {}
    for line in text.split('\r\n'):
        if line == '# Commandstats':
            # The section is there, lines or not: a server that has it and
            # leaves out a command Redis counts is a mismatch, not a field
            # it does not report.
            fields[COMMANDSTATS] = 1
            continue
        name, sep, value = line.partition(':')
        if not sep:
            continue
        if name in FIELDS:
            fields[name] = int(value)
        elif name.startswith('errorstat_'):
            fields[name] = int(value.removeprefix('count='))
        elif name.startswith('cmdstat_'):
            stats = dict(part.split('=', 1) for part in value.split(','))
            for counter in ('calls', 'rejected_calls', 'failed_calls'):
                fields[f'{name}.{counter}'] = int(stats[counter])
    return fields


def run_case(port, base, case):
    session = parity.Session(port, auth=False, protocol=2)
    try:
        session.call('FLUSHDB')
        for command in base:
            session.call(*command)
        session.call('CONFIG', 'RESETSTAT')
        for command in case:
            if command[0] == 'SLEEP':
                time.sleep(int(command[1]) / 1000)
            else:
                session.call(*command)
        return counters(session.call('INFO', 'stats', 'errorstats', 'commandstats'))
    finally:
        session.close()


def compare(keel, redis):
    """The fields that differ, and those Keel does not report at all."""
    mismatched, not_reported = {}, []
    keel_cmdstats = COMMANDSTATS in keel
    for name in sorted(set(keel) | set(redis)):
        if name == COMMANDSTATS:
            continue
        if name in FIELDS and name not in keel:
            not_reported.append(name)
        elif name.startswith('cmdstat_') and not keel_cmdstats:
            not_reported.append('commandstats')
        elif keel.get(name) != redis.get(name):
            mismatched[name] = {'keel': keel.get(name), 'redis': redis.get(name)}
    return mismatched, not_reported


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--bin', type=Path, required=True, help='the Keel binary')
    parser.add_argument('--redis', type=Path, required=True, help='redis-server')
    parser.add_argument('--redis-module', type=Path, help='RedisBloom, which adds the BF, CF and CMS cases')
    parser.add_argument('--out', type=Path, required=True)
    args = parser.parse_args(argv)
    args.out.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(dir=args.out) as scratch:
        servers = parity.Servers(args, Path(scratch))
        try:
            keel = servers.start_keel('keel', password=False)
            redis = servers.start_redis('redis', password=False)
            lazy_keel = servers.start_keel('keel-lazy', '-active-expire-samples', '0', password=False)
            lazy_redis = servers.start_redis('redis-lazy', 'enable-debug-command local', password=False)
            session = parity.Session(lazy_redis, auth=False, protocol=2)
            try:
                session.call('DEBUG', 'SET-ACTIVE-EXPIRE', '0')
            finally:
                session.close()
            base, cases = BASE, CASES
            if args.redis_module:
                base, cases = BASE + MODULE_BASE, CASES + MODULE_CASES
            results = []
            for pair, pair_cases in (((keel, redis), cases), ((lazy_keel, lazy_redis), EXPIRY_CASES)):
                for case in pair_cases:
                    got = {'keel': run_case(pair[0], base, case), 'redis': run_case(pair[1], base, case)}
                    mismatched, not_reported = compare(got['keel'], got['redis'])
                    results.append({'commands': case, **got, 'mismatched': mismatched, 'not_reported': not_reported})
        finally:
            servers.stop()
    failed = [r for r in results if r['mismatched']]
    not_reported = sorted({name for r in results for name in r['not_reported']})
    for r in results:
        r['not_reported'] = sorted(set(r['not_reported']))
    report = {'cases': len(results), 'mismatched_cases': len(failed), 'not_reported': not_reported,
              'redis_version': parity.subprocess.run([str(args.redis), '--version'], capture_output=True,
                                                     text=True).stdout.strip()}
    (args.out / 'cases.json').write_text(json.dumps(results, indent=1) + '\n')
    (args.out / 'report.json').write_text(json.dumps(report, indent=1) + '\n')
    for r in failed:
        print(' / '.join(' '.join(c) for c in r['commands']), json.dumps(r['mismatched']))
    print(json.dumps(report))
    return 1 if failed else 0


if __name__ == '__main__':
    sys.exit(main())
