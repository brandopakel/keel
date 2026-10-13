#!/usr/bin/env python3
"""Scripted redis-server scenarios behind docs/redis-internals-for-keel.md's
"Runtime evidence from REA".

Each scenario starts a redis-server of its own (REDIS_SERVER, by default
/opt/homebrew/bin/redis-server) on a free loopback port, drives it over raw
RESP sockets, prints the bytes seen on the wire as EVIDENCE lines, and stops
the server. Everything it writes goes under REA_OUT. The scenarios are named
after the document's questions (q1 to q14, with q1b and q13b as follow-ups);
run one or more by name, or all with no arguments:

    REA_OUT=dist/rea python3 -I scripts/redis-runtime-scenarios.py q1 q1b

The REA toolkit's capture_process_scenario ran it to record the evidence;
it needs nothing but Python and a redis-server, and each scenario finishes in
well under the two-minute local limit (q8, the longest, in about 18 s).
"""
import os
import socket
import subprocess
import sys
import time
from pathlib import Path

SERVER = os.environ.get('REDIS_SERVER', '/opt/homebrew/bin/redis-server')
OUT = Path(os.environ.get('REA_OUT', '.')).resolve()


def ev(q, label, data):
    text = repr(data) if isinstance(data, (bytes, bytearray)) else str(data)
    if len(text) > 1200:
        text = text[:1200] + f'... ({len(text)} chars)'
    print(f'EVIDENCE {q} {label}: {text}', flush=True)


def free_port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]


def encode(*parts):
    parts = [p if isinstance(p, bytes) else str(p).encode() for p in parts]
    return b'*%d\r\n' % len(parts) + b''.join(b'$%d\r\n' % len(p) + p + b'\r\n' for p in parts)


class Conn:
    def __init__(self, port, timeout=1.0):
        self.s = socket.create_connection(('127.0.0.1', port), timeout=timeout)

    def send(self, data):
        self.s.sendall(data)

    def call(self, *parts, wait=0.3):
        self.s.sendall(encode(*parts))
        return self.drain(wait)

    def drain(self, wait=0.3, limit=1 << 20):
        out = b''
        self.s.settimeout(wait)
        try:
            while len(out) < limit:
                chunk = self.s.recv(65536)
                if not chunk:
                    out += b'<EOF>'
                    break
                out += chunk
        except socket.timeout:
            pass
        return out

    def close(self):
        self.s.close()


class Server:
    def __init__(self, name, *config, port=None):
        self.dir = OUT / name
        self.dir.mkdir(parents=True, exist_ok=True)
        self.port = port or free_port()
        args = [SERVER, '--port', str(self.port), '--bind', '127.0.0.1', '--save', '', '--dir', str(self.dir),
                '--logfile', '', *config]
        self.log = (self.dir / 'server.log').open('wb')
        self.p = subprocess.Popen(args, stdout=self.log, stderr=subprocess.STDOUT, cwd=self.dir)
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            try:
                c = Conn(self.port)
                r = c.call('PING')
                c.close()
                if r.startswith(b'+PONG') or r.startswith(b'-NOAUTH'):
                    return
            except OSError:
                time.sleep(0.05)
        raise SystemExit(f'server {name} did not start; see {self.dir}/server.log')

    def logtext(self):
        self.log.flush()
        return (self.dir / 'server.log').read_text(errors='replace')

    def stop(self):
        self.p.terminate()
        try:
            self.p.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.p.kill()
        self.log.close()


