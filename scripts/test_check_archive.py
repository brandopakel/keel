"""Downloaded code must be authenticated even when Python assertions are disabled."""
import hashlib
import io
import platform
import subprocess
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path


class ArchiveIntegrityTest(unittest.TestCase):
    def test_checksum_guards_with_and_without_optimization(self):
        arch = {'x86_64': 'amd64', 'aarch64': 'arm64', 'arm64': 'arm64'}[platform.machine()]
        for optimize in (False, True):
            for failure in ('none', 'pin', 'sidecar_digest', 'sidecar_filename'):
                with self.subTest(optimize=optimize, failure=failure), tempfile.TemporaryDirectory() as tmp:
                    root = Path(tmp)
                    archive = root / f'keel_test_{platform.system().lower()}_{arch}.tar.gz'
                    with tarfile.open(archive, 'w:gz') as tar:
                        for name in ('keel', 'LICENSE', 'THIRD_PARTY_NOTICES.md', 'README.md',
                                     'docs/alpha-release-notes.md', 'examples/test_client.py'):
                            body = b'#!/bin/sh\necho "keel test (abcdef012345)"\n' if name == 'keel' else b'test\n'
                            member = tarfile.TarInfo(name)
                            member.size = len(body)
                            member.mode = 0o755 if name == 'keel' else 0o644
                            tar.addfile(member, io.BytesIO(body))
                    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
                    sidecar_digest = '0' * 64 if failure == 'sidecar_digest' else digest
                    sidecar_name = 'different.tar.gz' if failure == 'sidecar_filename' else archive.name
                    Path(str(archive) + '.sha256').write_text(f'{sidecar_digest}  {sidecar_name}\n')
                    pin = '0' * 64 if failure == 'pin' else digest
                    destination = root / 'extracted'
                    command = [sys.executable] + (['-O'] if optimize else []) + [
                        str(Path(__file__).with_name('check-archive.py')), '--archive', str(archive),
                        '--expected-sha256', pin, '--revision', 'abcdef012345', '--out', str(destination)]
                    result = subprocess.run(command, capture_output=True, text=True, timeout=10)
                    if failure == 'none':
                        self.assertEqual(result.returncode, 0, result.stderr)
                        self.assertTrue((destination / 'archive-check.json').exists())
                    else:
                        self.assertNotEqual(result.returncode, 0)
                        self.assertIn('independent digest pin' if failure == 'pin' else 'archive checksum mismatch', result.stderr)
                        self.assertFalse(destination.exists(), 'reject before extraction or execution')


if __name__ == '__main__':
    unittest.main()
