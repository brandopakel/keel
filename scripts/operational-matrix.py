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


def mode_flags(mode):
    return ['-replication-protocol', '2', '-aof-concurrent-append'] if mode == 'v2-concurrent' else ['-replication-protocol', '1']


def eviction(binary, root, policy, limit, mode):
    flags = ['-replication-feed', '-evict', policy, *mode_flags(mode)]
    flags += ['-maxkeys', '64'] if limit == 'keys' else ['-maxmemory', '128kb']
    primary = Server(binary, root/'primary', async_append=True, extra=flags)
    replica = Server(binary, root/'replica', async_append=True, password=primary.password,
                     extra=['-replicaof', f'127.0.0.1:{primary.port}',
                            '-primary-password-env', 'KEEL_VALIDATION_PASSWORD', *mode_flags(mode)])
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
        return {'status': 'passed', 'mode': mode, 'policy': policy, 'limit': limit, 'keys_surviving': len(expected),
                'evicted_keys': int(stats['evicted_keys']), 'estimated_keyspace_bytes': int(memory['used_memory']),
                'recovery_seconds': recovery, 'acknowledged_non_evicted_values_lost': 0}
    finally:
        primary.stop(check=False)
        replica.stop(check=False)


def slow_readers(binary, root, mode):
    readers, latencies, rss = [], [], []
    with Server(binary, root, policy='everysec', async_append=True, extra=mode_flags(mode)) as server:
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
    with Server(binary, root, policy='everysec', async_append=True, extra=mode_flags(mode)) as recovered:
        assert recovered.client.call('GET', 'after-slow-readers') == b'ok'
        assert recovered.client.call('GET', 'large') == payload
    return {'status': 'passed', 'mode': mode, 'readers': 8, 'offered_reply_bytes_per_reader': 16*1048576,
            'independent_ping_ms': {'median': statistics.median(latencies), 'max': max(latencies)},
            'rss_peak_kib': max(rss), 'clean_restart_verified': True,
            'limits': 'Responsiveness under bounded non-reading clients, not a hard process RSS guarantee or a latency SLO.'}


def combined(binary, root, mode):
    """Exercise overlapping pressure with two independently recovering replicas."""
    common = mode_flags(mode)
    primary = Server(binary, root/'primary', async_append=True,
                     extra=['-replication-feed', '-maxmemory', '256kb', '-evict', 'lru', *common])
    replicas, readers, latency = [], [], []
    try:
        primary.start()
        primary.client.call('SET', 'seed', b's'*32768)
        for index in range(2):
            replica = Server(binary, root/f'replica-{index}', async_append=True,
                             password=primary.password, extra=['-replicaof', f'127.0.0.1:{primary.port}',
                             '-primary-password-env', 'KEEL_VALIDATION_PASSWORD', *common])
            replicas.append(replica)
            replica.start()
            synchronized(primary, replica)
        for replica in replicas:
            replica.stop(crash=True)
        for _ in range(4):
            reader = Client('127.0.0.1', primary.port, primary.password)
            readers.append(reader)
            reader.socket.sendall(b'*2\r\n$3\r\nGET\r\n$4\r\nseed\r\n'*32)
        before = int(info(primary.client, 'persistence')['aof_rewrites'])
        for index in range(2500):
            start = time.monotonic()
            primary.client.call('SET', f'churn:{index % 128}', str(index).encode()+b'x'*4096)
            latency.append((time.monotonic()-start)*1000)
            if index % 250 == 0 and info(primary.client, 'persistence')['aof_rewrite_in_progress'] == '0':
                primary.client.call('BGREWRITEAOF')
        for index in range(64):
            primary.client.call('SET', f'expiring:{index}', b'x', 'PX', 100)
        time.sleep(.2)
        for index in range(64):
            assert primary.client.call('EXISTS', f'expiring:{index}') == 0
        stats = info(primary.client, 'stats')
        assert int(stats['evicted_keys']) > 0 and int(stats['expired_keys']) > 0
        completed_rewrites = int(info(primary.client, 'persistence')['aof_rewrites']) - before
        assert completed_rewrites > 0, 'a rewrite must complete during combined pressure'
        expected = snapshot(primary.client)
        assert expected
        recovery = []
        for replica in replicas:
            start = time.monotonic()
            replica.start()
            synchronized(primary, replica)
            assert snapshot(replica.client) == expected
            recovery.append(time.monotonic()-start)
        # Both remain attached while the primary recovers its acknowledged state.
        primary.stop(crash=True)
        primary.start()
        assert snapshot(primary.client) == expected
        for replica in replicas:
            synchronized(primary, replica)
            assert snapshot(replica.client) == expected
        assert primary.client.call('PING') == b'PONG'
        return {'status':'passed', 'mode':mode, 'writes_during_replica_outage':2500,
                'replicas':2, 'nonreading_clients':4, 'completed_rewrites':completed_rewrites, 'replica_recovery_seconds':recovery,
                'evicted_keys':int(stats['evicted_keys']), 'expired_keys':int(stats['expired_keys']),
                'surviving_acknowledged_values_lost':0,
                'write_ms':{'median':statistics.median(latency),'max':max(latency)},
                'limits':'Short overlapping-fault correctness test; sequential replica reconnects, no deployment capacity or latency SLO claim.'}
    finally:
        for reader in readers:
            reader.close()
        for replica in replicas:
            replica.stop(check=False)
        primary.stop(check=False)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bin', required=True, type=Path)
    parser.add_argument('--out', required=True, type=Path)
    parser.add_argument('--mode', choices=['all','v1-async','v2-concurrent'], default='all')
    args = parser.parse_args()
    os.umask(0o077)
    root = args.out.resolve()
    root.mkdir(parents=True, exist_ok=False)
    report = {'status': 'running', 'platform': platform.platform(), 'binary_sha256': sha256(args.bin),
              'harness_sha256': sha256(__file__), 'eviction': [], 'slow_readers': [], 'combined': []}
    try:
        for mode in (['v1-async','v2-concurrent'] if args.mode == 'all' else [args.mode]):
            for policy in ('lru', 'lfu', 'random'):
                for limit in ('keys', 'bytes'):
                    report['eviction'].append(eviction(args.bin, root/mode/(policy+'-'+limit), policy, limit, mode))
                    (root/'progress.json').write_text(json.dumps(report, indent=2)+'\n')
            report['slow_readers'].append(slow_readers(args.bin, root/mode/'slow-readers', mode))
            report['combined'].append(combined(args.bin, root/mode/'combined', mode))
        report['status'] = 'passed'
    except BaseException as exc:
        report.update(status='failed', failure=repr(exc))
        raise
    finally:
        (root/'report.json').write_text(json.dumps(report, indent=2)+'\n')
    print(json.dumps(report, indent=2))


if __name__ == '__main__':
    main()
