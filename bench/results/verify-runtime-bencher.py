#!/usr/bin/env python3
"""Reverify this historical hosted batch from the retained Jobs API evidence."""
import argparse,csv,gzip,hashlib,json,math,pathlib,re,statistics
parser=argparse.ArgumentParser(description=__doc__)
parser.add_argument('--runs',type=pathlib.Path,required=True,help='directory containing job JSON, reports and unpacked evidence')
parser.add_argument('--out',type=pathlib.Path,default=pathlib.Path(__file__).with_name('runtime-bencher-hosted-2026-09-06.json'))
args=parser.parse_args()
RUNS=args.runs.resolve()
POLICIES=['off','everysec','always']
ARMS=['baseline-sync','candidate-sync','candidate-worker','candidate-concurrent']
EXPECTED={'baseline-sync':('18e67eb3a25f8f81af397fe53a0945cd6df77ecf54f073fd93f74dc5894aaabb','16aaee8396894dc48a781cb66384f65f78477f5f'),**{arm:('dae193c6f10f9ebc23e7c5fe46fcb658e094e51b127bc16f8e72d5653912db07','6e581cdfeddb94ce8b426767ca1bd6145a4a7a45') for arm in ARMS[1:]}}
measurements=[]; jobs=[]; hosts=[]; rawfiles=[]; seen=set()
for rep in range(5):
 for policy in POLICIES:
  name=f'{policy}-{rep}'
  job=json.loads((RUNS/(name+'.job.json')).read_text())
  report=json.loads((RUNS/(name+'.stdout')).read_text())
  assert job['status']=='processed' and report['job']==job['uuid']
  root=RUNS/(name+'-evidence')/'evidence'
  host=json.loads((root/'host.json').read_text()); meta=json.loads((root/'paired/metadata.json').read_text()); summaries=json.loads((root/'paired/summary.json').read_text()); bmf=json.loads((root/'metrics.bmf.json').read_text())
  assert len(summaries)==8 and len(bmf)==56 and meta['failures']==0
  assert meta['seconds']==5 and meta['reps']==1 and meta['start_rep']==rep and meta['policies']==[policy]
  assert host['harness_revision']=='6e581cdfeddb94ce8b426767ca1bd6145a4a7a45'
  assert 'UP' in host['loopback']['after'][0]['flags']
  assert meta['binaries'].keys()==EXPECTED.keys()
  for arm,(digest,revision) in EXPECTED.items():
   binary=meta['binaries'][arm]
   assert binary['sha256']==digest and 'vcs.revision='+revision in binary['buildinfo']
   assert 'go1.27.1' in binary['buildinfo'] and 'CGO_ENABLED=0' in binary['buildinfo'] and 'vcs.modified=false' in binary['buildinfo']
  order=ARMS[rep%4:]+ARMS[:rep%4]
  if rep%2:order.reverse()
  assert [x['arm'] for x in meta['order']]==order
  for arm in ARMS:
   csvpath=root/'paired'/f'{arm}-{policy}-{rep}.csv.gz'
   with gzip.open(csvpath,'rt') as f: rows=list(csv.DictReader(f))
   assert rows and all(r['policy']==policy and int(r['rep'])==rep and r['role'] in ['load','probe'] for r in rows)
   samples=json.loads(pathlib.Path(str(csvpath)+f'.{policy}.{rep}.server.log.telemetry.json').read_text())
   assert len(samples)>=8 and all(s['rss_kib']>0 and math.isfinite(s['cpu_percent_lifetime']) for s in samples)
   log=pathlib.Path(str(csvpath)+f'.{policy}.{rep}.server.log').read_text()
   gc=re.findall(r'gc \d+ @[^\n]*?: ([0-9.eE+-]+)\+([0-9.eE+-]+)\+([0-9.eE+-]+) ms clock',log)
   assert gc,'missing GC trace'
   pauses=[float(x[0])+float(x[2]) for x in gc]
   for role in ['load','probe']:
    selected=[r for r in rows if r['role']==role]
    values=sorted(float(r['scheduled_ms']) for r in selected)
    assert values and all(math.isfinite(v) and v>=0 for v in values)
    derived={'attempts':len(values),'errors':sum(bool(r['error']) for r in selected),'drops':sum(r['error'].startswith('dropped:') for r in selected),'requests_per_second':len(values)/5,'p99_ms':values[int((len(values)-1)*.99)],'p999_ms':values[int((len(values)-1)*.999)],'max_ms':values[-1]}
    summary=next(s for s in summaries if s['arm']==arm and s['role']==role)
    assert derived['errors']==derived['drops']==0
    for k,v in derived.items():assert math.isclose(summary[k],v,rel_tol=1e-12,abs_tol=1e-12),(name,arm,role,k)
    prefix=f'{arm}/tail/{policy}/rep-{rep}/{role}/'
    for k in ['attempts','errors','drops','requests_per_second']:assert bmf[prefix+k]['count']['value']==derived[k]
    for k in ['p99_ms','p999_ms','max_ms']:assert math.isclose(bmf[prefix+k.removesuffix('_ms')]['latency']['value'],derived[k]*1e6,rel_tol=1e-12)
    measurements.append({**summary,'rss_peak_kib':max(s['rss_kib'] for s in samples),'cpu_percent_lifetime_median':statistics.median(s['cpu_percent_lifetime'] for s in samples),'telemetry_samples':len(samples),'gc_cycles_including_setup':len(gc),'gc_stw_max_ms_including_setup':max(pauses),'gc_stw_total_ms_including_setup':sum(pauses)})
    seen.add((rep,policy,arm,role))
   rawfiles.append({'job':job['uuid'],'path':'paired/'+csvpath.name,'sha256':hashlib.sha256(csvpath.read_bytes()).hexdigest(),'attempts':len(rows)})
  archive=root.parent/'evidence.tar.gz'
  jobs.append({'policy':policy,'rep':rep,'report':report['uuid'],'job':job['uuid'],'runner':job['runner'],'status':job['status'],'started':job['started'],'completed':job['completed'],'archive_sha256':hashlib.sha256(archive.read_bytes()).hexdigest(),'archive_bytes':archive.stat().st_size,'order':order})
  cpuinfo=(root/'proc-cpuinfo.txt').read_text()
  model=re.search(r'^model name\s*:\s*(.+)$',cpuinfo,re.M)
  hosts.append({'job':job['uuid'],'host':host,'cpu_model':model.group(1) if model else None,'spec':job['spec']})
