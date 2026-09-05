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
parser.add_argument('--out', required=True)
args = parser.parse_args()
archive = Path(args.archive).resolve()
target_os, target_arch = archive.name.removesuffix('.tar.gz').rsplit('_', 2)[1:]
native_arch = {'x86_64': 'amd64', 'aarch64': 'arm64', 'arm64': 'arm64'}[platform.machine()]
assert (target_os, target_arch) == (platform.system().lower(), native_arch), 'native runner architecture mismatch'
digest = hashlib.sha256(archive.read_bytes()).hexdigest()
expected, filename = Path(str(archive) + '.sha256').read_text().split()
assert expected == digest and Path(filename).name == archive.name, 'archive checksum mismatch'
destination = Path(args.out).resolve()
assert not destination.exists(), 'extract to a fresh directory'
destination.mkdir(parents=True)
with tarfile.open(archive) as tar:
    names = set(tar.getnames())
    required = {'keel', 'LICENSE', 'THIRD_PARTY_NOTICES.md', 'README.md',
                'docs/alpha-release-notes.md', 'examples/test_client.py'}
    assert required <= names, required - names
    assert not any('__pycache__' in n or n.endswith('.pyc') for n in names)
    tar.extractall(destination, filter='data')
binary = destination / 'keel'
version = subprocess.check_output([str(binary), '-version'], text=True).strip()
if args.revision:
    assert args.revision[:12] in version and 'dirty' not in version, version
report = {'archive': archive.name, 'sha256': digest,
          'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(),
          'native_machine': platform.machine(), 'platform': platform.platform(),
          'version': version, 'files': len(names), 'passed': True}
(destination / 'archive-check.json').write_text(json.dumps(report, indent=2) + '\n')
print(json.dumps(report, indent=2))
