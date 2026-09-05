#!/usr/bin/env python3
"""Exercise alpha.2 -> candidate AOF upgrade, rewrite, restart and backup rollback."""
import argparse
import json
import platform
import shutil
import subprocess
import time
from pathlib import Path

from validation_lib import Server, rewrite, sha256


def seed(c):
    c.call('SET', 'string', 'alpha2')
    c.call('SET', 'integer', '41')
    c.call('INCR', 'integer')
    c.call('SET', 'ttl', 'alive', 'PX', 3600000)
    c.call('SET', 'expired', 'gone', 'PX', 1)
    c.call('HSET', 'hash', 'field', 'value')
    c.call('RPUSH', 'list', *range(800))
    c.call('PEXPIRE', 'list', 3600000)
    c.call('SADD', 'set', 'a', 'b', 'c')
    c.call('ZADD', 'sorted', '1.25', 'a', '2.5', 'b')
    c.call('GEOADD', 'geo', '13.361389', '38.115556', 'palermo')
    c.call('PFADD', 'hll', 'a', 'b', 'c')
    c.call('BF.MADD', 'bloom', 'member')
    c.call('CF.ADD', 'cuckoo', 'member')
    c.call('CMS.INITBYDIM', 'cms', 100, 5)
    c.call('CMS.INCRBY', 'cms', 'item', 7)
    c.call('MORRIS.INITBYDIM', 'morris', 200, 5)
    c.call('MORRIS.INCRBY', 'morris', 'hits', 50000)
    time.sleep(.02)
    assert c.call('GET', 'expired') is None


def snapshot(c):
    queries = [
        ('GET', 'string'), ('GET', 'integer'), ('GET', 'ttl'), ('GET', 'expired'),
        ('HGETALL', 'hash'), ('LRANGE', 'list', 0, -1),
        ('ZRANGE', 'sorted', 0, -1, 'WITHSCORES'), ('GEOHASH', 'geo', 'palermo'),
        ('PFCOUNT', 'hll'), ('BF.EXISTS', 'bloom', 'member'),
        ('CF.EXISTS', 'cuckoo', 'member'), ('CMS.QUERY', 'cms', 'item'),
        ('MORRIS.QUERY', 'morris', 'hits'), ('DBSIZE',),
    ]
    result = {str(q): c.call(*q) for q in queries}
    result['set'] = sorted(c.call('SMEMBERS', 'set'))
    for key in ['hll', 'bloom', 'cuckoo', 'cms', 'morris']:
        result[key + ':dump'] = c.call('KEEL.DUMP', key)
    for key in ['ttl', 'list']:
        assert 0 < c.call('PTTL', key) <= 3600000, key
    return result


def check(args):
    root = Path(args.out).resolve()
    root.mkdir(parents=True, exist_ok=True)
    report = {
        'platform': platform.platform(),
        'baseline_sha256': sha256(args.baseline), 'candidate_sha256': sha256(args.candidate),
        'baseline_version': subprocess.check_output([args.baseline, '-version'], text=True).strip(),
        'candidate_version': subprocess.check_output([args.candidate, '-version'], text=True).strip(),
        'cases': [],
    }
    for policy in ['no', 'everysec', 'always']:
        for worker in [False, True]:
            name = f'{policy}-worker-{worker}'
            directory = root / name
            assert not directory.exists(), f'use a fresh output directory: {directory}'
            directory.mkdir()
            data = directory / 'data'
            with Server(args.baseline, data, policy=policy) as old:
                seed(old.client)
                expected = snapshot(old.client)
            backup = directory / 'rollback-alpha2.aof'
            shutil.copy2(data / 'store.aof', backup)
            backup_hash = sha256(backup)
            with Server(args.candidate, data, policy=policy, async_append=worker) as candidate:
                assert snapshot(candidate.client) == expected, 'upgrade changed persisted state'
                candidate.client.call('SET', 'string', 'alpha3')
                candidate.client.call('INCRBY', 'integer', 8)
                candidate.client.call('RPUSH', 'list', 'candidate')
                candidate.client.call('HSET', 'hash', 'field', 'candidate')
                rewrite(candidate.client)
                upgraded = snapshot(candidate.client)
            with Server(args.candidate, data, policy=policy, async_append=worker) as restarted:
                assert snapshot(restarted.client) == upgraded, 'rewrite/restart changed state'
            rollback = directory / 'rollback'
            rollback.mkdir()
            shutil.copy2(backup, rollback / 'store.aof')
            with Server(args.baseline, rollback, policy=policy) as old:
                assert snapshot(old.client) == expected, 'backup rollback changed state'
            assert sha256(backup) == backup_hash, 'rollback backup was mutated'
            report['cases'].append({'name': name, 'passed': True, 'backup_sha256': backup_hash})
            print(name, 'upgrade/rewrite/restart/backup rollback passed', flush=True)
    report['passed'] = True
    (root / 'report.json').write_text(json.dumps(report, indent=2) + '\n')


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--baseline', required=True)
    parser.add_argument('--candidate', required=True)
    parser.add_argument('--out', required=True)
    args = parser.parse_args()
    args.baseline = str(Path(args.baseline).resolve())
    args.candidate = str(Path(args.candidate).resolve())
    check(args)
