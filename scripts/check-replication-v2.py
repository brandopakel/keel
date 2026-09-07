#!/usr/bin/env python3
"""Owned-process protocol 2 snapshot, reconnect, checkpoint and fallback checks."""
import argparse
import hashlib
import json
import os
import select
import socket
import threading
import time
from pathlib import Path

from validation_lib import Server, info, rewrite, sha256


class Link:
    """Local TCP relay: count downstream bytes and interrupt an actual transfer."""
    def __init__(self, target):
        self.target = target
        self.listener = socket.socket()
        self.listener.bind(('127.0.0.1', 0))
        self.listener.listen()
        self.listener.settimeout(.1)
        self.port = self.listener.getsockname()[1]
        self.stopped = threading.Event()
        self.offline = threading.Event()
        self.downstream = 0
        self.disconnect_after = 1 << 20
        self.interruptions = 0
        self.thread = threading.Thread(target=self.run, daemon=True)
        self.thread.start()

    def run(self):
        while not self.stopped.is_set():
            try:
                client, _ = self.listener.accept()
            except (OSError, TimeoutError):
                continue
            upstream = None
            try:
                if self.offline.is_set():
                    continue
                upstream = socket.create_connection(('127.0.0.1', self.target), timeout=2)
                client.settimeout(2)
                while not self.stopped.is_set() and not self.offline.is_set():
                    ready, _, _ = select.select([client, upstream], [], [], .05)
                    ended = False
                    for source in ready:
                        body = source.recv(65536)
                        if not body:
                            ended = True
                            break
                        if source is upstream:
                            self.downstream += len(body)
                            if self.disconnect_after and self.downstream >= self.disconnect_after:
                                self.disconnect_after = 0
                                self.interruptions += 1
                                ended = True
                                break
                        (client if source is upstream else upstream).sendall(body)
                    if ended:
                        break
            except (OSError, TimeoutError):
                pass
            finally:
                client.close()
                if upstream is not None:
                    upstream.close()

    def close(self):
        self.stopped.set()
        self.listener.close()
        self.thread.join(timeout=3)
        assert not self.thread.is_alive()


def caught_up(primary, replica, timeout=30):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        p, r = info(primary.client, 'replication'), info(replica.client, 'replication')
        if (r['replica_ready'] == '1' and p['primary_epoch'] == r['replica_epoch']
                and p['primary_offset'] == r['replica_offset']):
            return r
        time.sleep(.02)
    raise TimeoutError('protocol 2 did not catch up')


def dataset(client, keys):
    digest = hashlib.sha256()
    for n in range(keys):
        body = client.call('GET', f'bulk:{n}')
        assert body == bytes([65+n % 26]) * 65536, n
        digest.update(body)
    for parts in [('LRANGE', 'list', 0, -1), ('HGETALL', 'hash'), ('ZRANGE', 'ranking', 0, -1, 'WITHSCORES')]:
        row = client.call(*parts)
        if parts[0] == 'HGETALL':
            row = [piece for pair in sorted(zip(row[::2], row[1::2])) for piece in pair]
        digest.update(repr(row).encode())
    return digest.hexdigest()