def q1_auth_in_exec():
    s = Server('q1', '--requirepass', 'pw')
    try:
        c = Conn(s.port)
        ev('q1', 'AUTH pw', c.call('AUTH', 'pw'))
        c.send(encode('MULTI') + encode('AUTH', 'wrong') + encode('PING') + encode('EXEC'))
        ev('q1', 'MULTI; AUTH wrong; PING; EXEC (pipelined)', c.drain(0.5))
        c.close()
        c = Conn(s.port)
        c.call('AUTH', 'pw')
        c.send(encode('MULTI') + encode('AUTH', 'pw') + encode('PING') + encode('EXEC'))
        ev('q1', 'MULTI; AUTH pw (correct); PING; EXEC', c.drain(0.5))
        c.close()
        c = Conn(s.port)
        c.call('AUTH', 'pw')
        c.send(encode('MULTI') + encode('HELLO', '3', 'AUTH', 'default', 'wrong') + encode('PING') + encode('EXEC'))
        ev('q1', 'MULTI; HELLO 3 AUTH default wrong; PING; EXEC', c.drain(0.5))
        c.close()
        c = Conn(s.port)
        c.call('AUTH', 'pw')
        c.send(encode('MULTI') + encode('AUTH', 'default', 'wrong') + encode('EXEC'))
        ev('q1', 'MULTI; AUTH default wrong; EXEC', c.drain(0.5))
        ev('q1', 'after EXEC: PING (connection still authenticated?)', c.call('PING'))
        c.close()
    finally:
        s.stop()


def q2_hello():
    s = Server('q2')
    try:
        c = Conn(s.port)
        ev('q2', 'HELLO (RESP2 connection)', c.call('HELLO'))
        ev('q2', 'HELLO 3', c.call('HELLO', '3'))
        ev('q2', 'HELLO 2 after HELLO 3', c.call('HELLO', '2'))
        ev('q2', 'HELLO 3 SETNAME x', c.call('HELLO', '3', 'SETNAME', 'x'))
        c.close()
        ev('q2', 'server version line', [l for l in s.logtext().splitlines() if 'version' in l.lower()][:2])
    finally:
        s.stop()


def q3_push_order():
    s = Server('q3')
    try:
        a, b = Conn(s.port), Conn(s.port)
        ev('q3', 'A HELLO 3', a.call('HELLO', '3')[:40])
        ev('q3', 'A CLIENT TRACKING on', a.call('CLIENT', 'TRACKING', 'on'))
        ev('q3', 'A GET k (remember key)', a.call('GET', 'k'))
        ev('q3', 'B SET k v (invalidates)', b.call('SET', 'k', 'v'))
        time.sleep(0.2)
        a.send(encode('PING') + encode('GET', 'k'))
        ev('q3', 'A then PING; GET k pipelined: bytes in order', a.drain(0.5))
        # A push while the command that caused it is executing on the same connection
        ev('q3', 'A GET k again (remember)', a.call('GET', 'k'))
        a.send(encode('SET', 'k', 'w') + encode('PING'))
        ev('q3', 'A SET k w (own write invalidates own key); PING: bytes in order', a.drain(0.5))
        # SUBSCRIBE and PING pipelined on RESP3
        c = Conn(s.port)
        c.call('HELLO', '3')
        c.send(encode('SUBSCRIBE', 'ch') + encode('PING') + encode('GET', 'k'))
        ev('q3', 'C RESP3: SUBSCRIBE ch; PING; GET k pipelined', c.drain(0.5))
        ev('q3', 'B PUBLISH ch hi', b.call('PUBLISH', 'ch', 'hi'))
        ev('q3', 'C receives', c.drain(0.5))
        # RESP2 subscribed connection
        d = Conn(s.port)
        d.send(encode('SUBSCRIBE', 'ch') + encode('PING') + encode('GET', 'k'))
        ev('q3', 'D RESP2: SUBSCRIBE ch; PING; GET k pipelined', d.drain(0.5))
        for x in (a, b, c, d):
            x.close()
    finally:
        s.stop()


def q4_everysec():
    s = Server('q4', '--appendonly', 'yes', '--appendfsync', 'everysec')
    try:
        c = Conn(s.port)
        time.sleep(1.2)
        c.send(b''.join(encode('SET', f'k{i}', 'v' * 100) for i in range(5000)))
        c.drain(1.0)
        t0 = time.monotonic()
        samples = []
        for _ in range(6):
            info = c.call('INFO', 'persistence', wait=0.5).decode(errors='replace')
            fields = {l.split(':')[0]: l.split(':', 1)[1].strip() for l in info.splitlines() if ':' in l}
            samples.append((round(time.monotonic() - t0, 2), {k: fields.get(k) for k in (
                'aof_pending_bio_fsync', 'aof_delayed_fsync', 'aof_last_write_status', 'aof_buffer_length',
                'aof_pending_rewrite', 'aof_current_size')}))
            time.sleep(0.5)
        ev('q4', 'INFO persistence samples after 5000 SETs under everysec', samples)
        c.close()
    finally:
        s.stop()


