#!/usr/bin/env python3
"""Hosted diagnostic: capture shutdown after a sustained large-value write burst."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import gzip
import json
import os
from pathlib import Path
import threading
import time

from validation_lib import Client, Server, info, sha256


def writer_value(index, sequence):
    header = f'{index:02d}:{sequence:012d}:'.encode()
    return header + b'v' * ((1 << 20)-len(header))


def compress_aof(source, destination):
    with source.open('rb') as src, gzip.open(destination, 'xb', compresslevel=1) as out:
        while block := src.read(1 << 20):
            out.write(block)


def run(args):
    root = args.out.resolve()
    root.mkdir(parents=True, exist_ok=False)
    report = dict(status='running', binary_sha256=sha256(args.bin),
                  policy=args.policy, mode=args.mode, seconds=args.seconds,
                  shutdown_grace_seconds=args.shutdown_seconds,
                  purpose='shutdown diagnosis, not a capacity or comparative benchmark')
    flags = ['-auto-aof-rewrite-percentage', '0', '-shutdown-timeout', f'{args.shutdown_seconds}s']
    if args.mode == 'concurrent':
        flags.append('-aof-concurrent-append')
    server = Server(args.bin, root/'server', policy=args.policy,
                    async_append=args.mode != 'sync', extra=flags,
                    shutdown_timeout=args.shutdown_seconds+10, startup_timeout=60)
    stop = threading.Event()
    closed_archive = root/'closed-before-replay.aof.gz'
    counts = [0]*4
    errors = []
    def write(index):
        client = None
        try:
            client = Client('127.0.0.1', server.port, server.password)
            while not stop.is_set():
                assert client.call('SET', f'writer:{index}', writer_value(index, counts[index]+1)) == b'OK'
                counts[index] += 1
        except Exception as exc:
            errors.append(repr(exc))
            stop.set()
        finally:
            if client:
                client.close()
    try:
        server.start()
        with ThreadPoolExecutor(max_workers=4) as pool:
            futures = [pool.submit(write, index) for index in range(4)]
            stop.wait(args.seconds)
            stop.set()
            for future in futures:
                future.result()
        report.update(acknowledged_writes=counts, workload_errors=errors,
                      before_shutdown=info(server.client, 'persistence'))
        assert not errors and all(counts), 'write burst did not complete cleanly'
        for index in range(4):
            assert server.client.call('GET', f'writer:{index}') == writer_value(index, counts[index])
        started = time.monotonic()
        try:
            server.stop()
        finally:
            report['shutdown_seconds'] = time.monotonic()-started
        path = root/'server/store.aof'
        report['closed_aof'] = dict(bytes=path.stat().st_size, sha256=sha256(path))
        # Replay may repair a torn tail. Preserve the original closed bytes first
        # so a failing replay never destroys the evidence we need to inspect.
        compress_aof(path, closed_archive)
        report['closed_aof']['failure_archive'] = closed_archive.name
        started = time.monotonic()
        server.start()
        report['replay_ready_seconds'] = time.monotonic()-started
        for index in range(4):
            assert server.client.call('GET', f'writer:{index}') == writer_value(index, counts[index]), 'replay lost a final acknowledged sequence'
        report['recovered_sequences'] = counts[:]
        server.stop()
        report['status'] = 'passed'
    except Exception as exc:
        report.update(status='failed', failure=repr(exc))
    finally:
        stop.set()
        server.stop(check=False)
        path = root/'server/store.aof'
        if path.exists():
            report['aof'] = dict(bytes=path.stat().st_size, sha256=sha256(path))
            if report['status'] != 'passed':
                compress_aof(path, str(path)+'.gz')
                report['aof']['failure_archive'] = 'server/store.aof.gz'
            # Hosted artifacts preserve failure data; no raw duplicate remains.
            (root/'report.json').write_text(json.dumps(report, indent=2)+'\n')
            path.unlink()
        if report['status'] == 'passed':
            report['closed_aof']['failure_archive'] = None
            (root/'report.json').write_text(json.dumps(report, indent=2)+'\n')
            closed_archive.unlink()
        (root/'report.json').write_text(json.dumps(report, indent=2)+'\n')
    print(json.dumps(report, indent=2))
    return 0 if report['status'] == 'passed' else 1


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bin', required=True)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--policy', choices=['no', 'everysec'], required=True)
    parser.add_argument('--mode', choices=['sync', 'barrier', 'concurrent'], required=True)
    parser.add_argument('--seconds', type=int, default=10)
    parser.add_argument('--shutdown-seconds', type=int, default=30)
    args = parser.parse_args()
    if os.environ.get('GITHUB_ACTIONS') != 'true':
        parser.error('this large-file diagnostic runs only on hosted GitHub Actions')
    if not 1 <= args.seconds <= 20:
        parser.error('seconds must be 1..20')
    if not 1 <= args.shutdown_seconds <= 60:
        parser.error('shutdown-seconds must be 1..60')
    raise SystemExit(run(args))
