#!/usr/bin/env python3
"""Read-only status: a live PID alone does not establish workload progress."""
import argparse
import datetime
import json
import math
from pathlib import Path
import subprocess
import time


def classify(report, terminal, alive, age, stale_seconds):
    status = report.get('status', 'unknown')
    if status == 'passed':
        return 'passed' if terminal and report.get('passed') is True else 'invalid_pass_report'
    if status == 'running':
        if not alive:
            return 'interrupted_or_missing_process'
        if age > stale_seconds:
            return 'stalled_or_stale_progress'
    return status


def inspect(manifest, stale_seconds=120):
    launch = json.loads(Path(manifest).read_text())
    rows = []
    now = time.time()
    for run in launch['runs']:
        folder = Path(run['output'])
        terminal = (folder/'report.json').exists()
        path = folder/('report.json' if terminal else 'progress.json')
        row = {'run': run['name'], 'report_path': str(path), 'pid': run['pid']}
        try:
            report = json.loads(path.read_text())
            age = max(0, now-path.stat().st_mtime)
            process = subprocess.run(['ps', '-o', 'lstart=,command=', '-p', str(run['pid'])],
                                     capture_output=True, text=True, timeout=2, check=False)
            actual = process.stdout.strip()
            expected = run.get('pid_identity_after_exec', run.get('pid_identity'))
            alive = bool(actual) and bool(expected) and actual == expected
            row.update(status=classify(report, terminal, alive, age, stale_seconds),
                       reported_status=report.get('status'), owned_process_alive=alive,
                       progress_age_seconds=round(age, 3), failure=report.get('failure'))
            for field in ('acknowledged_writes', 'primary_crash_recoveries',
                          'replica_crash_recoveries', 'checkpoint_count', 'binary_sha256'):
                row[field] = report.get(field)
        except (OSError, ValueError, subprocess.SubprocessError) as exc:
            row.update(status='status_unavailable', error=repr(exc))
        rows.append(row)
    return {'observed_utc': datetime.datetime.now(datetime.timezone.utc).isoformat(),
            'source_revision': launch.get('source_revision'), 'stale_seconds': stale_seconds,
            'runs': rows}


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--manifest', type=Path, required=True)
    parser.add_argument('--stale-seconds', type=float, default=120)
    args = parser.parse_args()
    if not math.isfinite(args.stale_seconds) or args.stale_seconds <= 0:
        parser.error('stale-seconds must be finite and positive')
    print(json.dumps(inspect(args.manifest, args.stale_seconds), indent=2))
