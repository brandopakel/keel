"""Corrupt or unsafe provider output must fail before writing extracted files."""
import base64
import hashlib
import importlib.util
import io
import json
import tarfile
import tempfile
import unittest
from pathlib import Path

spec = importlib.util.spec_from_file_location('evidence', Path(__file__).with_name('bencher-evidence.py'))
evidence = importlib.util.module_from_spec(spec)
spec.loader.exec_module(evidence)


def envelope(names=('evidence/result.json',), kind=tarfile.REGTYPE):
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode='w:gz') as archive:
        for name in names:
            member = tarfile.TarInfo(name)
            member.type = kind
            if kind == tarfile.REGTYPE:
                payload = b'{"passed": true}\n'
                member.size = len(payload)
                archive.addfile(member, io.BytesIO(payload))
            else:
                member.linkname = '/outside'
                archive.addfile(member)
    raw = buffer.getvalue()
    return {'keel_evidence_v1': {'sha256': hashlib.sha256(raw).hexdigest(),
                                 'bytes': len(raw),
                                 'tar_gzip_base64': base64.b64encode(raw).decode()}}


class EvidenceTests(unittest.TestCase):
    def reject(self, document):
        with tempfile.TemporaryDirectory() as tmp:
            destination = Path(tmp) / 'unpacked'
            with self.assertRaises(ValueError):
                evidence.unpack(document, destination)
            self.assertFalse(destination.exists())

    def test_jobs_api_roundtrip_and_fresh_destination(self):
        document = {'output': {'results': [{'stderr': 'diagnostic\n' + json.dumps(envelope())}]}}
        with tempfile.TemporaryDirectory() as tmp:
            destination = Path(tmp) / 'unpacked'
            evidence.unpack(document, destination)
            self.assertTrue(json.loads((destination / 'evidence/result.json').read_text())['passed'])
            self.assertTrue((destination / 'evidence.tar.gz').is_file())
            with self.assertRaises(FileExistsError):
                evidence.unpack(document, destination)

    def test_checksum_and_length(self):
        for field, value in [('sha256', '0' * 64), ('bytes', 0)]:
            with self.subTest(field=field):
                document = envelope()
                document['keel_evidence_v1'][field] = value
                self.reject(document)

    def test_traversal_and_absolute_paths(self):
        for name in ['../outside', '/outside', 'evidence/../outside', 'other/file']:
            with self.subTest(name=name):
                self.reject(envelope([name]))

    def test_links_and_special_files(self):
        for kind in [tarfile.SYMTYPE, tarfile.LNKTYPE, tarfile.FIFOTYPE, tarfile.CHRTYPE]:
            with self.subTest(kind=kind):
                self.reject(envelope(kind=kind))

    def test_member_count(self):
        self.reject(envelope([f'evidence/{n}' for n in range(1001)]))

    def test_missing_or_duplicate_envelope(self):
        self.reject({'output': None})
        self.reject({'output': {'results': [{'stderr': (json.dumps(envelope()) + '\n') * 2}]}})


if __name__ == '__main__':
    unittest.main()
