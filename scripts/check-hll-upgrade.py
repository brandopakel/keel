#!/usr/bin/env python3
"""Verify compact HLLs preserve legacy dumps, rollback and replica state."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil

from validation_lib import Server, rewrite, sha256
from soak import synchronized


def state(client):
    return {key: (client.call('KEEL.DUMP', key), client.call('PFCOUNT', key))
            for key in sorted(client.call('KEYS', '*'))}


def fill(client):
    for size in (0, 1, 32, 500, 600, 10000):
        key = f'hll:{size}'
        for first in range(0, max(1, size), 256):
            client.call('PFADD', key, *[f'{size}:{index}' for index in range(first, min(size, first+256))])
    client.call('PFMERGE', 'union', 'hll:32', 'hll:500')
    client.call('PEXPIREAT', 'hll:1', 4102444800000)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--baseline', required=True, type=Path)
    parser.add_argument('--candidate', required=True, type=Path)
    parser.add_argument('--out', required=True, type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    root = args.out.resolve()
    root.mkdir(parents=True, exist_ok=False)
    report = {'status': 'running', 'baseline_sha256': sha256(args.baseline),
              'candidate_sha256': sha256(args.candidate), 'harness_sha256': sha256(__file__),
              'rollback_restarts': 0}
    try:
        with Server(args.baseline, root/'store') as old:
            fill(old.client)
            expected = state(old.client)
            rewrite(old.client)
        shutil.copy2(root/'store/store.aof', root/'rollback-original.aof')
        report['original_aof_sha256'] = sha256(root/'rollback-original.aof')
        with Server(args.candidate, root/'store') as candidate:
            assert state(candidate.client) == expected, 'old dump/AOF changed after upgrade'
            assert candidate.client.call('PTTL', 'hll:1') > 0
            # Force small and boundary sketches through additional writes and merges.
            for key in expected:
                candidate.client.call('PFADD', key, *[f'new:{i}' for i in range(700)])
            candidate.client.call('PFMERGE', 'union', 'hll:1', 'hll:10000')
            expected = state(candidate.client)
            rewrite(candidate.client)
        for _ in range(2):
            with Server(args.baseline, root/'store') as rolled_back:
                assert state(rolled_back.client) == expected, 'new AOF cannot be read by old Keel'
                assert rolled_back.client.call('PTTL', 'hll:1') > 0
            report['rollback_restarts'] += 1
        primary = Server(args.candidate, root/'primary', async_append=True, extra=['-replication-feed'])
        replica = Server(args.candidate, root/'replica', async_append=True, password=primary.password,
                         extra=['-replicaof', f'127.0.0.1:{primary.port}',
                                '-primary-password-env', 'KEEL_VALIDATION_PASSWORD'])
        try:
            primary.start()
            fill(primary.client)
            replica.start()
            synchronized(primary, replica)
            expected = state(primary.client)
            assert state(replica.client) == expected
            primary.client.call('PFADD', 'hll:1', 'another')
            primary.client.call('PFMERGE', 'union', 'hll:600')
            synchronized(primary, replica)
            expected = state(primary.client)
            assert state(replica.client) == expected
            rewrite(replica.client)
            replica.stop(crash=True)
            replica.start()
            synchronized(primary, replica)
            assert state(replica.client) == expected
            report['replication_full_delta_restart'] = True
        finally:
            primary.stop(check=False)
            replica.stop(check=False)
        report.update(status='passed', final_dump_sha256={key.decode(): hashlib.sha256(value[0]).hexdigest()
                                                        for key,value in expected.items()})
    except BaseException as exc:
        report.update(status='failed', failure=repr(exc))
        raise
    finally:
        (root/'report.json').write_text(json.dumps(report, indent=2)+'\n')
    print(json.dumps(report, indent=2))


if __name__ == '__main__':
    main()