def q6_backlog():
    s = Server('q6', '--tcp-backlog', '511')
    try:
        time.sleep(0.3)
        lines = [l for l in s.logtext().splitlines() if 'backlog' in l.lower() or 'somaxconn' in l.lower()]
        ev('q6', 'server log lines about the backlog (kern.ipc.somaxconn is 128 here)', lines)
        c = Conn(s.port)
        ev('q6', 'CONFIG GET tcp-backlog', c.call('CONFIG', 'GET', 'tcp-backlog'))
        c.close()
    finally:
        s.stop()


def q7_encodings():
    s = Server('q7')
    try:
        c = Conn(s.port)
        def enc(key):
            return c.call('OBJECT', 'ENCODING', key).strip()
        c.call('SET', 's1', '123'); c.call('SET', 's2', 'abc'); c.call('SET', 's3', 'a' * 44); c.call('SET', 's4', 'a' * 45)
        ev('q7', 'string: int 123 / abc / 44 bytes / 45 bytes', [enc('s1'), enc('s2'), enc('s3'), enc('s4')])
        c.send(b''.join(encode('HSET', 'h1', f'f{i}', 'v') for i in range(128))); c.drain(0.3)
        c.send(b''.join(encode('HSET', 'h2', f'f{i}', 'v') for i in range(129))); c.drain(0.3)
        c.call('HSET', 'h3', 'f', 'v' * 64); c.call('HSET', 'h4', 'f', 'v' * 65)
        ev('q7', 'hash: 128 fields / 129 fields / value 64 / value 65', [enc('h1'), enc('h2'), enc('h3'), enc('h4')])
        c.send(encode('SADD', 'i1', *[str(i) for i in range(512)])); c.drain(0.3)
        c.send(encode('SADD', 'i2', *[str(i) for i in range(513)])); c.drain(0.3)
        c.send(encode('SADD', 't1', *[f'm{i}' for i in range(128)])); c.drain(0.3)
        c.send(encode('SADD', 't2', *[f'm{i}' for i in range(129)])); c.drain(0.3)
        ev('q7', 'set: 512 ints / 513 ints / 128 strings / 129 strings', [enc('i1'), enc('i2'), enc('t1'), enc('t2')])
        c.send(encode('ZADD', 'z1', *[x for i in range(128) for x in (str(i), f'm{i}')])); c.drain(0.3)
        c.send(encode('ZADD', 'z2', *[x for i in range(129) for x in (str(i), f'm{i}')])); c.drain(0.3)
        c.call('ZADD', 'z3', '1', 'v' * 65)
        ev('q7', 'zset: 128 / 129 members / one member of 65 bytes', [enc('z1'), enc('z2'), enc('z3')])
        c.call('RPUSH', 'l1', 'a'); c.call('RPUSH', 'l2', 'v' * 9000)
        c.send(encode('RPUSH', 'l3', *['v' * 100 for _ in range(100)])); c.drain(0.3)
        ev('q7', 'list: one element / one 9000-byte element / 100 x 100 bytes', [enc('l1'), enc('l2'), enc('l3')])
        ev('q7', 'CONFIG GET of the thresholds', c.call('CONFIG', 'GET', '*-max-*-entries', wait=0.5) + c.call('CONFIG', 'GET', 'list-max-listpack-size'))
        c.close()
    finally:
        s.stop()


