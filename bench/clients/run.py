#!/usr/bin/env python3
"""Run real RESP2 clients against owned servers, then verify two AOF restarts."""
import argparse
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / 'scripts'))
from validation_lib import Client, sha256

ROOT = Path(__file__).resolve().parent
LIBRARIES = ('go-redis', 'redigo', 'redis-py', 'node-redis', 'ioredis')
VALUE = {'hex': '00ff0d0a62696e'}


def fixture(library, verify_only):
    prefix = f'compat:{library}:'
    key = lambda suffix: prefix + suffix
    commands = []
    def case(name, args, expected=None, **extra):
        commands.append(dict(name=name, args=args, expected=expected, **extra))
    case('ping', ['PING'], 'PONG')
    case('binary SET', ['SET', key('string'), VALUE], 'OK')
    case('binary GET', ['GET', key('string')], VALUE)
    case('nil GET', ['GET', key('missing')])
    case('conditional SET', ['SET', key('string'), 'wrong', 'NX'])
    case('MGET nil and repeated key', ['MGET', key('string'), key('missing'), key('string')], [VALUE, None, VALUE])
    case('hash fields', ['HSET', key('hash'), 'f1', VALUE, 'f2', 'v2'], 2)
    case('HMGET', ['HMGET', key('hash'), 'f1', 'f2', 'missing'], [VALUE, 'v2', None])
    case('list push', ['LPUSH', key('list'), 'a', 'b'], 2)
    case('list range', ['LRANGE', key('list'), '0', '-1'], ['b', 'a'])
    case('set add', ['SADD', key('set'), 'a', 'b'], 2)
    case('set membership', ['SISMEMBER', key('set'), 'a'], 1)
    case('sorted add', ['ZADD', key('zset'), '2', 'b', '1', 'a'], 2)
    case('sorted rank order', ['ZRANGE', key('zset'), '0', '-1'], ['a', 'b'])
    case('expiry SET', ['SET', key('expiry'), 'temporary', 'PX', '10000'], 'OK')
    case('millisecond TTL', ['PTTL', key('expiry')], range=[0, 10000])
    case('shorten TTL', ['PEXPIRE', key('expiry'), '60'], 1)
    case('counter init', ['SET', key('counter'), '0'], 'OK')
    case('marker', ['SET', key('marker'), VALUE], 'OK')
    case('wrong type', ['HGET', key('string'), 'field'], error='WRONGTYPE')
    case('connection after error', ['PING'], 'PONG')
    verification = [dict(name=name, args=args, expected=expected) for name, args, expected in (
        ('persistent binary', ['GET', key('marker')], VALUE),
        ('preserved string', ['GET', key('string')], VALUE),
        ('persistent counter', ['GET', key('counter')], '257'),
        ('persistent hash', ['HMGET', key('hash'), 'f1', 'f2'], [VALUE, 'v2']),
        ('persistent list', ['LRANGE', key('list'), '0', '-1'], ['b', 'a']),
        ('persistent set', ['SISMEMBER', key('set'), 'b'], 1),
        ('persistent sorted set', ['ZRANGE', key('zset'), '0', '-1'], ['a', 'b']),
        ('expired key absent', ['GET', key('expiry')], None))]
    return dict(prefix=prefix, commands=commands, verification=verification, verify_only=verify_only,
                marker_key=key('marker'), marker_value=VALUE, counter_key=key('counter'),
                expiry_key=key('expiry'), scan_keys=[key(x) for x in ('string', 'counter', 'marker')])


