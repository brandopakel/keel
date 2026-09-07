#!/usr/bin/env python3
"""Recover copies of a failed soak's AOFs, retaining original failure evidence."""
import argparse
import base64
import hashlib
import json
import os
import shutil
import subprocess
import sys
import time
from pathlib import Path
from validation_lib import Server, info, sha256


def state(client):
    keys = client.call('KEYS', '*')
    assert len(keys) <= 10000, 'this diagnostic expects the bounded soak fixture'
    out = {}
    for key in keys:
        kind = client.call('TYPE', key)
        if kind == b'string': value = client.call('GET', key)
        elif kind == b'list': value = client.call('LRANGE', key, 0, -1)
        elif kind == b'set': value = sorted(client.call('SMEMBERS', key))
        elif kind == b'zset': value = client.call('ZRANGE', key, 0, -1, 'WITHSCORES')
        elif kind == b'hash':
            raw = client.call('HGETALL', key)
            assert len(raw) % 2 == 0
            value = sorted(zip(raw[::2], raw[1::2]))
        else: raise AssertionError(f'unexpected fixture type {kind!r}')
        out[key] = (kind, value)
    return out


def digest(data):
    def normalize(v):
        if isinstance(v, bytes): return {'bytes': base64.b64encode(v).decode()}
        if isinstance(v, (tuple,list)): return [normalize(x) for x in v]
        return v
    body = json.dumps([(normalize(k),normalize(v)) for k,v in sorted(data.items())], separators=(',',':')).encode()
    return hashlib.sha256(body).hexdigest()


def clone(source, target):
    target.parent.mkdir(parents=True, exist_ok=False)
    if sys.platform == 'darwin': subprocess.run(['/bin/cp','-c',str(source),str(target)], check=True)
    else: shutil.copyfile(source,target)


def main(args):
    os.umask(0o077)
    root=args.out.resolve();root.mkdir(parents=True,exist_ok=False)
    failure=json.loads((args.failed/'report.json').read_text())
    report={'status':'running','original_status':failure['status'],'original_failure':failure.get('failure'),
            'acknowledged_writes':failure['acknowledged_writes'],
            'baseline_sha256':sha256(args.baseline),'candidate_sha256':sha256(args.candidate),'runs':{}}
    datasets={}
    try:
        for name,source,binary in [('original-primary','primary',args.baseline),('original-replica','replica',args.baseline),('candidate-replica','replica',args.candidate)]:
            src=args.failed/source/'store.aof';directory=root/name
            clone(src,directory/'store.aof')
            row={'input_bytes':src.stat().st_size,'input_sha256':sha256(src)};report['runs'][name]=row
            # An explicit 60s diagnostic deadline measures recovery beyond the
            # failed soak's 10s SLO; it does not turn that failed test into a pass.
            server=Server(binary,directory,startup_timeout=60)
            started=time.monotonic()
            try:
                server.start();row['startup_seconds']=time.monotonic()-started
                datasets[name]=state(server.client);row['state_sha256']=digest(datasets[name])
                if name=='candidate-replica':
                    deadline=time.monotonic()+30
                    while int(info(server.client,'persistence')['aof_rewrites'])<1:
                        if time.monotonic()>deadline: raise TimeoutError('candidate did not compact recovered log')
                        time.sleep(.02)
                    row['rewritten_bytes']=(directory/'store.aof').stat().st_size
                    assert row['rewritten_bytes'] < row['input_bytes']/10
                    assert state(server.client)==datasets[name]
            finally:server.stop(check=False)
            if name=='candidate-replica':
                started=time.monotonic()
                with Server(args.baseline,directory,startup_timeout=60) as rollback:
                    row['rollback_restart_seconds']=time.monotonic()-started
                    assert state(rollback.client)==datasets[name]
        n=failure['acknowledged_writes']
        for index in range(max(0,n-1000),n):
            key=f'cache:{index%1000}'.encode()
            assert datasets['original-primary'][key]==(b'string',str(index).encode()+b':'+b'v'*256)
        report['primary_acknowledged_cache_values_lost']=0
        assert datasets['candidate-replica']==datasets['original-replica']
        primary,replica=datasets['original-primary'],datasets['original-replica']
        report['replica_differing_keys_before_resync']=[k.decode() for k in sorted(set(primary)|set(replica)) if primary.get(k)!=replica.get(k)]
        report['status']='passed'
    except BaseException as exc:
        report['status']='failed';report['failure']=repr(exc);raise
    finally:
        (root/'report.json').write_text(json.dumps(report,indent=2)+'\n')
        print(json.dumps(report,indent=2))

if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__)
    for name in ['baseline','candidate','failed','out']:p.add_argument('--'+name,type=Path,required=True)
    main(p.parse_args())
