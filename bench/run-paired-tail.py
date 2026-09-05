#!/usr/bin/env python3
"""Interleave three matched configurations on one host and retain every attempt."""
import argparse
import csv
import gzip
import hashlib
import importlib.util
import json
import math
import os
import platform
import subprocess
from pathlib import Path
from types import SimpleNamespace

spec = importlib.util.spec_from_file_location('tail', Path(__file__).with_name('run-tail.py'))
tail = importlib.util.module_from_spec(spec)
spec.loader.exec_module(tail)

parser = argparse.ArgumentParser()
parser.add_argument('--baseline', required=True)
parser.add_argument('--candidate', required=True)
parser.add_argument('--out', required=True)
parser.add_argument('--seconds', type=float, default=15)
parser.add_argument('--reps', type=int, default=5)
parser.add_argument('--start-rep', type=int, default=0)
parser.add_argument('--policies', nargs='+', choices=['off', 'everysec', 'always'],
                    default=['off', 'everysec', 'always'])
args = parser.parse_args()
if not math.isfinite(args.seconds) or args.seconds < 1 or args.reps < 1 or args.start_rep < 0:
    parser.error('positive repetitions and at least one second required')
root = Path(args.out).resolve()
assert not root.exists(), 'use a fresh result directory'
root.mkdir(parents=True)
binaries = {'baseline-sync': str(Path(args.baseline).resolve()),
            'candidate-sync': str(Path(args.candidate).resolve()),
            'candidate-worker': str(Path(args.candidate).resolve())}
metadata = {'platform': platform.platform(), 'cpus': os.cpu_count(),
            'python': platform.python_version(), 'seconds': args.seconds, 'reps': args.reps,
            'start_rep': args.start_rep, 'policies': args.policies,
            'harness_sha256': hashlib.sha256(Path(spec.origin).read_bytes()).hexdigest(),
            'driver_sha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
            'model': 'same-host loopback; fresh processes; rotated/reversed arm order; closed-loop load and 100Hz scheduled probe',
            'telemetry': 'ps RSS and lifetime-average CPU every 0.5s; GODEBUG=gctrace=1 in raw server logs for all arms',
            'binaries': {name: {'sha256': hashlib.sha256(Path(binary).read_bytes()).hexdigest(),
                                'buildinfo': subprocess.check_output(['go','version','-m',binary],text=True)}
                         for name,binary in binaries.items()}, 'order': []}
summaries = []
failures = 0
try:
    for rep in range(args.start_rep, args.start_rep + args.reps):
        order = list(binaries)
        order = order[rep % 3:] + order[:rep % 3]
        if rep % 2:
            order.reverse()
        for policy in args.policies:
            for arm in order:
                metadata['order'].append({'rep': rep, 'policy': policy, 'arm': arm})
                path = root / f'{arm}-{policy}-{rep}.csv.gz'
                opts = SimpleNamespace(bin=binaries[arm], out=str(path),
                                       async_append=arm == 'candidate-worker',
                                       seconds=args.seconds, members=10000, telemetry=True)
                try:
                    rows = tail.run(opts, policy, rep)
                except tail.RunFailure as exc:
                    rows = exc.rows or [[policy,rep,'setup','INIT',0,0,0,f'{type(exc).__name__}: {exc}']]
                    failures += 1
                except Exception as exc:
                    rows = [[policy,rep,'setup','INIT',0,0,0,f'{type(exc).__name__}: {exc}']]
                    failures += 1
                with gzip.open(path,'wt') as file:
                    writer = csv.writer(file)
                    writer.writerow(['policy','rep','role','command','scheduled_seconds','service_ms','scheduled_ms','error'])
                    writer.writerows(rows)
                for role in ['load','probe']:
                    selected = [row for row in rows if row[2] == role]
                    if not selected:
                        failures += 1
                        continue
                    values = sorted(row[6] for row in selected)
                    errors = sum(bool(row[7]) for row in selected)
                    failures += errors
                    summary = {'arm':arm, 'policy':policy, 'rep':rep, 'role':role,
                               'attempts':len(values), 'errors':errors,
                               'drops':sum(str(row[7]).startswith('dropped:') for row in selected),
                               'requests_per_second':len(values)/args.seconds,
                               'p99_ms':values[int((len(values)-1)*.99)],
                               'p999_ms':values[int((len(values)-1)*.999)],
                               'max_ms':values[-1]}
                    summaries.append(summary)
                    print(json.dumps(summary), flush=True)
finally:
    metadata['failures'] = failures
    (root/'metadata.json').write_text(json.dumps(metadata,indent=2)+'\n')
    (root/'summary.json').write_text(json.dumps(summaries,indent=2)+'\n')
if failures:
    raise SystemExit(f'{failures} failed/missing attempts; inspect retained raw results')
