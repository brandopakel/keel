#!/usr/bin/env python3
"""Compare barrier/concurrent appends in one exact binary; profile separately."""
import argparse
import json
from pathlib import Path
import subprocess
import sys


def run(args):
    args.out.mkdir(parents=True, exist_ok=False)
    manifest = dict(status='running', method='Same exact binary, matched fsync policy and workloads; baseline uses the worker barrier, candidate permits ordered concurrent appends. Fresh processes and rotated repeated arms. Profiles are separate candidate-only diagnostic runs, excluded from comparisons.', arguments={k:str(v) if isinstance(v,Path) else v for k,v in vars(args).items()})
    common = [sys.executable, str(Path(__file__).with_name('run-general.py')), '--candidate', str(args.binary), '--memtier', str(args.memtier), '--worker', '--load-threads', '2']
    if args.server_cpus: common += ['--server-cpus',args.server_cpus]
    if args.client_cpus: common += ['--client-cpus',args.client_cpus]
    try:
        for policy in args.policies.split(','):
            if policy not in ('no','everysec','always'): raise ValueError('unknown fsync policy')
            directory=args.out/policy; directory.mkdir()
            paired=common+['--policy',policy,'--baseline',str(args.binary),'--candidate-concurrent','--cases',args.cases,'--reps',str(args.repetitions),'--seconds',str(args.seconds),'--out',str(directory/'matched')]
            subprocess.run(paired,check=True)
            subprocess.run([sys.executable,str(Path(__file__).with_name('summarize-general.py')),str(directory/'matched')],check=True)
            for mode in ('barrier','concurrent'):
                profile=common+['--policy',policy,'--profiles','--cases','cache-write-64','--reps','1','--seconds',str(args.profile_seconds),'--out',str(directory/f'profile-{mode}')]
                if mode=='concurrent':profile+=['--candidate-concurrent']
                subprocess.run(profile,check=True)
                for sample in (directory/f'profile-{mode}').rglob('cpu.pprof'):
                    with sample.with_name('cpu-top.log').open('w') as out:
                        subprocess.run(['go','tool','pprof','-top','-nodecount=35',str(args.binary),str(sample)],stdout=out,stderr=subprocess.STDOUT,check=True)
        manifest['status']='completed'
    except BaseException as exc:
        manifest['status'],manifest['failure']='failed',repr(exc)
        raise
    finally:(args.out/'manifest.json').write_text(json.dumps(manifest,indent=2)+'\n')


if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--binary',type=Path,required=True)
    p.add_argument('--memtier',type=Path,required=True)
    p.add_argument('--out',type=Path,required=True)
    p.add_argument('--server-cpus')
    p.add_argument('--client-cpus')
    p.add_argument('--policies',default='no,everysec,always')
    p.add_argument('--cases',default='cache-balanced-64,cache-write-64,pipeline-16,hash')
    p.add_argument('--seconds',type=int,default=10)
    p.add_argument('--repetitions',type=int,default=3)
    p.add_argument('--profile-seconds',type=int,default=15)
    a=p.parse_args()
    if not (1<=a.seconds<=60 and 1<=a.repetitions<=10 and 1<=a.profile_seconds<=60):p.error('invalid duration/repetition limits')
    for key in ('binary','memtier','out'):setattr(a,key,getattr(a,key).resolve())
    run(a)