assert len(seen)==120
assert len({j['job'] for j in jobs})==15 and len({j['report'] for j in jobs})==15
medians=[]
for policy in POLICIES:
 for arm in ARMS:
  for role in ['load','probe']:
   rows=[r for r in measurements if (r['policy'],r['arm'],r['role'])==(policy,arm,role)]
   fields=['requests_per_second','p99_ms','p999_ms','max_ms','rss_peak_kib']
   medians.append({'policy':policy,'arm':arm,'role':role,**{k:statistics.median(r[k] for r in rows) for k in fields}})
ratios=[]
for policy in POLICIES:
 for numerator,denominator in [('candidate-concurrent','candidate-worker'),('candidate-worker','candidate-sync'),('candidate-sync','baseline-sync')]:
  for role,field in [('load','requests_per_second'),('probe','p99_ms')]:
   values=[]
   for rep in range(5):
    pair={r['arm']:r[field] for r in measurements if (r['policy'],r['rep'],r['role'])==(policy,rep,role)}
    values.append(pair[numerator]/pair[denominator])
   ratios.append({'policy':policy,'numerator':numerator,'denominator':denominator,'role':role,'field':field,'values':values,'median':statistics.median(values),'min':min(values),'max':max(values)})
result={'project_url':'https://bencher.dev/perf/keel','image':'registry.bencher.dev/keel@sha256:b0f53ec3a75e8d91d478ae162f16f6f0fd93e2c6098fcbe44eaa83283e27ed4b','image_build_url':'https://github.com/brandopakel/keel/actions/runs/34080997192','harness_revision':'6e581cdfeddb94ce8b426767ca1bd6145a4a7a45','duration_per_arm_seconds':5,'repetitions_per_policy':5,'successful_jobs':len(jobs),'runs':60,'attempts':sum(r['attempts'] for r in measurements),'errors':0,'drops':0,'bmf_measurements':840,'binaries':meta['binaries'],'jobs':jobs,'hosts':hosts,'measurements':measurements,'medians':medians,'paired_ratios':ratios,'raw_files':rawfiles,'excluded_failed_jobs':[],'limits':['five-second arms; not sustained capacity or statistical proof of improvement','server and Python load generator share one four-vCPU Firecracker guest over loopback','on-demand Bencher runner; no reserved separate client/server hosts','AOF off disables worker/concurrent flags in all arms; those labels are repeat controls for off','ps CPU is lifetime average; RSS sampled every 0.5s; GC tracing enabled in every arm','GC summaries include setup and shutdown','all exact source revisions rebuilt using Go 1.27.1 and CGO_ENABLED=0; these are not byte-identical to release downloads','raw archives retained locally and available through authenticated project Jobs API']}
target=args.out
target.write_text(json.dumps(result,indent=2,allow_nan=False)+'\n')
print(json.dumps({k:result[k] for k in ['successful_jobs','runs','attempts','errors','drops','bmf_measurements']},indent=2))
print('MEDIANS',json.dumps([r for r in medians if r['role']=='load'],indent=2))
print('RATIOS',json.dumps(ratios,indent=2))