def q8_active_expiry():
    s = Server('q8')
    try:
        c = Conn(s.port)
        n = 200000
        batch = b''.join(encode('SET', f'k{i}', 'v', 'PX', str(100 + (i % 900))) for i in range(n))
        t0 = time.monotonic()
        c.send(batch)
        c.drain(2.0, limit=10 << 20)
        ev('q8', f'{n} keys with PX 100..999 loaded in', f'{time.monotonic() - t0:.2f}s')
        samples = []
        for _ in range(8):
            info = c.call('INFO', 'stats', wait=0.5).decode(errors='replace')
            fields = {l.split(':')[0]: l.split(':', 1)[1].strip() for l in info.splitlines() if ':' in l}
            size = c.call('DBSIZE').strip()
            samples.append((round(time.monotonic() - t0, 2), size, {k: fields.get(k) for k in (
                'expired_keys', 'expired_stale_perc', 'expired_time_cap_reached_count',
                'expire_cycle_cpu_milliseconds', 'expired_keys_active')}))
            time.sleep(1.0)
        ev('q8', 'INFO stats and DBSIZE each second', samples)
        ev('q8', 'hz / active-expire-effort', c.call('CONFIG', 'GET', 'hz') + c.call('CONFIG', 'GET', 'active-expire-effort'))
        c.close()
    finally:
        s.stop()


def q9_unauth_limits():
    s = Server('q9', '--requirepass', 'pw')
    try:
        c = Conn(s.port)
        c.send(b'*2\r\n$4\r\nAUTH\r\n$20000\r\n' + b'x' * 20000 + b'\r\n')
        ev('q9', 'unauthenticated: a 20000-byte bulk', c.drain(0.5))
        ev('q9', 'then PING on the same socket', c.call('PING'))
        c.close()
        c = Conn(s.port)
        c.send(encode('AUTH', *['a'] * 12))
        ev('q9', 'unauthenticated: 13-element multibulk', c.drain(0.5))
        ev('q9', 'then PING', c.call('PING'))
        c.close()
        c = Conn(s.port)
        c.send(encode('AUTH', *['a'] * 9))
        ev('q9', 'unauthenticated: 10-element multibulk (arity error expected, not closed)', c.drain(0.5))
        ev('q9', 'then PING', c.call('PING'))
        c.close()
        c = Conn(s.port)
        # 1.1 MiB of a single bulk under 16 KiB chunks is impossible; send many small commands without reading
        try:
            c.send(b'*2\r\n$4\r\nAUTH\r\n$16384\r\n' + b'x' * 16384 + b'\r\n')
            ev('q9', 'unauthenticated: exactly 16384-byte bulk', c.drain(0.5))
            c.send(b'*2\r\n$4\r\nAUTH\r\n$16385\r\n' + b'x' * 16385 + b'\r\n')
            ev('q9', 'unauthenticated: 16385-byte bulk', c.drain(0.5))
        except OSError as e:
            ev('q9', 'socket error', e)
        c.close()
        c = Conn(s.port)
        c.send(b'*1000000\r\n')
        ev('q9', 'unauthenticated: multibulk header *1000000 alone', c.drain(0.5))
        c.close()
        c = Conn(s.port)
        c.call('AUTH', 'pw')
        c.send(encode('AUTH', *['a'] * 12))
        ev('q9', 'authenticated: 13-element multibulk', c.drain(0.5))
        c.close()
    finally:
        s.stop()


def q10_client_list():
    s = Server('q10')
    try:
        c = Conn(s.port)
        c.call('CLIENT', 'SETNAME', 'probe'); c.call('CLIENT', 'SETINFO', 'lib-name', 'rea'); c.call('CLIENT', 'SETINFO', 'lib-ver', '1')
        ev('q10', 'CLIENT INFO (RESP2)', c.call('CLIENT', 'INFO'))
        ev('q10', 'CLIENT LIST', c.call('CLIENT', 'LIST'))
        c.call('HELLO', '3')
        ev('q10', 'CLIENT INFO (RESP3)', c.call('CLIENT', 'INFO'))
        ev('q10', 'CLIENT LIST TYPE normal', c.call('CLIENT', 'LIST', 'TYPE', 'normal'))
        c.close()
    finally:
        s.stop()


