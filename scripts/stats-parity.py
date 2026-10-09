#!/usr/bin/env python3
"""Compare how Keel's INFO counters move with Redis's, command by command.

Each case runs on both servers the same way, on one connection: FLUSHDB, the
same base keys, CONFIG RESETSTAT, the case's commands, then INFO stats and
errorstats. What the case moved is compared field by field:
total_commands_processed, total_error_replies, keyspace_hits,
keyspace_misses, every errorstat_ line, and each cmdstat_ line's calls,
rejected_calls and failed_calls (docs/info-compatibility.md, part c). The
time a command took is not compared, being the time it took; replies are
not compared either: scripts/error-parity.py does that.

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

HERE = Path(__file__).resolve().parent
_spec = importlib.util.spec_from_file_location('error_parity', HERE / 'error-parity.py')
parity = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(parity)

FIELDS = ('total_commands_processed', 'total_error_replies', 'keyspace_hits', 'keyspace_misses')

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
    ['LCS', 'str', 'missing'],
    # The keyspace.
    ['DEL', 'str', 'missing'], ['UNLINK', 'str'], ['EXISTS', 'str', 'missing', 'str'], ['TYPE', 'str'],
    ['TYPE', 'missing'], ['KEYS', '*'], ['SCAN', '0'], ['TTL', 'str'], ['TTL', 'missing'], ['PTTL', 'str'],
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
MODULE_CASES = [[c] for c in [
    ['BF.ADD', 'bf', 'b'], ['BF.EXISTS', 'bf', 'a'], ['BF.EXISTS', 'missing', 'a'], ['BF.MADD', 'bf', 'c', 'd'],
    ['BF.MEXISTS', 'bf', 'a', 'z'], ['BF.INFO', 'bf'], ['CF.ADD', 'cf', 'b'], ['CF.ADDNX', 'cf', 'a'],
    ['CF.EXISTS', 'cf', 'a'], ['CF.EXISTS', 'missing', 'a'], ['CF.MEXISTS', 'cf', 'a', 'z'], ['CF.DEL', 'cf', 'a'],
    ['CF.COUNT', 'cf', 'a'], ['CF.INFO', 'cf'], ['CMS.INCRBY', 'cms', 'a', '1'], ['CMS.QUERY', 'cms', 'a'],
    ['CMS.QUERY', 'missing', 'a'],
]]


def counters(info):
    """INFO's counters, from a bulk or verbatim reply."""
    text = info.decode()
    fields = {}
    for line in text.split('\r\n'):
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
            session.call(*command)
        return counters(session.call('INFO', 'stats', 'errorstats', 'commandstats'))
    finally:
        session.close()


def compare(keel, redis):
    """The fields that differ, and those Keel does not report at all."""
    mismatched, not_reported = {}, []
    keel_cmdstats = any(name.startswith('cmdstat_') for name in keel)
    for name in sorted(set(keel) | set(redis)):
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
            base, cases = BASE, CASES
            if args.redis_module:
                base, cases = BASE + MODULE_BASE, CASES + MODULE_CASES
            results = []
            for case in cases:
                got = {'keel': run_case(keel, base, case), 'redis': run_case(redis, base, case)}
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