def run(args):
    args.out.mkdir(parents=True, exist_ok=False)
    report = dict(status='running', binary_sha256=sha256(args.bin), modes=[])
    env = {key: os.environ[key] for key in ('PATH', 'HOME', 'TMPDIR') if key in os.environ}
    try:
        for mode in args.modes.split(','):
            if mode not in ('off', 'barrier', 'concurrent'):
                raise ValueError('unknown append mode')
            directory = args.out / mode
            directory.mkdir()
            password = '' if mode == 'off' else 'public-client-compatibility-fixture'
            mode_report = dict(mode=mode, authenticated=bool(password), phases=[])
            report['modes'].append(mode_report)
            for phase in range(1 if mode == 'off' else 3):
                with socket.socket() as reservation:
                    reservation.bind(('127.0.0.1', 0))
                    port = reservation.getsockname()[1]
                phase_dir = directory / str(phase)
                phase_dir.mkdir()
                command = [str(args.bin), '-host', '127.0.0.1', '-port', str(port)]
                if mode != 'off':
                    command += ['-appendonly', '-aof-async-append', '-appendfsync', 'always',
                                '-appendfilename', str(directory/'store.aof'),
                                '-requirepass-env', 'KEEL_COMPAT_PASSWORD']
                    if mode == 'concurrent':
                        command += ['-aof-concurrent-append']
                child_env = dict(env, KEEL_COMPAT_ADDR=f'127.0.0.1:{port}', KEEL_COMPAT_PASSWORD=password)
                phase_report = dict(phase=phase, clients=[])
                mode_report['phases'].append(phase_report)
                with (phase_dir/'server.log').open('wb') as log:
                    server = subprocess.Popen(command, env=child_env, stdout=log, stderr=log)
                    try:
                        deadline = time.monotonic()+10
                        while True:
                            if server.poll() is not None:
                                raise RuntimeError('owned server exited')
                            client = None
                            try:
                                client = Client('127.0.0.1', port)
                                if password:
                                    assert client.call('AUTH', password) == b'OK'
                                assert client.call('PING') == b'PONG'
                                break
                            except OSError:
                                if time.monotonic() >= deadline:
                                    raise TimeoutError('owned server did not listen')
                                time.sleep(.02)
                            finally:
                                if client is not None:
                                    client.close()
                        for library in LIBRARIES:
                            path = phase_dir/f'{library}.fixture.json'
                            path.write_text(json.dumps(fixture(library, phase > 0)))
                            if library in ('go-redis', 'redigo'):
                                command = [str(args.go_clients), library, str(path)]
                            elif library == 'redis-py':
                                command = [str(args.python), str(ROOT/'python_client.py'), str(path)]
                            else:
                                command = [args.node, str(ROOT/'node/client.mjs'), library, str(path)]
                            try:
                                result = subprocess.run(command, env=child_env, text=True, capture_output=True, timeout=30)
                            except subprocess.TimeoutExpired as exc:
                                for suffix, captured in [('stdout', exc.stdout), ('stderr', exc.stderr)]:
                                    text = captured.decode(errors='replace') if isinstance(captured, bytes) else captured or ''
                                    (phase_dir/f'{library}.{suffix}').write_text(text)
                                raise
                            (phase_dir/f'{library}.stdout').write_text(result.stdout)
                            (phase_dir/f'{library}.stderr').write_text(result.stderr)
                            if result.returncode:
                                raise RuntimeError(f'{mode}/{phase}/{library} failed; see captured output')
                            phase_report['clients'].append(json.loads(result.stdout))
                        server.send_signal(signal.SIGTERM)
                        if server.wait(timeout=10):
                            raise RuntimeError('owned server failed shutdown')
                    finally:
                        if server.poll() is None:
                            server.kill()
                            server.wait()
        report['status'] = 'passed'
    except BaseException as exc:
        report.update(status='failed', error=repr(exc))
        raise
    finally:
        (args.out/'report.json').write_text(json.dumps(report, indent=2)+'\n')


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--bin', type=Path, required=True)
    parser.add_argument('--go-clients', type=Path, required=True)
    parser.add_argument('--python', type=Path, required=True)
    parser.add_argument('--node', default='node')
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--modes', default='off,barrier,concurrent')
    options = parser.parse_args()
    options.bin = options.bin.resolve()
    options.go_clients = options.go_clients.resolve()
    options.out = options.out.resolve()
    run(options)
