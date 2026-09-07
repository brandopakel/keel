#!/usr/bin/env python3
"""Export passed native candidate throughput and tail latency to Bencher BMF.

Latency is nanoseconds and throughput operations/second. Preserve repetitions,
workload and policy; never attach other binaries' results to a candidate commit.
Memory stays in the native evidence, whose RSS units are explicit.
"""
import argparse
import json
from pathlib import Path
import re


def export(roots):
    output, revisions, binaries = {}, set(), set()
    for root in roots:
        manifest = json.loads((root/'manifest.json').read_text())
        assert manifest['status'] == 'passed'
        assert not manifest['arguments']['profiles']
        binary = manifest['binaries']['candidate']
        revisions.add(re.search(r'vcs.revision=([0-9a-f]{40})', binary['build'])[1])
        binaries.add(binary['sha256'])
        for run in manifest['runs']:
            report = json.loads((root/run['name']/'report.json').read_text())
            if report['arm'] != 'candidate':
                continue
            assert report['status'] == 'passed' and report['binary_sha256'] == binary['sha256']
            if 'totals' not in report:
                continue
            prefix = f"general/v1/{report['policy']}/{'worker' if report['worker'] else 'sync'}/{report['case']['name']}/rep-{report['repetition']}"
            key = prefix+'/throughput'
            assert key not in output, 'duplicate workload/repetition'
            output[key] = {'throughput': {'value': report['totals']['Ops/sec']}}
            for label in ('p99.00', 'p99.90'):
                output[prefix+'/'+label] = {'latency': {'value': report['totals']['Percentile Latencies'][label]*1000000}}
    assert output and len(revisions) == 1 and len(binaries) == 1, 'one exact executable per report'
    return output, {'revision': next(iter(revisions)), 'binary_sha256': next(iter(binaries)),
                    'metric_count': len(output), 'source_directories': [str(root) for root in roots],
                    'limits': 'Local closed-loop diagnostic measurements; not hosted or dedicated-host execution.'}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('roots', nargs='+', type=Path)
    parser.add_argument('--out', required=True, type=Path)
    args = parser.parse_args()
    metrics, metadata = export(args.roots)
    args.out.write_text(json.dumps(metrics, indent=2, allow_nan=False)+'\n')
    args.out.with_suffix('.metadata.json').write_text(json.dumps(metadata, indent=2)+'\n')
