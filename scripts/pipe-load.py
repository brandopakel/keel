#!/usr/bin/env python3
"""Load the same keys into Keel and into Redis with redis-cli --pipe.

redis-cli --pipe sends its input as it is, then "\\r\\n" and an ECHO of twenty
random bytes, and stops when that ECHO is answered (redis-cli.c, pipeMode).
The "\\r\\n" is an empty inline request, which Redis skips. Three inputs are
sent to both servers, one after another: RESP only, the form Redis's mass
insertion guide recommends; inline lines with quotes, escapes, tabs, bare
newlines and empty lines among them; and a mix of the two with hashes and
lists.

For each input, the summary redis-cli prints must be the same for both
servers and count no errors. Then DBSIZE must be the number of keys loaded,
on both, and sampled keys must hold the same values. Keel runs with its
append-only log and is restarted from it: the keys must come back, which
shows the log holds the commands as parsed, not the lines as sent.

The report, the inputs' sizes and checksums, and both servers' logs are
written under --out; the exit status is non-zero on any difference.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import time


def sha256(path):
    digest = hashlib.sha256()
    with Path(path).open('rb') as source:
        while chunk := source.read(1 << 20):
            digest.update(chunk)
    return digest.hexdigest()


def resp(*parts):
    parts = [p if isinstance(p, bytes) else str(p).encode() for p in parts]
    return b'*%d\r\n' % len(parts) + b''.join(b'$%d\r\n' % len(p) + p + b'\r\n' for p in parts)


def inputs(keys):
    """The three inputs, and how many keys they load in all."""
    loaded = set()
    plain = []
    for i in range(keys):
        loaded.add(f'pipe:resp:{i}')
        plain.append(resp('SET', f'pipe:resp:{i}', f'value-{i}'))
    lines = []
    for i in range(keys):
        loaded.add(f'pipe:inline:{i}')
        if i % 4 == 0:
            line = b'SET pipe:inline:%d "value %d\\x41\\n\\"q\\""' % (i, i)
        elif i % 4 == 1:
            line = b"SET pipe:inline:%d 'it\\'s %d'" % (i, i)
        elif i % 4 == 2:
            line = b'SET\tpipe:inline:%d\tv%d' % (i, i)
        else:
            line = b'  set pipe:inline:%d v%d  ' % (i, i)
        lines.append(line + (b'\n' if i % 5 == 0 else b'\r\n'))
        if i % 1000 == 999:
            lines.append(b'\r\n' if i % 2000 == 999 else b'\n')
    mixed = []
    for i in range(keys):
        if i % 3 == 0:
            loaded.add(f'pipe:hash:{i % 100}')
            mixed.append(resp('HSET', f'pipe:hash:{i % 100}', f'f{i}', f'v{i}'))
        elif i % 3 == 1:
            loaded.add(f'pipe:list:{i % 100}')
            mixed.append(b'RPUSH pipe:list:%d "item %d"\r\n' % (i % 100, i))
        else:
            loaded.add(f'pipe:mixed:{i}')
            mixed.append(b'SET pipe:mixed:%d %d\r\n' % (i, i))
    return [('resp', b''.join(plain)), ('inline', b''.join(lines)), ('mixed', b''.join(mixed))], len(loaded)


def free_port():
    with socket.socket() as reservation:
        reservation.bind(('127.0.0.1', 0))
        return reservation.getsockname()[1]


def read_reply(stream):
    """One whole reply, as the bytes that carried it."""
    line = stream.readline()
    if not line.endswith(b'\r\n'):
        raise ValueError(f'invalid reply {line!r}')
    if line[:1] == b'$' and line != b'$-1\r\n':
        return line + stream.read(int(line[1:-2]) + 2)
    if line[:1] == b'*' and line != b'*-1\r\n':
        return line + b''.join(read_reply(stream) for _ in range(int(line[1:-2])))
    return line


def call(port, *parts):
    with socket.create_connection(('127.0.0.1', port), timeout=10) as conn, conn.makefile('rb') as stream:
        conn.sendall(resp(*parts))
        return read_reply(stream)


def wait_ready(process, port, log):
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError(f'server exited during startup; see {log}')
        try:
            if call(port, 'PING') == b'+PONG\r\n':
                return
        except OSError:
            pass
        time.sleep(.05)
    raise TimeoutError(f'server on {port} not ready; see {log}')


class Server:
    def __init__(self, command, directory, port, stdin=None):
        self.port, self.log_path = port, directory / 'server.log'
        self.log = self.log_path.open('ab')
        env = {key: os.environ[key] for key in ('PATH', 'HOME', 'TMPDIR') if key in os.environ}
        self.process = subprocess.Popen(command, cwd=directory, env=env, stdout=self.log, stderr=self.log,
                                        stdin=subprocess.PIPE if stdin else None)
        if stdin:
            self.process.stdin.write(stdin)
            self.process.stdin.close()
        wait_ready(self.process, port, self.log_path)

    def stop(self):
        if self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait()
        self.log.close()


def start_keel(args, directory, port):
    return Server([str(args.bin.resolve()), '-host', '127.0.0.1', '-port', str(port), '-appendonly',
                   '-appendfilename', str(directory / 'store.aof')], directory, port)


def start_redis(args, directory, port):
    # Debian's redis-server may be a symlink to a multicall executable;
    # preserve argv[0], which selects server rather than RDB-check mode.
    config = '\n'.join(['bind 127.0.0.1', f'port {port}', 'save ""', 'appendonly no', f'dir {directory}']) + '\n'
    return Server([str(args.redis.absolute()), '-'], directory, port, stdin=config.encode())


def pipe(args, port, path):
    with path.open('rb') as source:
        done = subprocess.run([str(args.cli.absolute()), '-h', '127.0.0.1', '-p', str(port), '--pipe'],
                              stdin=source, capture_output=True, timeout=args.timeout)
    output = (done.stdout + done.stderr).decode('latin-1')
    summary = next((line for line in output.splitlines() if line.startswith('errors:')), None)
    return dict(exit=done.returncode, summary=summary, output=output[-2000:])


def sample(port, keys):
    probes = []
    for i in range(0, keys, max(1, keys // 50)):
        probes += [('GET', f'pipe:resp:{i}'), ('GET', f'pipe:inline:{i}'), ('GET', f'pipe:mixed:{i}')]
    probes += [('HLEN', f'pipe:hash:{i}') for i in range(0, 100, 7)]
    probes += [('LRANGE', f'pipe:list:{i}', '0', '2') for i in range(0, 100, 7)]
    return {' '.join(p): call(port, *p).decode('latin-1') for p in probes}


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--bin', type=Path, required=True, help='keel')
    parser.add_argument('--redis', type=Path, required=True, help='redis-server')
    parser.add_argument('--cli', type=Path, required=True, help='redis-cli')
    parser.add_argument('--keys', type=int, default=50000, help='keys per input')
    parser.add_argument('--timeout', type=int, default=300, help='seconds per redis-cli --pipe')
    parser.add_argument('--out', type=Path, required=True)
    args = parser.parse_args()
    root = args.out.resolve()
    root.mkdir(parents=True, exist_ok=False)
    report = dict(status='running', keys_per_input=args.keys, binary_sha256=sha256(args.bin),
                  redis_sha256=sha256(args.redis), cli_sha256=sha256(args.cli), harness_sha256=sha256(__file__),
                  inputs=[], failures=[])
    servers = []
    try:
        loads, distinct = inputs(args.keys)
        report['expected_dbsize'] = distinct
        for name in ('keel', 'redis'):
            (root / name).mkdir()
        keel_port, redis_port = free_port(), free_port()
        keel = start_keel(args, root / 'keel', keel_port)
        servers.append(keel)
        servers.append(start_redis(args, root / 'redis', redis_port))
        report['redis_version'] = subprocess.run([str(args.redis.absolute()), '--version'], capture_output=True,
                                                 text=True).stdout.strip()
        for name, data in loads:
            path = root / f'{name}.input'
            path.write_bytes(data)
            entry = dict(name=name, bytes=len(data), sha256=sha256(path))
            for side, port in (('keel', keel_port), ('redis', redis_port)):
                started = time.monotonic()
                entry[side] = pipe(args, port, path)
                entry[side]['seconds'] = round(time.monotonic() - started, 3)
            if entry['keel']['summary'] != entry['redis']['summary']:
                report['failures'].append(f"{name}: redis-cli summarised Keel's load as {entry['keel']['summary']!r} "
                                          f"and Redis's as {entry['redis']['summary']!r}")
            for side in ('keel', 'redis'):
                if entry[side]['exit'] != 0 or not (entry[side]['summary'] or '').startswith('errors: 0,'):
                    report['failures'].append(f'{name}: redis-cli against {side}: {entry[side]["summary"]!r}, '
                                              f'exit {entry[side]["exit"]}')
            path.unlink()
            report['inputs'].append(entry)
        sizes = {side: call(port, 'DBSIZE').decode().strip() for side, port in (('keel', keel_port), ('redis', redis_port))}
        report['dbsize'] = sizes
        for side, size in sizes.items():
            if size != f':{distinct}':
                report['failures'].append(f'{side} holds {size}, not :{distinct}')
        keel_values, redis_values = sample(keel_port, args.keys), sample(redis_port, args.keys)
        report['sampled'] = len(redis_values)
        differing = [k for k in redis_values if keel_values[k] != redis_values[k]]
        if differing:
            report['failures'].append(f'{len(differing)} sampled replies differ, first {differing[0]!r}: '
                                      f'Keel {keel_values[differing[0]]!r}, Redis {redis_values[differing[0]]!r}')
        # Keel from its log alone.
        keel.stop()
        servers.remove(keel)
        keel = start_keel(args, root / 'keel', keel_port)
        servers.append(keel)
        report['dbsize_after_restart'] = call(keel_port, 'DBSIZE').decode().strip()
        if report['dbsize_after_restart'] != f':{distinct}':
            report['failures'].append(f"Keel restarted from its log holds {report['dbsize_after_restart']}")
        restarted = sample(keel_port, args.keys)
        differing = [k for k in redis_values if restarted[k] != redis_values[k]]
        if differing:
            report['failures'].append(f'after the restart, {len(differing)} sampled replies differ, first '
                                      f'{differing[0]!r}: {restarted[differing[0]]!r}')
        report['status'] = 'passed' if not report['failures'] else 'failed'
    except BaseException as exc:
        report.update(status='error', failure=repr(exc))
        raise
    finally:
        for server in servers:
            server.stop()
        aof = root / 'keel' / 'store.aof'
        if aof.exists():
            report['keel_log_bytes'] = aof.stat().st_size
            aof.unlink()
        (root / 'report.json').write_text(json.dumps(report, indent=2) + '\n')
        print(json.dumps({k: v for k, v in report.items() if k != 'inputs'}, indent=2))
        for entry in report['inputs']:
            print(entry['name'], entry['bytes'], 'bytes;', 'keel:', entry['keel']['summary'],
                  f"({entry['keel']['seconds']} s);", 'redis:', entry['redis']['summary'],
                  f"({entry['redis']['seconds']} s)")
    return 0 if report['status'] == 'passed' else 1


if __name__ == '__main__':
    sys.exit(main())
