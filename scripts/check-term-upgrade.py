#!/usr/bin/env python3
"""Mixed-version protocol-2 recovery and nonzero-term restart checks."""
import argparse
import json
import os
import time
from pathlib import Path

from validation_lib import Server, info, sha256


def caught_up(primary, replica):
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        p, r = info(primary.client, 'replication'), info(replica.client, 'replication')
        if (r['replica_ready'] == '1' and p['primary_epoch'] == r['replica_epoch']
                and p['primary_offset'] == r['replica_offset']):
            return
        time.sleep(.02)
    raise TimeoutError('mixed-version protocol 2 did not catch up')


def rejected(client, *parts, contains):
    try:
        client.call(*parts)
    except RuntimeError as exc:
        assert contains in str(exc), str(exc)
    else:
        raise AssertionError(f'{parts[0]} unexpectedly succeeded')


def pair(root, primary_bin, replica_bin, *, terms=False):
    common = ['-replication-protocol', '2', '-aof-concurrent-append']
    primary = Server(primary_bin, root/'primary', async_append=True,
                     extra=['-replication-feed', *common])
    replica = None
    try:
        primary.start()
        if terms:
            assert primary.client.call('KEEL.PROMOTE', 1) == b'OK'
        for n in range(128):
            assert primary.client.call('SET', f'key:{n}', f'value:{n}') == b'OK'
        replica = Server(replica_bin, root/'replica', async_append=True,
                         password=primary.password,
                         extra=['-replicaof', f'127.0.0.1:{primary.port}',
                                '-primary-password-env', 'KEEL_VALIDATION_PASSWORD', *common])
        replica.start()
        caught_up(primary, replica)
        for n in range(128):
            assert replica.client.call('GET', f'key:{n}') == f'value:{n}'.encode()
        replica.stop(crash=True)
        assert primary.client.call('INCR', 'counter') == 1
        replica.start()
        caught_up(primary, replica)
        assert replica.client.call('GET', 'counter') == b'1'
        if terms:
            assert primary.client.call('KEEL.FENCE', 2) == b'OK'
            rejected(primary.client, 'SET', 'forbidden', 'value', contains='FENCED')
        primary.stop(crash=True)
        primary.start()
        if terms:
            rejected(primary.client, 'SET', 'forbidden', 'value', contains='FENCED')
            rejected(primary.client, 'KEEL.PROMOTE', 2, contains='not above')
            assert primary.client.call('KEEL.PROMOTE', 3) == b'OK'
        assert primary.client.call('INCR', 'counter') == 2
        caught_up(primary, replica)
        assert replica.client.call('GET', 'counter') == b'2'
        assert replica.client.call('GET', 'forbidden') is None
        return {'initial_snapshot_keys': 128, 'replica_crash_recovery': True,
                'primary_crash_recovery': True, 'nonzero_term_regrant': terms}
    finally:
        if replica is not None:
            replica.stop(check=False)
        primary.stop(check=False)


def main(args):
    os.umask(0o077)
    args.out.mkdir(parents=True, exist_ok=False)
    report = {'status': 'running', 'baseline_sha256': sha256(args.baseline),
              'candidate_sha256': sha256(args.candidate), 'harness_sha256': sha256(__file__),
              'checks': {}}
    try:
        for name, primary, replica in [
            ('old-primary-new-replica', args.baseline, args.candidate),
            ('new-primary-old-replica', args.candidate, args.baseline),
        ]:
            report['checks'][name] = pair(args.out/name, primary, replica)
        report['checks']['nonzero-term'] = pair(args.out/'nonzero-term',
                                                args.candidate, args.candidate, terms=True)
        report['status'] = 'passed'
    except BaseException as exc:
        report['status'] = 'failed'
        report['error'] = f'{type(exc).__name__}: {exc}'
        raise
    finally:
        (args.out/'report.json').write_text(json.dumps(report, indent=2) + '\n')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--baseline', type=Path, required=True)
    parser.add_argument('--candidate', type=Path, required=True)
    parser.add_argument('--out', type=Path, required=True)
    main(parser.parse_args())
