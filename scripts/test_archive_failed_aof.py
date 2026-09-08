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

    def test_existing_partial_archive_is_not_deleted(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'store.aof'
            path.write_bytes(b'original')
            partial = path.with_name('store.aof.gz.partial')
            partial.write_bytes(b'earlier failure evidence')
            with self.assertRaises(FileExistsError):
                archive.preserve(path)
            self.assertEqual(partial.read_bytes(), b'earlier failure evidence')
            self.assertEqual(path.read_bytes(), b'original')


    def test_restart_after_archive_publication(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'store.aof'
            path.write_bytes(b'original' * 1000)
            real_link = archive.os.link
            def interrupted_link(source, target):
                real_link(source, target)
                raise InterruptedError('stopped after archive publication')
            with patch.object(archive.os, 'link', side_effect=interrupted_link):
                with self.assertRaises(InterruptedError):
                    archive.preserve(path)
            result = archive.preserve(path)
            self.assertTrue(result['complete'])
            self.assertTrue(result['source_removed'])
            self.assertFalse(path.exists())

    def test_restart_after_each_sample_publication(self):
        for boundary in (1, 2):
            with self.subTest(boundary=boundary), tempfile.TemporaryDirectory() as directory:
                path = Path(directory) / 'store.aof'
                path.write_bytes(os.urandom(4096))
                # Reproduce the legacy boundary: a published sample without a report.
                take = 64
                body = path.read_bytes()
                for label, offset in [('prefix', 0), ('tail', len(body)-take)][:boundary]:
                    path.with_name('store.aof.'+label+'.gz').write_bytes(gzip.compress(body[offset:offset+take], mtime=0))
                result = archive.preserve(path, compressed_limit=0, sample_limit=take)
                self.assertFalse(result['complete'])
                self.assertEqual(len(result['samples']), 2)
                self.assertTrue(path.exists())

    def test_restart_after_source_removal(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'store.aof'
            path.write_bytes(b'original')
            real_unlink = Path.unlink
            def interrupted_unlink(target, *args, **kwargs):
                real_unlink(target, *args, **kwargs)
                if target == path:
                    raise InterruptedError('stopped after source removal')
            with patch.object(Path, 'unlink', new=interrupted_unlink):
                with self.assertRaises(InterruptedError):
                    archive.preserve(path)
            result = archive.preserve(path)
            self.assertTrue(result['complete'])
            self.assertTrue(result['source_removed'])


    def test_interrupted_journal_transitions_reconcile(self):
        for boundary in ('removal_pending', 'complete'):
            with self.subTest(boundary=boundary), tempfile.TemporaryDirectory() as directory:
                path = Path(directory) / 'store.aof'
                path.write_bytes(b'original')
                real_write = archive.write_report
                def interrupted_write(target, report):
                    if report.get('state') == boundary:
                        raise InterruptedError('stopped before journal transition')
                    real_write(target, report)
                with patch.object(archive, 'write_report', side_effect=interrupted_write):
                    with self.assertRaises(InterruptedError):
                        archive.preserve(path)
                # Discovery must work even after the source was removed.
                with patch('sys.argv', ['archive-failed-aof.py', directory]):
                    self.assertEqual(archive.main(), 0)
                result = json.loads(path.with_name('failed-aof.json').read_text())
                self.assertEqual(result['state'], 'complete')
                self.assertTrue(result['source_removed'])
                self.assertFalse(path.exists())

    def test_interrupted_sample_links_reuse_persisted_plan(self):
        for boundary in (1, 2):
            with self.subTest(boundary=boundary), tempfile.TemporaryDirectory() as directory:
                path = Path(directory) / 'store.aof'
                path.write_bytes(os.urandom(4096))
                real_link = archive.os.link
                published = 0
                def interrupted_link(source, target):
                    nonlocal published
                    real_link(source, target)
                    published += 1
                    if published == boundary:
                        raise InterruptedError('stopped after sample publication')
                with patch.object(archive.os, 'link', side_effect=interrupted_link):
                    with self.assertRaises(InterruptedError):
                        archive.preserve(path, compressed_limit=0, sample_limit=64)
                result = archive.preserve(path, compressed_limit=4096, sample_limit=128)
                self.assertEqual(result['sample_bytes'], 64)
                self.assertEqual(len(result['samples']), 2)
                self.assertTrue(path.exists())


if __name__ == '__main__':
    unittest.main()