def q11_monitor():
    s = Server('q11')
    try:
        m, c = Conn(s.port), Conn(s.port)
        ev('q11', 'MONITOR', m.call('MONITOR'))
        c.call('SET', 'k', 'v', 'EX', '100'); c.call('EXPIRE', 'k', '200'); c.call('SET', 'k2', 'v', 'GET')
        c.call('SETEX', 'k3', '10', 'v'); c.call('GETEX', 'k', 'PX', '5000'); c.call('INCR', 'n')
        c.send(encode('MULTI') + encode('SET', 'k4', 'v', 'PX', '1000') + encode('EXEC')); c.drain(0.3)
        time.sleep(0.3)
        ev('q11', 'MONITOR lines (original argv or rewritten?)', m.drain(0.5))
        m.close(); c.close()
    finally:
        s.stop()


def q12_slowlog():
    s = Server('q12')
    try:
        c = Conn(s.port)
        c.call('CONFIG', 'SET', 'slowlog-log-slower-than', '0')
        c.call('SADD', 'big', *[('a%02d' % i) + 'x' * 200 for i in range(40)])
        ev('q12', 'SLOWLOG GET 1 after SADD with 40 arguments of 203 bytes', c.call('SLOWLOG', 'GET', '1', wait=0.5))
        ev('q12', 'CONFIG GET slowlog-entry-max-argc / slowlog-max-len', c.call('CONFIG', 'GET', 'slowlog-entry-max-argc') + c.call('CONFIG', 'GET', 'slowlog-max-len'))
        c.close()
    finally:
        s.stop()


def q13_psync():
    s = Server('q13')
    try:
        c = Conn(s.port)
        c.call('SET', 'k', 'v')
        ev('q13', 'REPLCONF listening-port 1', c.call('REPLCONF', 'listening-port', '1'))
        ev('q13', 'REPLCONF capa eof capa psync2', c.call('REPLCONF', 'capa', 'eof', 'capa', 'psync2'))
        c.send(encode('PSYNC', '?', '-1'))
        time.sleep(1.0)
        data = c.drain(1.0)
        ev('q13', 'PSYNC ? -1: first 300 bytes', data[:300])
        ev('q13', 'PSYNC ? -1: total bytes received in ~2 s', len(data))
        ev('q13', 'INFO replication', c.call('INFO', 'replication', wait=0.5))
        c.close()
    finally:
        s.stop()


def q14_replica_expiry():
    primary = Server('q14-primary', '--enable-debug-command', 'yes')
    replica = Server('q14-replica', '--replicaof', '127.0.0.1', str(primary.port))
    try:
        p, r = Conn(primary.port), Conn(replica.port)
        for _ in range(50):
            if b'master_link_status:up' in r.call('INFO', 'replication', wait=0.3):
                break
            time.sleep(0.1)
        ev('q14', 'primary DEBUG SET-ACTIVE-EXPIRE 0', p.call('DEBUG', 'SET-ACTIVE-EXPIRE', '0'))
        p.call('SET', 'k', 'v', 'PX', '300')
        time.sleep(0.2)
        ev('q14', 'replica before expiry: GET k / DBSIZE', [r.call('GET', 'k'), r.call('DBSIZE')])
        time.sleep(0.6)
        ev('q14', 'replica after expiry, primary not expiring: GET k / EXISTS k / TTL k / DBSIZE / KEYS *',
           [r.call('GET', 'k'), r.call('EXISTS', 'k'), r.call('TTL', 'k'), r.call('DBSIZE'), r.call('KEYS', '*')])
        ev('q14', 'primary meanwhile: GET k / DBSIZE', [p.call('GET', 'k'), p.call('DBSIZE')])
        ev('q14', 'primary DBSIZE after its lazy GET', p.call('DBSIZE'))
        time.sleep(0.3)
        ev('q14', 'replica after the primary deleted it: DBSIZE / INFO stats expired_keys',
           [r.call('DBSIZE'), [l for l in r.call('INFO', 'stats', wait=0.5).decode().splitlines() if l.startswith('expired_keys')]])
        p.call('SET', 'k2', 'v', 'PX', '300')
        time.sleep(0.6)
        ev('q14', 'replica, key k2 expired, primary still not expiring actively: DBSIZE', r.call('DBSIZE'))
        ev('q14', 'primary DEBUG SET-ACTIVE-EXPIRE 1', p.call('DEBUG', 'SET-ACTIVE-EXPIRE', '1'))
        time.sleep(0.5)
        ev('q14', 'replica DBSIZE after active expiry resumed on the primary', r.call('DBSIZE'))
        p.close(); r.close()
    finally:
        replica.stop(); primary.stop()


