#!/usr/bin/env python3
"""Disposable eviction/replication/recovery and slow-reader operational checks."""
import argparse
import json
import os
from pathlib import Path
import platform
import statistics
import subprocess
import time

from validation_lib import Client, Server, info, rewrite, sha256
from soak import synchronized


def snapshot(client):
    return {key: client.call('GET', key) for key in client.call('KEYS', '*')}


def eviction(binary, root, policy, limit):
    flags = ['-replication-feed', '-evict', policy]
    flags += ['-maxkeys', '64'] if limit == 'keys' else ['-maxmemory', '128kb']
    primary = Server(binary, root/'primary', async_append=True, extra=flags)
    replica = Server(binary, root/'replica', async_append=True, password=primary.password,
                     extra=['-replicaof', f'127.0.0.1:{primary.port}',
                            '-primary-password-env', 'KEEL_VALIDATION_PASSWORD'])
    try:
        primary.start()
        replica.start()
        for index in range(512):
            assert primary.client.call('SET', f'key:{index}', str(index).encode()+b'x'*4096) == b'OK'
        expected = snapshot(primary.client)
        assert expected and len(expected) <= 64
        for key, value in expected.items():
            assert value == key.split(b':')[1]+b'x'*4096
        stats = info(primary.client, 'stats')
        memory = info(primary.client, 'memory')
        assert int(stats['evicted_keys']) > 0
        if limit == 'bytes':
            assert int(memory['used_memory']) <= 128*1024
        synchronized(primary, replica)
        assert snapshot(replica.client) == expected, 'replica diverged after primary eviction'
        rewrite(primary.client)
        recovery = []
        for _ in range(2):
            start = time.monotonic()
            primary.stop(crash=True)
            primary.start()
            assert snapshot(primary.client) == expected
            synchronized(primary, replica)
            assert snapshot(replica.client) == expected
            recovery.append(time.monotonic()-start)
        return {'status': 'passed', 'policy': policy, 'limit': limit, 'keys_surviving': len(expected),
                'evicted_keys': int(stats['evicted_keys']), 'estimated_keyspace_bytes': int(memory['used_memory']),
                'recovery_seconds': recovery, 'acknowledged_non_evicted_values_lost': 0}
    finally:
        primary.stop(check=False)
        replica.stop(check=False)


def slow_readers(binary, root):
    readers, latencies, rss = [], [], []
    with Server(binary, root, policy='everysec') as server:
        payload = b'x'*1048576
        assert server.client.call('SET', 'large', payload) == b'OK'
        try:
            for _ in range(8):
                reader = Client('127.0.0.1', server.port, server.password)
                readers.append(reader)
                # Queue 16 MiB per reader and intentionally do not drain replies.
                reader.socket.sendall(b'*2\r\n$3\r\nGET\r\n$5\r\nlarge\r\n'*16)
            for _ in range(50):
                start = time.monotonic()
                assert server.client.call('PING') == b'PONG'
                latencies.append((time.monotonic()-start)*1000)
                rss.append(int(subprocess.check_output(['ps','-o','rss=','-p',str(server.process.pid)],text=True)))
                time.sleep(.01)
        finally:
            for reader in readers:
                reader.close()
        assert server.client.call('SET', 'after-slow-readers', 'ok') == b'OK'
        assert server.client.call('GET', 'large') == payload
    with Server(binary, root, policy='everysec') as recovered:
        assert recovered.client.call('GET', 'after-slow-readers') == b'ok'
        assert recovered.client.call('GET', 'large') == payload
    return {'status': 'passed', 'readers': 8, 'offered_reply_bytes_per_reader': 16*1048576,
            'independent_ping_ms': {'median': statistics.median(latencies), 'max': max(latencies)},
            'rss_peak_kib': max(rss), 'clean_restart_verified': True,
            'limits': 'Responsiveness under bounded non-reading clients, not a hard process RSS guarantee or a latency SLO.'}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bin', required=True, type=Path)
    parser.add_argument('--out', required=True, type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    root = args.out.resolve()
    root.mkdir(parents=True, exist_ok=False)
    report = {'status': 'running', 'platform': platform.platform(), 'binary_sha256': sha256(args.bin),
              'harness_sha256': sha256(__file__), 'eviction': []}
    try:
        for policy in ('lru', 'lfu', 'random'):
            for limit in ('keys', 'bytes'):
                report['eviction'].append(eviction(args.bin, root/(policy+'-'+limit), policy, limit))
                (root/'progress.json').write_text(json.dumps(report, indent=2)+'\n')
        report['slow_readers'] = slow_readers(args.bin, root/'slow-readers')
        report['status'] = 'passed'
    except BaseException as exc:
        report.update(status='failed', failure=repr(exc))
        raise
    finally:
        (root/'report.json').write_text(json.dumps(report, indent=2)+'\n')
    print(json.dumps(report, indent=2))


if __name__ == '__main__':
    main()
