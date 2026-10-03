#!/usr/bin/env python3
"""Compare Keel's error replies with a reference Redis, byte for byte.

Every case is a short session on a fresh connection to a server of its kind -
a primary with a password, one without, or a read-only replica - and every
reply of the session is compared as the bytes on the wire, errors included,
over RESP2 and, for sessions that log in, over RESP3 as well. Nothing is
normalized: an error is the same only if its every byte is.

Commands Redis does not have (MORRIS.*, KEEL.*, MEMKV.*, SRAND) cannot be
compared against it; their cases carry the reply Redis's own format gives for
the same refusal, and Keel is compared against that. BF, CF and CMS cases run
only when --redis-module names RedisBloom. A reply whose content legitimately
differs (a HELLO map, which names each server) is compared by its first byte.

A few cases differ because Keel does not have what Redis has - a command, a
subcommand, an option, a second database. They are listed in KNOWN with the
reason, still run, and reported as known differences with both replies; any
other difference is a failure. The report lists every case with both
servers' replies; the exit status is non-zero if any reply differs that is not
a known difference.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import sys
import time

PASSWORD = secrets.token_hex(16)


def sha256(path):
    digest = hashlib.sha256()
    with Path(path).open('rb') as source:
        while chunk := source.read(1 << 20):
            digest.update(chunk)
    return digest.hexdigest()


def encode(parts):
    parts = [p if isinstance(p, bytes) else str(p).encode() for p in parts]
    return b'*%d\r\n' % len(parts) + b''.join(b'$%d\r\n' % len(p) + p + b'\r\n' for p in parts)


def read_raw(stream, depth=0):
    """One whole reply, as the bytes that carried it."""
    if depth > 32:
        raise ValueError('RESP nesting limit')
    line = stream.readline(1 << 20)
    if not line.endswith(b'\r\n'):
        raise ValueError(f'invalid RESP header {line!r}')
    kind, body = line[:1], line[1:-2]
    if kind in b'-+:_,#(':
        return line
    n = int(body)
    if kind in b'$=!':
        if n < 0:
            return line
        payload = stream.read(n + 2)
        if len(payload) != n + 2:
            raise ValueError('short bulk reply')
        return line + payload
    if kind in b'*~>%|':
        if n < 0:
            return line
        out = line
        for _ in range(n * (2 if kind in b'%|' else 1)):
            out += read_raw(stream, depth + 1)
        if kind == b'|':
            out += read_raw(stream, depth + 1)
        return out
    raise ValueError(f'unknown RESP type {line!r}')


class Session:
    def __init__(self, port, *, auth, protocol):
        self.socket = socket.create_connection(('127.0.0.1', port), timeout=5)
        self.stream = self.socket.makefile('rb')
        if protocol == 3:
            hello = self.call('HELLO', 3, *(('AUTH', 'default', PASSWORD) if auth else ()))
            if not hello.startswith(b'%'):
                raise RuntimeError(f'HELLO 3 answered {hello!r}')
        elif auth:
            if self.call('AUTH', PASSWORD) != b'+OK\r\n':
                raise RuntimeError('AUTH failed')

    def call(self, *parts):
        self.socket.sendall(encode(parts))
        return read_raw(self.stream)

    def close(self):
        self.stream.close()
        self.socket.close()


def free_port():
    with socket.socket() as reservation:
        reservation.bind(('127.0.0.1', 0))
        return reservation.getsockname()[1]


def wait_ready(process, port, password, log):
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError(f'server exited during startup; see {log}')
        try:
            session = Session(port, auth=password is not None, protocol=2)
            try:
                if session.call('PING') == b'+PONG\r\n':
                    return
            finally:
                session.close()
        except (OSError, RuntimeError, ValueError):
            pass
        time.sleep(.05)
    raise TimeoutError(f'server on {port} not ready; see {log}')


class Servers:
    """The six servers the cases run against, started once and stopped at exit."""

    def __init__(self, args, root):
        self.args, self.root, self.processes, self.ports = args, root, [], {}
        self.env = {key: os.environ[key] for key in ('PATH', 'HOME', 'TMPDIR') if key in os.environ}

    def start_keel(self, name, *extra, password=True):
        directory = self.root / name
        directory.mkdir()
        port = free_port()
        command = [str(self.args.bin.resolve()), '-host', '127.0.0.1', '-port', str(port), *extra]
        env = dict(self.env)
        if password:
            command += ['-requirepass-env', 'KEEL_ERROR_PARITY_PASSWORD']
            env['KEEL_ERROR_PARITY_PASSWORD'] = PASSWORD
        log = (directory / 'server.log').open('wb')
        process = subprocess.Popen(command, cwd=directory, env=env, stdout=log, stderr=log)
        self.processes.append((process, log))
        wait_ready(process, port, PASSWORD if password else None, directory / 'server.log')
        self.ports['keel', name] = port
        return port

    def start_redis(self, name, *config, password=True):
        directory = self.root / name
        directory.mkdir()
        port = free_port()
        log = (directory / 'server.log').open('wb')
        # Debian's redis-server may be a symlink to a multicall executable;
        # preserve argv[0], which selects server rather than RDB-check mode.
        process = subprocess.Popen([str(self.args.redis.absolute()), '-'], cwd=directory, env=self.env,
                                   stdin=subprocess.PIPE, stdout=log, stderr=log)
        lines = ['bind 127.0.0.1', f'port {port}', 'save ""', 'appendonly no', f'dir {directory}', *config]
        if password:
            lines.append(f'requirepass {PASSWORD}')
        if self.args.redis_module:
            lines.append(f'loadmodule {self.args.redis_module.absolute()}')
        process.stdin.write(('\n'.join(lines) + '\n').encode())
        process.stdin.close()
        self.processes.append((process, log))
        wait_ready(process, port, PASSWORD if password else None, directory / 'server.log')
        self.ports['redis', name] = port
        return port

    def start(self):
        primary = self.start_keel('keel-primary', '-replication-feed', '-appendonly',
                                  '-appendfilename', str(self.root / 'keel-primary' / 'store.aof'))
        self.start_keel('keel-nopass', password=False)
        self.start_keel('keel-replica', '-replicaof', f'127.0.0.1:{primary}', '-appendonly',
                        '-appendfilename', str(self.root / 'keel-replica' / 'store.aof'),
                        '-primary-password-env', 'KEEL_ERROR_PARITY_PASSWORD')
        reference = self.start_redis('redis-primary')
        self.start_redis('redis-nopass', password=False)
        self.start_redis('redis-replica', f'replicaof 127.0.0.1 {reference}', f'masterauth {PASSWORD}')
        # A replica answers reads once it has its primary's state; wait for both.
        for side in ('keel', 'redis'):
            deadline = time.monotonic() + 15
            while True:
                session = Session(self.ports[side, f'{side}-replica'], auth=True, protocol=2)
                try:
                    if session.call('GET', 'replica:ready').startswith(b'$'):
                        break
                finally:
                    session.close()
                if time.monotonic() > deadline:
                    raise TimeoutError(f'{side} replica never caught up')
                time.sleep(.1)

    def stop(self):
        for process, log in self.processes:
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=8)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
            log.close()


# Differences that stay, each with the reason. The key is the command that
# differs, as sent, its words joined by spaces.
KNOWN = {
    'watch k': 'WATCH is not implemented and stays an unknown command, so an optimistic-locking API fails '
               'loudly instead of quietly not watching (docs/transactions.md); before AUTH that answers '
               'unknown command rather than NOAUTH, as Redis answers for any command it does not have',
    'object encoding k': 'OBJECT is not implemented; Keel answers it as Redis answers a command it does not have',
    'config get x': 'CONFIG is not implemented; Keel answers it as Redis answers a command it does not have',
    'client kill x y': 'CLIENT KILL is not implemented; Keel answers it as Redis answers a subcommand it does not have',
    'SELECT 1': 'Keel has one database, and SELECT of any other answers the error for one that does not exist',
    'ZADD zset GT LT 1 m': 'ZADD GT and LT are not implemented, so GT is read as the score, as a Redis without them reads it',
    'ZADD zset NX GT 1 m': 'ZADD GT is not implemented, so GT is read as the score and the pairs do not pair up',
    'ZADD zset INCR 1 a 2 b': 'ZADD INCR is not implemented, so INCR is read as the score and the pairs do not pair up',
    'ZRANGE zset 0 1 BYLEX': 'ZRANGE BYLEX is not implemented and is refused as any option Keel does not know',
    'nosuch ' + ' '.join(['ab'] * 60): 'Redis refuses a request of more than ten arguments from a connection that has '
               'not logged in, as a protocol error that closes it; Keel bounds requests with its own admission '
               'limits and has not adopted those',
}


def arity_error(name):
    return b"-ERR wrong number of arguments for '%s' command\r\n" % name.lower().encode()


LONG = 'n' * 200
UNKNOWN = [
    ['nosuchcommand'], ['NoSuchCommand', 'a', 'b'], ['nosuch', 'a', 'b', 'c'], [LONG], ['x' * 128], ['x' * 129],
    ['nosuch', 'a' * 200], ['nosuch', 'a' * 125, 'b'], ['nosuch', 'a' * 124, 'b', 'c'], ['nosuch', 'a' * 123, 'b', 'c'],
    ['nosuch', *['ab'] * 60], ['nosuch', b'a\x00b', 'c'], [b'n\x00osuch', 'x'], ['nosuch', 'a\r\nb', 'c\nd'],
    [b'no\r\nsuch', 'x'], ['nosuch', ''], ['nosuch', '', 'x'], ['', 'a'], ['nosuch', 'é' * 70], ['é' * 70],
    [b'\xff\xfe', b'\xc3'], ['watch', 'k'], ['object', 'encoding', 'k'], ['config', 'get', 'x'],
]

# Generic errors, each with the setup that produces it. Keys are typed by name.
SETUP = [['SET', 'str', 'abc'], ['SET', 'num', '10'], ['SET', 'max', '9223372036854775807'],
         ['SET', 'min', '-9223372036854775808'], ['RPUSH', 'list', 'a', 'b'], ['HSET', 'hash', 'f', 'v', 'n', '5', 'big', '9223372036854775807'],
         ['SADD', 'set', 'a', 'b'], ['ZADD', 'zset', '1', 'a', '2', 'b'], ['ZADD', 'zinf', 'inf', 'a'],
         ['GEOADD', 'geo', '13.361389', '38.115556', 'Palermo', '15.087269', '37.502669', 'Catania']]

GENERIC = [
    # Strings
    ['SET', 'k', 'v', 'EX', 'x'], ['SET', 'k', 'v', 'EX', '0'], ['SET', 'k', 'v', 'EX', '-1'], ['SET', 'k', 'v', 'PX', '0'],
    ['SET', 'k', 'v', 'EXAT', '0'], ['SET', 'k', 'v', 'PXAT', '-5'], ['SET', 'k', 'v', 'EX', '9223372036854775807'],
    ['SET', 'k', 'v', 'EX', '+5'], ['SET', 'k', 'v', 'EX', '05'], ['SET', 'k', 'v', 'EX', '10', 'EX', '20'],
    ['SET', 'k', 'v', 'EX', 'x', 'NX', 'XX'], ['SET', 'k', 'v', 'NX', 'NX'], ['SET', 'k', 'v', 'EX'], ['SET', 'k', 'v', 'BAD'],
    ['SET', 'k', 'v', 'NX', 'XX'], ['SET', 'k', 'v', 'EX', '10', 'PX', '10'], ['SET', 'k', 'v', 'KEEPTTL', 'EX', '10'],
    ['SET', 'list', 'v', 'GET'], ['SET', 'k', 'v', 'ex', '10', 'get', 'nx'], ['GET', 'list'], ['SETNX', 'list', 'v'],
    ['INCR', 'str'], ['INCR', 'max'], ['DECR', 'min'], ['INCRBY', 'num', 'x'], ['INCRBY', 'num', '1.5'],
    ['INCRBY', 'max', '1'], ['DECRBY', 'num', '-9223372036854775808'], ['DECRBY', 'min', '1'], ['INCR', 'list'],
    ['INCRBY', 'list', 'x'], ['INCRBY', 'list', '1'], ['DECRBY', 'hash', 'x'], ['INCRBY', 'num', '+1'],
    ['DECRBY', 'list', '-9223372036854775808'], ['INCR', 'num', 'x'],
    ['MSET', 'a', '1', 'b'], ['MGET', 'list', 'str'], ['SETEX', 'k', 'x', 'v'], ['SETEX', 'k', '0', 'v'],
    ['SETEX', 'k', '-1', 'v'], ['PSETEX', 'k', '0', 'v'], ['PSETEX', 'k', 'x', 'v'], ['SETEX', 'k', '9223372036854775807', 'v'],
    ['LCS', 'str', 'num', 'BAD'], ['LCS', 'str', 'num', 'LEN', 'IDX'], ['LCS', 'str', 'num', 'MINMATCHLEN', 'x'],
    ['LCS', 'str', 'num', 'MINMATCHLEN'], ['LCS', 'list', 'str'], ['LCS', 'str', 'list'],
    # Keys and expiry
    ['EXPIRE', 'str', 'x'], ['EXPIRE', 'str', '10', 'BAD'], ['EXPIRE', 'str', '10', 'NX', 'XX'], ['EXPIRE', 'str', '10', 'GT', 'LT'],
    ['EXPIRE', 'str', '10', 'NX', 'GT'], ['EXPIRE', 'str', '9223372036854775807'], ['PEXPIRE', 'str', '9223372036854775807'],
    ['EXPIREAT', 'str', '9223372036854775807'], ['PEXPIREAT', 'str', 'x'], ['EXPIRE', 'str', '10', 'xx', 'nx'],
    ['SCAN', 'x'], ['SCAN', '-1'], ['SCAN', '0', 'COUNT', '0'], ['SCAN', '0', 'COUNT', 'x'], ['SCAN', '0', 'BAD'],
    ['SCAN', '0', 'MATCH'], ['SCAN', '0', 'TYPE'], ['SCAN', '18446744073709551616'],
    ['SELECT', '1'], ['SELECT', 'x'], ['SELECT', '-1'], ['SELECT', '00'], ['FLUSHDB', 'BAD'], ['FLUSHDB', 'SYNC', 'ASYNC'],
    ['TYPE', 'nokey'],
    # Hashes
    ['HSET', 'hash', 'f'], ['HSET', 'hash', 'f', 'v', 'g'], ['HSET', 'str', 'f', 'v', 'g'], ['HGET', 'list', 'f'],
    ['HINCRBY', 'hash', 'f', '1'],
    ['HINCRBY', 'hash', 'n', 'x'], ['HINCRBY', 'hash', 'big', '1'], ['HINCRBY', 'list', 'f', 'x'], ['HGETALL', 'str'],
    ['HMGET', 'str', 'f'], ['HSETNX', 'str', 'f', 'v'],
    # Lists
    ['LPUSH', 'str', 'a'], ['LPOP', 'list', 'x'], ['LPOP', 'list', '-1'], ['RPOP', 'list', '0'], ['LPOP', 'list', '1', '2'],
    ['LINDEX', 'list', 'x'], ['LSET', 'nokey', '0', 'v'], ['LSET', 'list', '10', 'v'], ['LSET', 'list', 'x', 'v'],
    ['LRANGE', 'list', 'a', '1'], ['LRANGE', 'list', '0', 'b'], ['LTRIM', 'list', 'a', '1'], ['LLEN', 'str'],
    ['LINDEX', 'str', '0'], ['LSET', 'str', '0', 'v'], ['LINDEX', 'nokey', 'x'], ['LSET', 'nokey', 'x', 'v'],
    ['LINDEX', 'str', 'x'], ['LSET', 'str', 'x', 'v'], ['LPOP', 'str', 'x'], ['LRANGE', 'str', 'a', 'b'],
    ['LTRIM', 'str', 'a', 'b'], ['LRANGE', 'list', '+0', '1'], ['LPOP', 'list', '-0'], ['LPOP', 'set', '1', '2'],
    ['RPOP', 'str', '1', '2'],
    # Sets
    ['SADD', 'str', 'a'], ['SPOP', 'set', 'x'], ['SPOP', 'set', '-1'], ['SPOP', 'set', '1', '2'], ['SRANDMEMBER', 'set', 'x'],
    ['SRANDMEMBER', 'set', '-9223372036854775808'], ['SRANDMEMBER', 'set', '1', '2'], ['SISMEMBER', 'str', 'a'],
    ['SMISMEMBER', 'str', 'a'], ['SCARD', 'list'], ['SMEMBERS', 'str'], ['SREM', 'str', 'a'], ['SPOP', 'str', 'x'],
    ['SPOP', 'str', '1', '2'], ['SRANDMEMBER', 'str', 'x'], ['SRANDMEMBER', 'str', '1', '2'],
    # Sorted sets
    ['ZADD', 'zset', 'x', 'm'], ['ZADD', 'zset', 'nan', 'm'], ['ZADD', 'zset', 'NX', 'XX', '1', 'm'],
    ['ZADD', 'zset', 'GT', 'LT', '1', 'm'], ['ZADD', 'zset', 'NX', 'GT', '1', 'm'], ['ZADD', 'zset', '1'],
    ['ZADD', 'zset', '1', 'm', '2'], ['ZADD', 'zset', 'INCR', '1', 'a', '2', 'b'], ['ZADD', 'str', '1', 'm'],
    ['ZADD', 'zset', 'CH'], ['ZINCRBY', 'zset', 'x', 'a'], ['ZINCRBY', 'zinf', '-inf', 'a'], ['ZINCRBY', 'str', '1', 'a'],
    ['ZRANGE', 'zset', 'a', '1'], ['ZRANGE', 'zset', '0', '1', 'BAD'], ['ZRANGE', 'zset', '0', '1', 'LIMIT', '0', '1'],
    ['ZRANGE', 'zset', 'x', 'y', 'BYSCORE'], ['ZRANGE', 'zset', '0', '1', 'BYSCORE', 'LIMIT', 'x', '1'],
    ['ZRANGE', 'zset', '0', '1', 'BYSCORE', 'LIMIT', '0'], ['ZRANGE', 'zset', '0', '1', 'BYLEX'], ['ZRANGE', 'zset', '0', '-1', 'REV', 'REV'],
    ['ZRANGEBYSCORE', 'zset', 'a', 'b'], ['ZRANGEBYSCORE', 'zset', '0', '1', 'LIMIT', 'x', '1'],
    ['ZRANGEBYSCORE', 'zset', '0', '1', 'BAD'], ['ZRANGEBYSCORE', 'zset', '0', '1', 'LIMIT', '0'],
    ['ZREVRANGEBYSCORE', 'zset', 'a', 'b'], ['ZCOUNT', 'zset', 'a', 'b'], ['ZCOUNT', 'zset', '(x', '1'],
    ['ZPOPMIN', 'zset', 'x'], ['ZPOPMIN', 'zset', '-1'], ['ZPOPMAX', 'zset', '1', '2'], ['ZRANK', 'zset', 'a', 'BAD'],
    ['ZRANK', 'zset', 'a', 'WITHSCORE', 'x'], ['ZSCORE', 'str', 'a'], ['ZCARD', 'str'], ['ZREM', 'str', 'a'],
    ['ZRANGE', 'str', '0', '1'], ['ZPOPMIN', 'str'], ['ZADD', 'str', 'x', 'm'], ['ZADD', 'str', 'NX', 'XX', '1', 'm'],
    ['ZADD', 'zset', 'NX', 'XX', '1'], ['ZINCRBY', 'str', 'x', 'a'], ['ZRANGE', 'str', 'a', 'b'], ['ZCOUNT', 'str', 'a', 'b'],
    ['ZRANGEBYSCORE', 'str', '0', '1', 'BAD'], ['ZRANGEBYSCORE', 'zset', 'a', 'b', 'BAD'],
    ['ZRANGEBYSCORE', 'zset', 'a', 'b', 'LIMIT', 'x', '1'], ['ZPOPMIN', 'str', 'x'], ['ZRANK', 'str', 'a', 'BAD'],
    ['ZRANK', 'zset', 'a', 'WITHSCORE'], ['ZRANK', 'zset', 'nobody', 'WITHSCORE'], ['ZRANK', 'nokey', 'a', 'withscore'],
    ['ZRANGE', 'zset', '0', '-1', 'BYSCORE'], ['ZRANGE', 'zset', '(1', '+inf', 'BYSCORE', 'LIMIT', '0', '1', 'WITHSCORES'],
    ['ZRANGE', 'zset', '+inf', '-inf', 'BYSCORE', 'REV'], ['ZRANGE', 'zset', '0', '1', 'BYSCORE', 'BYSCORE'],
    ['ZRANGE', 'zset', '0', '1', 'LIMIT', 'x', '1'], ['ZRANGE', 'zset', '0', '-1', 'LIMIT', '5', '-1'],
    ['ZRANGE', 'zset', '0', '-1', 'LIMIT', '0', '1'],
    # Geo
    ['GEOADD', 'geo', 'x', '1', 'm'], ['GEOADD', 'geo', '200', '100', 'm'], ['GEOADD', 'geo', 'NX', 'XX', '1', '1', 'm'],
    ['GEOADD', 'geo', '1', '1', 'm', '2'], ['GEOADD', 'geo', 'CH', '1', '1'], ['GEOADD', 'str', '1', '1', 'm'],
    ['GEODIST', 'geo', 'Palermo', 'Catania', 'parsecs'], ['GEODIST', 'geo', 'Palermo', 'Catania', 'km', 'x'],
    ['GEOHASH', 'str', 'a'], ['GEOPOS', 'str', 'a'], ['GEOADD', 'str', 'x', '1', 'm'], ['GEOADD', 'str', 'NX', 'XX', '1', '1', 'm'],
    ['GEODIST', 'str', 'a', 'b', 'parsecs'], ['GEODIST', 'str', 'a', 'b', 'km', 'x'],
    ['GEOSEARCH', 'geo', 'BYRADIUS', '10', 'km', 'x', 'y'], ['GEOSEARCH', 'geo', 'FROMMEMBER', 'Palermo', 'ASC', 'x', 'y'],
    ['GEOSEARCH', 'geo', 'FROMMEMBER', 'Palermo', 'BYRADIUS', '10', 'km', 'COUNT', '0'],
    ['GEOSEARCH', 'geo', 'FROMMEMBER', 'Palermo', 'BYRADIUS', '10', 'km', 'ANY'],
    ['GEOSEARCH', 'geo', 'FROMMEMBER', 'nobody', 'BYRADIUS', '10', 'km'],
    ['GEOSEARCH', 'geo', 'FROMMEMBER', 'Palermo', 'BYRADIUS', 'x', 'km'],
    ['GEOSEARCH', 'geo', 'FROMMEMBER', 'Palermo', 'BYRADIUS', '10', 'parsecs'],
    ['GEOSEARCH', 'geo', 'FROMMEMBER', 'Palermo', 'BYBOX', 'x', '10', 'km'],
    ['GEOSEARCH', 'geo', 'FROMMEMBER', 'Palermo', 'BYBOX', '10', 'x', 'km'],
    ['GEOSEARCH', 'geo', 'FROMLONLAT', 'x', '1', 'BYRADIUS', '10', 'km'],
    ['GEOSEARCH', 'geo', 'FROMLONLAT', '200', '100', 'BYRADIUS', '10', 'km'],
    ['GEOSEARCH', 'geo', 'FROMMEMBER', 'Palermo', 'FROMLONLAT', '1', '1', 'BYRADIUS', '10', 'km'],
    ['GEOSEARCH', 'geo', 'FROMMEMBER', 'Palermo', 'BYRADIUS', '10', 'km', 'BYBOX', '1', '1', 'km'],
    ['GEOSEARCH', 'geo', 'FROMMEMBER', 'Palermo', 'BYRADIUS', '10', 'km', 'BAD'],
    ['GEOSEARCH', 'geo', 'FROMMEMBER', 'Palermo', 'BYRADIUS', '10', 'km', 'COUNT', 'x'],
    ['GEOSEARCH', 'str', 'FROMLONLAT', '1', '1', 'BYRADIUS', '10', 'km'],
    # HyperLogLog
    ['PFADD', 'list', 'a'], ['PFCOUNT', 'list'], ['PFMERGE', 'list', 'str'], ['PFADD', 'str', 'a'], ['PFCOUNT', 'str'],
    ['PFMERGE', 'dest', 'str'],
    # Server
    ['MEMORY', 'USAGE', 'str', 'BAD'], ['MEMORY', 'USAGE', 'str', 'SAMPLES', 'x'], ['MEMORY', 'USAGE', 'str', 'SAMPLES'],
    ['MEMORY', 'USAGE', 'str', 'SAMPLES', '-1'], ['MEMORY', 'USAGE', 'nokey', 'SAMPLES', '5'],
    ['INFO', 'nosuchsection', 'other'], ['MEMORY', 'USAGE', 'nokey', 'SAMPLES', '0'], ['MEMORY', 'usage', 'nokey', 'samples', '5', 'samples', '1'],
    # Transactions
    ['EXEC'], ['DISCARD'], ['EXEC', 'x'], ['DISCARD', 'x'], ['MULTI', 'x'], ['UNWATCH', 'x'],
    # Connection
    ['CLIENT', 'SETNAME', 'a b'], ['CLIENT', 'SETINFO', 'bad', 'x'], ['CLIENT', 'SETINFO', 'lib-name', 'a b'],
    ['CLIENT', 'SETINFO', 'LIB-VER', 'a\nb'], ['CLIENT', 'SETINFO', 'x' * 200, 'v'], ['CLIENT', 'SETINFO', b'a\x00b', 'v'],
    ['HELLO', 'x'], ['HELLO', '4'], ['HELLO', '1'], ['HELLO', '3', 'BAD'], ['HELLO', '3', b'B\x00AD'],
    ['HELLO', '3', 'AUTH', 'default'], ['HELLO', '3', 'SETNAME'], ['HELLO', '3', 'SETNAME', 'a b'],
    ['HELLO', '3', 'AUTH', 'default', 'wrong'], ['HELLO', '3', 'AUTH', 'other', 'wrong'],
    ['AUTH', 'wrong'], ['AUTH', 'default', 'wrong'], ['AUTH', 'other', 'wrong'], ['AUTH', 'a', 'b', 'c'],
    ['QUIT', 'x'],
]

SUBCOMMANDS = [
    ['client', 'nosuch'], ['CLIENT', 'nosuch', 'x'], ['client'], ['client', 'id', 'x'], ['CLIENT', 'GETNAME', 'x'],
    ['client', 'setname'], ['client', 'SetInfo', 'lib-name'], ['client', 'info', 'x'], ['client', 'x' * 200],
    ['client', b'a\x00b'], ['client', 'a\r\nb'], ['client', 'é' * 70], ['client', 'kill', 'x', 'y'],
    ['memory', 'nosuch'], ['MEMORY', 'Nosuch', 'x'], ['memory'], ['memory', 'usage'], ['Memory', 'UsAge'],
    ['memory', 'stats', 'x'], ['MEMORY', 'x' * 200],
]

# Commands whose arity Redis does not hold, worded as it words every other.
KEEL_ONLY_ARITY = {'MORRIS.INITBYDIM': 4, 'MORRIS.INITBYPROB': 4, 'MORRIS.INCRBY': -4, 'MORRIS.QUERY': -3,
                   'MORRIS.INFO': 2, 'KEEL.DUMP': 2, 'KEEL.RESTORE': 3, 'MEMKV.DUMP': 2, 'MEMKV.RESTORE': 3,
                   'SRAND': -2, 'KEEL.PROMOTE': 2, 'KEEL.FENCE': 2, 'KEEL.REPL.PULL': 3, 'KEEL.REPL.PULL2': -5}

MODULE_ARITY = {'BF.RESERVE': -4, 'BF.ADD': 3, 'BF.MADD': -3, 'BF.EXISTS': 3, 'BF.MEXISTS': -3, 'BF.INFO': -2,
                'CF.RESERVE': -3, 'CF.ADD': 3, 'CF.ADDNX': 3, 'CF.EXISTS': 3, 'CF.MEXISTS': -3, 'CF.DEL': 3,
                'CF.COUNT': 3, 'CF.INFO': 2, 'CMS.INITBYDIM': 4, 'CMS.INITBYPROB': 4, 'CMS.INCRBY': -4, 'CMS.QUERY': -3}
# Counts RedisBloom checks in the command itself, past the arity Redis checks
# for it: an upper bound, or an argument count that has to be odd or even.
MODULE_HANDLER_ARITY = [['BF.RESERVE', 'bf', '0.01', '100', 'EXPANSION', '2', 'NONSCALING', 'x'],
                        ['BF.INFO', 'bf', 'CAPACITY', 'x'], ['CF.RESERVE', 'cf', '100', 'BUCKETSIZE'],
                        ['CMS.INCRBY', 'cms', 'a', '1', 'b']]
MODULE_SETUP = [['BF.RESERVE', 'bf', '0.01', '100'], ['CF.RESERVE', 'cf', '100'], ['CMS.INITBYDIM', 'cms', '10', '5']]


def arity_cases(arities):
    """For each command, the counts on either side of its arity: one argument
    short of a fixed or minimum count, and one over a fixed one."""
    cases = []
    for name, arity in sorted(arities.items()):
        counts = [abs(arity) - 1] if arity < 0 else [arity - 1, arity + 1]
        for count in counts:
            if count >= 1:
                cases.append([name.lower() if count % 2 else name, *['1'] * (count - 1)])
    return cases


def case(name, commands, *, server='primary', auth=True, protocols=(2, 3), expect=None, loose=(), setup=()):
    return dict(name=name, commands=[list(c) for c in commands], server=server, auth=auth,
                protocols=list(protocols), expect=expect, loose=list(loose), setup=[list(c) for c in setup])


def build_cases(redis_arities, modules):
    cases = []
    for command in UNKNOWN:
        cases.append(case('unknown command', [command]))
        cases.append(case('unknown command before AUTH', [command], auth=False, protocols=(2,)))
        cases.append(case('unknown command queued', [['MULTI'], command, ['EXEC']]))
    for command in arity_cases(redis_arities):
        cases.append(case('arity', [command]))
    for command in arity_cases({k: v for k, v in redis_arities.items() if k not in ('MULTI', 'EXEC', 'DISCARD')})[::3]:
        cases.append(case('arity queued', [['MULTI'], command, ['EXEC']]))
    for command in arity_cases({k: v for k, v in redis_arities.items() if k != 'AUTH'})[::4]:
        cases.append(case('arity before AUTH', [command], auth=False, protocols=(2,)))
    for command in arity_cases(KEEL_ONLY_ARITY):
        cases.append(case('arity, Keel only', [command], expect=[arity_error(command[0])]))
    for command in SUBCOMMANDS:
        cases.append(case('subcommand', [command]))
        cases.append(case('subcommand before AUTH', [command], auth=False, protocols=(2,)))
        cases.append(case('subcommand queued', [['MULTI'], command, ['EXEC']]))
    for command in GENERIC:
        cases.append(case('generic', [command], setup=SETUP))
    # A failed AUTH inside EXEC leaves no element in Redis 8.10.1's reply, whose
    # header still counts one, so a client reading it waits forever; AUTH is
    # not queued here.
    for command in [c for c in GENERIC if 'AUTH' not in c][::5]:
        cases.append(case('generic queued', [['MULTI'], command, ['PING'], ['EXEC']], setup=SETUP))
    # Before AUTH: what Redis answers ahead of NOAUTH, and NOAUTH itself.
    for command in (['GET', 'k'], ['PING'], ['MULTI'], ['EXEC'], ['DISCARD'], ['EXEC', 'x'], ['client', 'id'],
                    ['CLIENT', 'SETNAME', 'x'], ['MEMORY', 'USAGE', 'k'], ['HELLO'], ['HELLO', '3'], ['HELLO', '4'],
                    ['HELLO', 'x'], ['HELLO', '3', 'AUTH', 'default', 'wrong'], ['AUTH'], ['AUTH', 'wrong'],
                    ['AUTH', 'other', 'wrong'], ['AUTH', 'a', 'b', 'c'], ['QUIT', 'x'], ['watch', 'k'], ['unwatch']):
        cases.append(case('before AUTH', [command], auth=False, protocols=(2,)))
    cases.append(case('failed AUTH keeps the session', [['AUTH', PASSWORD], ['AUTH', 'wrong'], ['GET', 'k']],
                      auth=False, protocols=(2,)))
    for command in (['AUTH', 'x'], ['AUTH', 'default', 'x'], ['AUTH', 'other', 'x'], ['AUTH', 'a', 'b', 'c'], ['AUTH']):
        cases.append(case('no password configured', [command], server='nopass', auth=False, protocols=(2,)))
    cases.append(case('no password configured', [['HELLO', '3', 'AUTH', 'default', 'x']], server='nopass',
                      auth=False, protocols=(2,), loose=[0]))
    cases.append(case('no password configured', [['HELLO', '3', 'AUTH', 'other', 'x']], server='nopass',
                      auth=False, protocols=(2,)))
    # Transactions' own refusals.
    for commands in ([['MULTI'], ['MULTI'], ['EXEC']], [['MULTI'], ['EXEC', 'x'], ['EXEC']],
                     [['MULTI'], ['DISCARD', 'x'], ['EXEC']], [['MULTI'], ['MULTI', 'x'], ['EXEC']],
                     [['MULTI'], ['WATCH', 'k'], ['EXEC']], [['MULTI'], ['WATCH'], ['EXEC']],
                     [['MULTI'], ['AUTH', 'x', 'y', 'z'], ['EXEC']], [['MULTI'], ['AUTH'], ['EXEC']],
                     [['MULTI'], ['QUIT', 'x']], [['MULTI'], ['CLIENT', 'SETNAME', 'a b'], ['EXEC']],
                     [['MULTI'], ['SET', 'k', 'v', 'EX', '0'], ['GET', 'k'], ['EXEC']]):
        cases.append(case('transaction', commands))
    # A replica refuses writes before it queues them, and counts arguments and
    # names commands first, as a primary does.
    for commands in ([['SET', 'k', 'v']], [['nosuch', 'x']], [['GET']], [['SET', 'k']], [['client', 'nosuch']],
                     [['MULTI'], ['SET', 'k', 'v'], ['EXEC']], [['MULTI'], ['nosuch'], ['GET', 'k'], ['EXEC']],
                     [['PFADD', 'h', 'a']], [['GET', 'replica:missing']]):
        cases.append(case('replica', commands, server='replica'))
    if modules:
        for command in arity_cases(MODULE_ARITY):
            cases.append(case('arity, RedisBloom', [command]))
        for command in arity_cases(MODULE_ARITY)[::3]:
            cases.append(case('arity queued, RedisBloom', [['MULTI'], command, ['EXEC']]))
        for command in MODULE_HANDLER_ARITY:
            cases.append(case('arity in the command, RedisBloom', [command], setup=MODULE_SETUP))
            cases.append(case('arity in the command queued, RedisBloom', [['MULTI'], command, ['EXEC']], setup=MODULE_SETUP))
        # RedisBloom's own replies and errors past these - BF.EXISTS of a key of
        # another type answers 0, for one - are RedisBloom parity's to compare.
        for command in (['BF.ADD', 'str', 'a'], ['CF.ADD', 'str', 'a'], ['CMS.QUERY', 'str', 'a']):
            cases.append(case('generic, RedisBloom', [command], setup=SETUP))
    return cases


def redis_arities(port):
    """The arity Redis holds for every command name Keel answers."""
    names = ['PING', 'ECHO', 'SELECT', 'UNWATCH', 'SET', 'SETNX', 'GET', 'INCR', 'INCRBY', 'DECR', 'DECRBY', 'MGET',
             'MSET', 'SETEX', 'PSETEX', 'LCS', 'DEL', 'UNLINK', 'EXISTS', 'TYPE', 'KEYS', 'SCAN', 'TTL', 'PTTL',
             'EXPIRE', 'PEXPIREAT', 'PEXPIRE', 'EXPIREAT', 'PERSIST', 'DBSIZE', 'FLUSHDB', 'MEMORY', 'INFO',
             'BGREWRITEAOF', 'HSET', 'HSETNX', 'HGET', 'HMGET', 'HDEL', 'HEXISTS', 'HLEN', 'HKEYS', 'HVALS', 'HGETALL',
             'HINCRBY', 'LPUSH', 'RPUSH', 'LPOP', 'RPOP', 'LTRIM', 'LLEN', 'LINDEX', 'LSET', 'LRANGE', 'SADD', 'SREM',
             'SCARD', 'SMEMBERS', 'SISMEMBER', 'SMISMEMBER', 'SPOP', 'SRANDMEMBER', 'ZCOUNT', 'ZRANGEBYSCORE',
             'ZREVRANGEBYSCORE', 'ZINCRBY', 'ZPOPMIN', 'ZPOPMAX', 'ZRANGE', 'ZADD', 'ZRANK', 'ZREM', 'ZSCORE', 'ZCARD',
             'GEOADD', 'GEODIST', 'GEOHASH', 'GEOSEARCH', 'GEOPOS', 'PFADD', 'PFCOUNT', 'PFMERGE', 'AUTH', 'HELLO',
             'QUIT', 'CLIENT', 'MULTI', 'EXEC', 'DISCARD']
    session = Session(port, auth=True, protocol=2)
    try:
        session.socket.sendall(encode(['COMMAND', 'INFO', *names]))
        session.stream.readline()
        arities = {}
        for name in names:
            header = session.stream.readline()
            assert header.startswith(b'*'), header
            fields = [read_raw(session.stream) for _ in range(int(header[1:-2]))]
            arities[name] = int(fields[1][1:-2])
        return arities
    finally:
        session.close()


def run_case(servers, side, spec, protocol):
    port = servers.ports[side, f'{side}-{spec["server"]}']
    if spec['server'] == 'primary':
        admin = Session(port, auth=True, protocol=2)
        try:
            assert admin.call('FLUSHDB') == b'+OK\r\n'
            for command in spec['setup']:
                admin.call(*command)
        finally:
            admin.close()
    session = Session(port, auth=spec['auth'], protocol=protocol)
    try:
        replies = []
        for command in spec['commands']:
            try:
                replies.append(session.call(*command))
            except TimeoutError:
                replies.append(b'<no reply within 5 seconds>')
                break
            if command[0].upper() == 'QUIT':
                break
        return replies
    finally:
        session.close()


def show(value):
    return value.decode('latin-1') if isinstance(value, bytes) else str(value)


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--bin', type=Path, required=True)
    parser.add_argument('--redis', type=Path, required=True)
    parser.add_argument('--redis-module', type=Path, help='RedisBloom, which adds the BF, CF and CMS cases')
    parser.add_argument('--out', type=Path, required=True)
    args = parser.parse_args()
    root = args.out.resolve()
    root.mkdir(parents=True, exist_ok=False)
    servers = Servers(args, root)
    report = dict(status='running', binary_sha256=sha256(args.bin), redis_sha256=sha256(args.redis),
                  redis_module_sha256=sha256(args.redis_module) if args.redis_module else None,
                  harness_sha256=sha256(__file__), cases=0, replies=0, mismatches=0, known_differences=0)
    results = []
    try:
        servers.start()
        version = Session(servers.ports['redis', 'redis-primary'], auth=True, protocol=2).call('INFO', 'server')
        report['redis_version'] = next(line.split(':', 1)[1] for line in version.decode().split('\r\n')
                                       if line.startswith('redis_version:'))
        cases = build_cases(redis_arities(servers.ports['redis', 'redis-primary']), args.redis_module is not None)
        for spec in cases:
            for protocol in spec['protocols']:
                keel = run_case(servers, 'keel', spec, protocol)
                redis = spec['expect'] or run_case(servers, 'redis', spec, protocol)
                same = len(keel) == len(redis) and all(
                    a[:1] == b[:1] if i in spec['loose'] else a == b for i, (a, b) in enumerate(zip(keel, redis)))
                known = None if same else next((KNOWN[key] for key in (' '.join(show(p) for p in c)
                                                                         for c in spec['commands']) if key in KNOWN), None)
                report['cases'] += 1
                report['replies'] += len(keel)
                report['mismatches'] += not same and known is None
                report['known_differences'] += known is not None
                results.append(dict(name=spec['name'], server=spec['server'], auth=spec['auth'], protocol=protocol,
                                    commands=[[show(p) for p in c] for c in spec['commands']],
                                    reference='synthesized' if spec['expect'] else 'redis',
                                    keel=[show(r) for r in keel], redis=[show(r) for r in redis], match=same,
                                    **({'known': known} if known else {})))
        report['status'] = 'passed' if report['mismatches'] == 0 else 'failed'
    except BaseException as exc:
        report.update(status='error', failure=repr(exc))
        raise
    finally:
        servers.stop()
        (root / 'cases.json').write_text(json.dumps(results, indent=1) + '\n')
        (root / 'mismatches.json').write_text(json.dumps([r for r in results if not r['match']], indent=1) + '\n')
        report['known'] = sorted({r['known'] for r in results if 'known' in r})
        (root / 'report.json').write_text(json.dumps(report, indent=2) + '\n')
        print(json.dumps(report, indent=2))
    return 0 if report['status'] == 'passed' else 1


if __name__ == '__main__':
    sys.exit(main())
