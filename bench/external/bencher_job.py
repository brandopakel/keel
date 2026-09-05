#!/usr/bin/env python3
"""Run one matched triplet offline; retain raw evidence in Bencher job stderr."""
import argparse
import base64
import hashlib
import io
import json
import math
import os
import platform
import subprocess
import sys
import tarfile
import tempfile
from pathlib import Path


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--baseline', default='/opt/keel/bin/keel-alpha2')
    parser.add_argument('--candidate', default='/opt/keel/bin/keel-alpha3')
    parser.add_argument('--policy', choices=['off', 'everysec', 'always'], required=True)
    parser.add_argument('--rep', type=int, required=True)
    parser.add_argument('--seconds', type=float, default=5)
    parser.add_argument('--enable-loopback', action='store_true',
                        help='bring up only lo inside the owned offline Linux benchmark microVM')
    args = parser.parse_args()
    if args.rep < 0 or not math.isfinite(args.seconds) or not 1 <= args.seconds <= 30:
        parser.error('nonnegative repetition and finite duration from 1 to 30 seconds required')
    loopback = None
    if args.enable_loopback:
        if sys.platform != 'linux':
            parser.error('--enable-loopback is only supported inside the Linux benchmark image')
        before = json.loads(subprocess.check_output(['ip', '-json', 'link', 'show', 'dev', 'lo'], text=True))
        if 'UP' not in before[0]['flags']:
            subprocess.run(['ip', 'link', 'set', 'dev', 'lo', 'up'], check=True)
        after = json.loads(subprocess.check_output(['ip', '-json', 'link', 'show', 'dev', 'lo'], text=True))
        if 'UP' not in after[0]['flags']:
            raise RuntimeError('loopback remains down')
        loopback = {'before': before, 'after': after}
    driver = Path(__file__).resolve().parents[1] / 'run-paired-tail.py'
    with tempfile.TemporaryDirectory(prefix='keel-hosted-') as tmp:
        root = Path(tmp)
        metadata = {'platform': platform.platform(), 'python': platform.python_version(),
                    'cpu_count': os.cpu_count(), 'arguments': vars(args),
                    'harness_revision': os.environ.get('KEEL_HARNESS_REVISION'), 'loopback': loopback,
                    'placement': 'server and load generator share one execution environment over loopback TCP; provider job metadata identifies the sandbox',
                    'job_script_sha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest()}
        for name in ['cpuinfo', 'meminfo', 'mounts', 'pressure/cpu', 'pressure/io']:
            path = Path('/proc') / name
            if path.exists():
                (root / ('proc-' + name.replace('/', '-') + '.txt')).write_text(path.read_text())
        packages = Path('/opt/keel-packages.txt')
        if packages.exists():
            (root / 'packages.txt').write_text(packages.read_text())
        metadata['filesystem'] = subprocess.check_output(['df', '-T', tmp], text=True) if sys.platform == 'linux' else subprocess.check_output(['df', tmp], text=True)
        (root / 'host.json').write_text(json.dumps(metadata, indent=2) + '\n')
        with (root / 'driver.log').open('w') as log:
            result = subprocess.run([sys.executable, str(driver), '--baseline', args.baseline,
                                     '--candidate', args.candidate, '--out', str(root / 'paired'),
                                     '--seconds', str(args.seconds), '--reps', '1',
                                     '--start-rep', str(args.rep), '--policies', args.policy],
                                    stdout=log, stderr=subprocess.STDOUT)
        bmf = {}
        summary_path = root / 'paired' / 'summary.json'
        if summary_path.exists():
            rows = json.loads(summary_path.read_text())
            for row in rows:
                prefix = f"{row['arm']}/tail/{row['policy']}/rep-{row['rep']}/{row['role']}"
                for field in ['attempts', 'errors', 'drops', 'requests_per_second']:
                    bmf[prefix + '/' + field] = {'count': {'value': row[field]}}
                for field in ['p99_ms', 'p999_ms', 'max_ms']:
                    bmf[prefix + '/' + field.removesuffix('_ms')] = {'latency': {'value': row[field] * 1_000_000}}
        (root / 'metrics.bmf.json').write_text(json.dumps(bmf, allow_nan=False) + '\n')
        # Bencher jobs have no external network. A checksum-protected archive in
        # stderr preserves all raw attempts, logs and telemetry through the Jobs API.
        buffer = io.BytesIO()
        with tarfile.open(fileobj=buffer, mode='w:gz') as archive:
            archive.add(root, arcname='evidence', filter=lambda member: member if member.isfile() or member.isdir() else None)
        payload = buffer.getvalue()
        encoded = base64.b64encode(payload).decode('ascii')
        if len(encoded) > 8 * 1024 * 1024:
            raise RuntimeError('raw evidence exceeds the conservative output budget; shorten each triplet')
        print(json.dumps({'keel_evidence_v1': {'sha256': hashlib.sha256(payload).hexdigest(),
                                             'bytes': len(payload), 'tar_gzip_base64': encoded}}),
              file=sys.stderr, flush=True)
        print(json.dumps(bmf, allow_nan=False), flush=True)
        if result.returncode or not bmf:
            raise SystemExit(result.returncode or 1)


if __name__ == '__main__':
    main()
