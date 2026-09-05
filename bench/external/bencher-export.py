#!/usr/bin/env python3
"""Export tail CSV to Bencher Metric Format; preserve repetitions separately.
Bencher's built-in latency unit is nanoseconds, not the input's milliseconds.
"""
import argparse, csv, gzip, json, math
from collections import defaultdict
from pathlib import Path

def export(path):
    groups=defaultdict(list)
    with (gzip.open(path,'rt') if str(path).endswith('.gz') else open(path)) as stream:
        for row in csv.DictReader(stream):
            value=float(row['scheduled_ms'])
            if not math.isfinite(value) or value<0: raise ValueError('invalid latency')
            # The harness emits error text, with only the empty string meaning
            # success. This is not a boolean/numeric flag column.
            groups[(row['policy'],row['rep'],row['role'])].append((value,bool(row['error'])))
    if not groups: raise ValueError('empty benchmark')
    output={}
    for (policy,rep,role),rows in groups.items():
        values=sorted(v for v,_ in rows); prefix=f'tail/{policy}/rep-{rep}/{role}'
        output[prefix+'/attempts']={'count':{'value':len(rows)}}
        output[prefix+'/errors']={'count':{'value':sum(e for _,e in rows)}}
        for name,q in [('p50',.5),('p95',.95),('p99',.99),('p999',.999)]:
            output[prefix+'/'+name]={'latency':{'value':values[int((len(values)-1)*q)]*1_000_000}}
    return output

if __name__=='__main__':
    parser=argparse.ArgumentParser();parser.add_argument('csv');parser.add_argument('--out',required=True);args=parser.parse_args()
    Path(args.out).write_text(json.dumps(export(args.csv),indent=2,allow_nan=False)+'\n')
