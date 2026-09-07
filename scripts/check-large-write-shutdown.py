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


def run(args):
    root = args.out.resolve()
    root.mkdir(parents=True, exist_ok=False)
    report = dict(status='running', binary_sha256=sha256(args.bin),
                  policy=args.policy, mode=args.mode, seconds=args.seconds,
                  purpose='shutdown diagnosis, not a capacity or comparative benchmark')
    flags = ['-auto-aof-rewrite-percentage', '0']
    if args.mode == 'concurrent':
        flags.append('-aof-concurrent-append')
    server = Server(args.bin, root/'server', policy=args.policy,
                    async_append=args.mode != 'sync', extra=flags)
    stop = threading.Event()
    value = b'v' * (1 << 20)
    counts = [0]*4
    errors = []
    def write(index):
        client = None
        try:
            client = Client('127.0.0.1', server.port, server.password)
            while not stop.is_set():
                assert client.call('SET', f'writer:{index}', value) == b'OK'
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
            assert server.client.call('GET', f'writer:{index}') == value
        started = time.monotonic()
        try:
            server.stop()
        finally:
            report['shutdown_seconds'] = time.monotonic()-started
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
                with path.open('rb') as source, gzip.open(str(path)+'.gz', 'xb', compresslevel=1) as out:
                    while block := source.read(1 << 20):
                        out.write(block)
                report['aof']['failure_archive'] = 'server/store.aof.gz'
            # Hosted artifacts preserve failure data; no raw duplicate remains.
            (root/'report.json').write_text(json.dumps(report, indent=2)+'\n')
            path.unlink()
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
    args = parser.parse_args()
    if os.environ.get('GITHUB_ACTIONS') != 'true':
        parser.error('this large-file diagnostic runs only on hosted GitHub Actions')
    if not 1 <= args.seconds <= 20:
        parser.error('seconds must be 1..20')
    raise SystemExit(run(args))
