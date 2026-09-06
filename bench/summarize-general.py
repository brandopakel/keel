#!/usr/bin/env python3
"""Summarize complete general benchmark runs without pooling unlike workloads."""
import argparse
import hashlib
import json
from pathlib import Path
import statistics


def summarize(root):
    manifest = json.loads((root/'manifest.json').read_text())
    assert manifest['status'] == 'passed', 'a partial or failed matrix is not a passing comparison'
    groups = {}
    hashes = {}
    for run in manifest['runs']:
        path = root/run['name']/'report.json'
        data = path.read_bytes()
        hashes[str(path.relative_to(root))] = hashlib.sha256(data).hexdigest()
        report = json.loads(data)
        assert report['status'] == 'passed'
        assert not report['profiles_enabled'], 'profile runs are not comparative measurements'
        groups.setdefault(report['case']['name'], {}).setdefault(report['arm'], []).append(report)
    expected = manifest['arguments']['reps']
    results = {}
    for name, arms in groups.items():
        result = {}
        for arm, runs in arms.items():
            assert len(runs) == expected
            assert {r['repetition'] for r in runs} == set(range(1, expected+1))
            assert len({r['binary_sha256'] for r in runs}) == 1
            metrics = {'rss_mib': [r['rss_kib']['median']/1024 for r in runs]}
            if 'totals' in runs[0]:
                metrics.update(ops_per_second=[r['totals']['Ops/sec'] for r in runs],
                               p99_ms=[r['totals']['Percentile Latencies']['p99.00'] for r in runs],
                               p999_ms=[r['totals']['Percentile Latencies']['p99.90'] for r in runs])
            result[arm] = {key: {'median': statistics.median(values), 'min': min(values), 'max': max(values)}
                           for key, values in metrics.items()}
            result[arm]['client_cpu_warnings'] = sum(r.get('client_cpu_warning', False) for r in runs)
        if 'baseline' in arms and 'candidate' in arms:
            paired = []
            for base in arms['baseline']:
                candidate = next(r for r in arms['candidate'] if r['repetition'] == base['repetition'])
                assert candidate['dataset_sha256'] == base['dataset_sha256']
                if 'totals' in base:
                    paired.append(candidate['totals']['Ops/sec']/base['totals']['Ops/sec'])
            if paired:
                result['candidate_baseline_throughput_ratio'] = {
                    'median': statistics.median(paired), 'min': min(paired), 'max': max(paired), 'pairs': paired}
        results[name] = result
    summary = {'status': 'passed', 'source_manifest_sha256': hashlib.sha256((root/'manifest.json').read_bytes()).hexdigest(),
               'repetitions': expected, 'report_sha256': hashes, 'cases': results,
               'limits': 'Local sequential closed-loop measurements; medians and observed range, not confidence intervals. No aggregate speedup or deployment-capacity claim. RSS includes runtime and buffers; pipelining changes latency interpretation.'}
    (root/'summary.json').write_text(json.dumps(summary, indent=2)+'\n')
    rows = ['| Workload | Arm | Ops/s median (range) | p99 ms median | RSS MiB median |',
            '| --- | --- | ---: | ---: | ---: |']
    for name, arms in results.items():
        for arm, metrics in arms.items():
            if arm == 'candidate_baseline_throughput_ratio':
                continue
            ops = metrics.get('ops_per_second')
            ops_text = f"{ops['median']:,.0f} ({ops['min']:,.0f}–{ops['max']:,.0f})" if ops else '—'
            tail = f"{metrics['p99_ms']['median']:.3f}" if ops else '—'
            rows.append(f"| {name} | {arm} | {ops_text} | {tail} | {metrics['rss_mib']['median']:.2f} |")
    (root/'summary.md').write_text('\n'.join(rows)+'\n')
    print(root/'summary.md')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('root', type=Path)
    summarize(parser.parse_args().root)