def main(args):
    os.umask(0o077)
    root = args.out.resolve()
    root.mkdir(parents=True, exist_ok=False)
    report = {'status': 'running', 'binary_sha256': sha256(args.bin),
              'harness_sha256': sha256(__file__), 'dataset_value_bytes': args.keys * 65536,
              'protocol': 2, 'concurrent_append': args.concurrent, 'checks': {}}
    common = ['-replication-protocol', '2'] + (['-aof-concurrent-append'] if args.concurrent else [])
    primary = Server(args.bin, root/'primary', policy='always', async_append=True,
                     extra=['-replication-feed', *common])
    replica = link = None
    try:
        primary.start()
        for n in range(args.keys):
            assert primary.client.call('SET', f'bulk:{n}', bytes([65+n % 26]) * 65536) == b'OK'
        for n in range(2000):
            primary.client.call('RPUSH', 'list', f'member:{n}')
            primary.client.call('HSET', 'hash', f'f:{n}', n)
            primary.client.call('ZADD', 'ranking', n, f'm:{n}')
        link = Link(primary.port)
        replica = Server(args.bin, root/'replica', policy='everysec', async_append=True,
                         password=primary.password, extra=['-replicaof', f'127.0.0.1:{link.port}',
                         '-primary-password-env', 'KEEL_VALIDATION_PASSWORD', *common])
        started = time.monotonic()
        replica.start()
        caught_up(primary, replica)
        report['checks']['initial_snapshot'] = {'seconds': time.monotonic()-started,
            'downstream_bytes': link.downstream, 'interrupted_connections': link.interruptions}
        assert link.interruptions == 1, 'snapshot must survive a mid-response disconnect'
        assert dataset(primary.client, args.keys) == dataset(replica.client, args.keys)
        # Stop heartbeats before measuring only the incremental data transfer.
        link.offline.set()
        time.sleep(.25)
        before = link.downstream
        primary.client.call('LSET', 'list', 1000, 'changed')
        primary.client.call('HINCRBY', 'hash', 'f:1000', 7)
        primary.client.call('ZINCRBY', 'ranking', 3, 'm:19')
        link.offline.clear()
        caught_up(primary, replica)
        delta_bytes = link.downstream-before
        assert delta_bytes < 8192, delta_bytes
        report['checks']['large_collection_deltas'] = {'downstream_bytes': delta_bytes}
        assert dataset(primary.client, args.keys) == dataset(replica.client, args.keys)

        # Restart from an exact synced checkpoint; INCR must not be replayed twice.
        replica.stop(crash=True)
        primary.client.call('HINCRBY', 'hash', 'f:1000', 11)
        before = link.downstream
        started = time.monotonic()
        replica.start()
        r = caught_up(primary, replica)
        assert r['replica_checkpoint_resumed'] == 'true', r
        assert replica.client.call('HGET', 'hash', 'f:1000') == b'1018'
        assert link.downstream-before < 8192
        report['checks']['checkpoint_restart'] = {'seconds': time.monotonic()-started,
                                                 'downstream_bytes': link.downstream-before}
        rewrite(replica.client)
        caught_up(primary, replica)
        # Let the next heartbeat persist a checkpoint for the new file generation.
        time.sleep(.25)
        replica.stop(crash=True)
        replica.start()
        assert caught_up(primary, replica)['replica_checkpoint_resumed'] == 'true'
        report['checks']['rewrite_checkpoint_restart'] = True

        # Overflow the bounded history while the link is down; full sync is required.
        link.offline.set()
        time.sleep(.25)
        for _ in range(4200):
            primary.client.call('INCR', 'history-counter')
        for _ in range(260):
            primary.client.call('SET', 'history-fill', b'h' * 65536)
        link.offline.clear()
        before = link.downstream
        caught_up(primary, replica)
        assert replica.client.call('GET', 'history-counter') == b'4200'
        assert link.downstream-before > report['dataset_value_bytes']
        report['checks']['history_overrun_full_sync'] = True

        # A changed local log cannot resume from its old checkpoint.
        replica.stop(crash=True)
        with (root/'replica/store.aof').open('ab') as file:
            file.write(b'*3\r\n$3\r\nSET\r\n$6\r\npoison\r\n$1\r\nx\r\n')
        replica.start()
        assert caught_up(primary, replica)['replica_checkpoint_resumed'] == 'false'
        assert replica.client.call('GET', 'poison') is None
        report['checks']['changed_aof_full_sync'] = True

        old_epoch = info(primary.client, 'replication')['primary_epoch']
        primary.stop(crash=True)
        primary.start()
        assert info(primary.client, 'replication')['primary_epoch'] != old_epoch
        caught_up(primary, replica)
        assert dataset(primary.client, args.keys) == dataset(replica.client, args.keys)
        assert replica.client.call('GET', 'history-counter') == b'4200'
        report['checks']['primary_epoch_full_sync'] = True
        report['status'] = 'passed'
    except BaseException as exc:
        report['status'] = 'failed'
        report['failure'] = repr(exc)
        raise
    finally:
        if replica is not None: replica.stop(check=False)
        if link is not None: link.close()
        primary.stop(check=False)
        (root/'report.json').write_text(json.dumps(report, indent=2)+'\n')
        print(json.dumps(report, indent=2))


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bin', type=Path, required=True)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--keys', type=int, default=512)
    parser.add_argument('--concurrent', action='store_true')
    args = parser.parse_args()
    if not 160 <= args.keys <= 1024: parser.error('--keys must be 160..1024')
    main(args)
