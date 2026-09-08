import gzip
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('archive_failed_aof', Path(__file__).with_name('archive-failed-aof.py'))
archive = importlib.util.module_from_spec(spec)
spec.loader.exec_module(archive)


class FailedAOFArchiveTests(unittest.TestCase):
    def test_complete_archive_is_verified_before_pruning(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'store.aof'
            body = b'*3\r\n$3\r\nSET\r\n' * 10000
            path.write_bytes(body)
            result = archive.preserve(path, compressed_limit=4096)
            self.assertTrue(result['complete'])
            self.assertTrue(result['source_removed'])
            self.assertFalse(path.exists())
            self.assertEqual(gzip.decompress((path.parent / result['artifact']).read_bytes()), body)
            self.assertEqual(result['sha256'], hashlib.sha256(body).hexdigest())
            self.assertEqual(json.loads((path.parent / 'failed-aof.json').read_text()), result)

    def test_output_limit_keeps_source_and_both_end_samples(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'store.aof'
            body = os.urandom(1 << 20)
            path.write_bytes(body)
            result = archive.preserve(path, compressed_limit=256, sample_limit=1024)
            self.assertFalse(result['complete'])
            self.assertFalse(result['source_removed'])
            self.assertEqual(path.read_bytes(), body)
            self.assertEqual(result['sha256'], hashlib.sha256(body).hexdigest())
            self.assertFalse(path.with_name('store.aof.gz.partial').exists())
            self.assertFalse(path.with_name('store.aof.gz').exists())
            self.assertEqual(len(result['samples']), 2)
            for sample in result['samples']:
                expected = body[sample['offset']:sample['offset']+sample['bytes']]
                self.assertEqual(gzip.decompress((path.parent / sample['artifact']).read_bytes()), expected)
            self.assertEqual(archive.preserve(path), result, 'an always-step retry preserves the existing partial evidence')

    def test_verification_failure_does_not_prune_source(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'store.aof'
            path.write_bytes(b'original')
            (path.parent / 'store.aof.gz').write_bytes(b'existing evidence')
            with self.assertRaises(FileExistsError):
                archive.preserve(path)
            self.assertEqual(path.read_bytes(), b'original')
            self.assertEqual((path.parent / 'store.aof.gz').read_bytes(), b'existing evidence')

    def test_low_free_space_leaves_original_with_bounded_samples(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'store.aof'
            body = os.urandom(1 << 20)
            path.write_bytes(body)
            with patch.object(archive.shutil, 'disk_usage', return_value=SimpleNamespace(free=2 << 20)):
                result = archive.preserve(path)
            self.assertFalse(result['complete'])
            self.assertEqual(result['compressed_limit_bytes'], 0)
            self.assertEqual(path.read_bytes(), body)
            self.assertLessEqual(sum((path.parent / s['artifact']).stat().st_size for s in result['samples']), 256 << 10)


if __name__ == '__main__':
    unittest.main()
