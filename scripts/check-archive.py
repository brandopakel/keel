#!/usr/bin/env python3
"""Verify an archive and execute its native binary before release publication."""
import argparse
import hashlib
import json
import platform
import subprocess
import tarfile
from pathlib import Path

parser = argparse.ArgumentParser()
parser.add_argument('--archive', required=True)
parser.add_argument('--revision')
parser.add_argument('--expected-sha256', help='independently pinned digest for a downloaded release')
parser.add_argument('--out', required=True)
args = parser.parse_args()
archive = Path(args.archive).resolve()
target_os, target_arch = archive.name.removesuffix('.tar.gz').rsplit('_', 2)[1:]
native_arch = {'x86_64': 'amd64', 'aarch64': 'arm64', 'arm64': 'arm64'}[platform.machine()]
if (target_os, target_arch) != (platform.system().lower(), native_arch):
    raise RuntimeError('native runner architecture mismatch')
digest = hashlib.sha256(archive.read_bytes()).hexdigest()
if args.expected_sha256 and digest != args.expected_sha256:
    raise RuntimeError('archive does not match independent digest pin')
expected, filename = Path(str(archive) + '.sha256').read_text().split()
if expected != digest or Path(filename).name != archive.name:
    raise RuntimeError('archive checksum mismatch')
destination = Path(args.out).resolve()
if destination.exists():
    raise FileExistsError('extract to a fresh directory')
destination.mkdir(parents=True)
with tarfile.open(archive) as tar:
    names = set(tar.getnames())
    required = {'keel', 'LICENSE', 'THIRD_PARTY_NOTICES.md', 'README.md',
                'docs/alpha-release-notes.md', 'examples/test_client.py'}
    if not required <= names:
        raise RuntimeError(f'missing archive files: {sorted(required - names)}')
    if any('__pycache__' in n or n.endswith('.pyc') for n in names):
        raise RuntimeError('archive contains Python cache files')
    tar.extractall(destination, filter='data')
binary = destination / 'keel'
version = subprocess.check_output([str(binary), '-version'], text=True).strip()
if args.revision and (args.revision[:12] not in version or 'dirty' in version):
    raise RuntimeError(f'archive version mismatch: {version}')
report = {'archive': archive.name, 'sha256': digest,
          'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(),
          'native_machine': platform.machine(), 'platform': platform.platform(),
          'version': version, 'files': len(names), 'passed': True}
(destination / 'archive-check.json').write_text(json.dumps(report, indent=2) + '\n')
print(json.dumps(report, indent=2))
