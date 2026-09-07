#!/usr/bin/env python3
"""Two lagging replicas, larger snapshots, history overflow and durable restart."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import time

from validation_lib import Server, info, rewrite, sha256

spec = importlib.util.spec_from_file_location('replication_validation', Path(__file__).with_name('check-replication-v2.py'))
validation = importlib.util.module_from_spec(spec)
spec.loader.exec_module(validation)


def verify(server, versions, counter):
    digest = hashlib.sha256()
    for index, version in enumerate(versions):
        body = server.client.call('GET', f'large:{index}')
        assert body == bytes([65+version % 26])*65536, (index, version)
        digest.update(body)
    assert server.client.call('GET', 'counter') == str(counter).encode()
    return digest.hexdigest()


def main(args):
    root = args.out.resolve()
    root.mkdir(parents=True, exist_ok=False)
    report = dict(status='running', binary_sha256=sha256(args.bin), harness_sha256=sha256(__file__),
                  dataset_value_bytes=args.keys*65536, replicas=2, protocol=2, concurrent=True, phases=[])
    flags = ['-replication-protocol', '2', '-aof-concurrent-append']
    primary = Server(args.bin, root/'primary', policy='always', async_append=True,
                     startup_timeout=180, extra=['-replication-feed', *flags])
    replicas, links = [], []
    versions = [index % 26 for index in range(args.keys)]
    counter = 0
    def phase(name, began, before):
        rows = []
        for replica, link, start_bytes in zip(replicas, links, before):
            state = validation.caught_up(primary, replica, timeout=180)
            rows.append(dict(replica=replica.directory.name, state=state,
                             observed_caught_up_seconds=time.monotonic()-began,
                             transferred_bytes=link.downstream-start_bytes))
        for replica, row in zip(replicas, rows):
            row['digest'] = verify(replica, versions, counter)
        expected = verify(primary, versions, counter)
        assert all(row['digest'] == expected for row in rows)
        report['phases'].append(dict(name=name, phase_seconds_including_verification=time.monotonic()-began, replicas=rows,
                                    primary=info(primary.client, 'replication')))
        (root/'progress.json').write_text(json.dumps(report, indent=2)+'\n')
        print(name, 'passed', flush=True)
        return rows
    try:
        primary.start()
        for index, version in enumerate(versions):
            assert primary.client.call('SET', f'large:{index}', bytes([65+version])*65536) == b'OK'
        assert primary.client.call('SET', 'counter', 0) == b'OK'
        for index in range(2):
            link = validation.Link(primary.port)
            link.disconnect_after = (index+1)*1048576
            links.append(link)
            replicas.append(Server(args.bin, root/f'replica-{index}', policy='always', async_append=True,
                                   password=primary.password, startup_timeout=180,
                                   extra=['-replicaof', f'127.0.0.1:{link.port}',
                                          '-primary-password-env', 'KEEL_VALIDATION_PASSWORD', *flags]))
        began = time.monotonic()
        for replica in replicas: replica.start()
        phase('initial_interrupted_snapshots', began, [0, 0])
        assert all(link.interruptions == 1 for link in links)

        for link in links: link.offline.set()
        time.sleep(.25)
        before = [link.downstream for link in links]
        # 32 MiB of canonical writes exceeds the shared 16 MiB history window.
        for index in range(512):
            slot = index % args.keys
            versions[slot] = (versions[slot]+7) % 26
            assert primary.client.call('SET', f'large:{slot}', bytes([65+versions[slot]])*65536) == b'OK'
            counter += 1
            assert primary.client.call('INCR', 'counter') == counter
        rewrite(primary.client)
        began = time.monotonic()
        for link in links: link.offline.clear()
        rows = phase('two_history_overrun_recoveries_after_rewrite', began, before)
        assert all(row['transferred_bytes'] > args.keys*65536 for row in rows), 'expected full snapshot transfer for both replicas'

        # Both replicas restart from durable checkpoints, then apply one INCR
        # exactly once. No full snapshot should be needed inside retained history.
        time.sleep(.25)
        for replica in replicas: replica.stop(crash=True)
        counter += 1
        assert primary.client.call('INCR', 'counter') == counter
        before = [link.downstream for link in links]
        began = time.monotonic()
        for replica in replicas: replica.start()
        rows = phase('two_checkpoint_restarts', began, before)
        assert all(row['state']['replica_checkpoint_resumed'] == 'true' for row in rows)
        assert all(row['transferred_bytes'] < 1<<20 for row in rows)

        before = [link.downstream for link in links]
        old_epoch = info(primary.client, 'replication')['primary_epoch']
        began = time.monotonic()
        primary.stop(crash=True)
        primary.start()
        assert info(primary.client, 'replication')['primary_epoch'] != old_epoch
        phase('primary_restart_and_two_epoch_recoveries', began, before)
        report['acknowledged_counter'] = counter
        report['status'] = 'passed'
    except BaseException as exc:
        report['status'], report['failure'] = 'failed', repr(exc)
        raise
    finally:
        for replica in replicas: replica.stop(check=False)
        for link in links: link.close()
        primary.stop(check=False)
        (root/'report.json').write_text(json.dumps(report, indent=2)+'\n')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bin', type=Path, required=True)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--keys', type=int, default=2048)
    args = parser.parse_args()
    if not 160 <= args.keys <= 4096: parser.error('--keys must be 160..4096')
    os.umask(0o077)
    main(args)