def q5_segmentation():
    s = Server('q5')
    try:
        c = Conn(s.port, timeout=5)
        c.call('SET', 'big', 'x' * (4 << 20))
        sizes = []
        c.s.sendall(encode('GET', 'big') * 4)
        total, t0 = 0, time.monotonic()
        c.s.settimeout(2.0)
        want = 4 * ((4 << 20) + len(b'$4194304\r\n') + 2)
        try:
            while total < want:
                chunk = c.s.recv(1 << 20)
                if not chunk:
                    break
                total += len(chunk)
                sizes.append((round((time.monotonic() - t0) * 1000, 1), len(chunk)))
        except socket.timeout:
            pass
        ev('q5', 'four pipelined GETs of a 4 MiB value: bytes received / expected', (total, want))
        ev('q5', 'recv() calls and the largest single read', (len(sizes), max(s for _, s in sizes)))
        ev('q5', 'first 12 reads (ms since send, bytes)', sizes[:12])
        info = c.call('INFO', 'stats', wait=0.5).decode(errors='replace')
        ev('q5', 'io_threaded and reply-buffer counters', [l for l in info.splitlines() if l.startswith(('total_net_output_bytes', 'io_threaded_writes_processed', 'reply_buffer'))])
        c.close()
    finally:
        s.stop()


def q1b_pipelined_auth():
    s = Server('q1b', '--requirepass', 'pw')
    try:
        c = Conn(s.port)
        ev('q1b', 'AUTH pw', c.call('AUTH', 'pw'))
        c.send(encode('PING') + encode('AUTH', 'wrong') + encode('PING'))
        ev('q1b', 'top level, pipelined in one write: PING; AUTH wrong; PING', c.drain(0.8))
        c.call('PING')
        ev('q1b', 'top level, alone: AUTH wrong', c.call('AUTH', 'wrong'))
        c.close()
        c = Conn(s.port)
        c.send(encode('PING') + encode('AUTH', 'wrong'))
        ev('q1b', 'unauthenticated, pipelined: PING; AUTH wrong', c.drain(0.8))
        c.close()
    finally:
        s.stop()


def q13b_psync():
    s = Server('q13b', '--repl-diskless-sync-delay', '0')
    try:
        c, w = Conn(s.port, timeout=3), Conn(s.port)
        w.call('SET', 'k', 'v')
        c.call('REPLCONF', 'capa', 'eof', 'capa', 'psync2')
        c.send(encode('PSYNC', '?', '-1'))
        head = c.drain(1.5)
        ev('q13b', 'PSYNC ? -1 with repl-diskless-sync-delay 0: first 120 bytes', head[:120])
        ev('q13b', 'bytes of the full sync received', len(head))
        w.call('SET', 'k2', 'v2'); w.call('INCR', 'n')
        ev('q13b', 'command stream after two writes on another connection', c.drain(1.5)[:300])
        ev('q13b', 'INFO replication of the primary', [l for l in w.call('INFO', 'replication', wait=0.5).decode().splitlines() if l.startswith(('connected_slaves', 'slave0', 'master_repl_offset', 'master_replid'))])
        c.close(); w.close()
    finally:
        s.stop()


SCENARIOS = {f.__name__.split('_')[0]: f for f in (q1_auth_in_exec, q2_hello, q3_push_order, q4_everysec, q5_segmentation, q6_backlog,
                                                    q7_encodings, q8_active_expiry, q9_unauth_limits, q10_client_list,
                                                    q11_monitor, q12_slowlog, q13_psync, q14_replica_expiry, q1b_pipelined_auth, q13b_psync)}

if __name__ == '__main__':
    which = sys.argv[1:] or sorted(SCENARIOS)
    print(f'redis-server: {SERVER}; out: {OUT}', flush=True)
    for q in which:
        print(f'== {q}', flush=True)
        SCENARIOS[q]()
    print('DONE', flush=True)
