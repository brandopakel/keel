import json
import importlib.util
import os
from pathlib import Path
import resource
import subprocess
import sys
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).with_name('run-local-validation.py')


def permission_fixture_parent():
    # A nested resource guard must not see a fixture made unreadable on purpose.
    enclosing = os.environ.get('KEEL_LOCAL_VALIDATION_ROOT')
    return Path(enclosing).parent if enclosing else None



class LocalValidationTests(unittest.TestCase):
    def invoke(self, root, code, *flags):
        return subprocess.run([sys.executable, str(SCRIPT), '--out', str(root),
            '--seconds', '3', '--max-output-mib', '2', '--max-file-mib', '1',
            '--min-free-gib', '2', *flags, '--', sys.executable, '-c', code, '{out}'],
            text=True, capture_output=True, timeout=8)

    def test_success_preserves_evidence(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            result = self.invoke(root, "from pathlib import Path; import sys; (Path(sys.argv[1])/'result.txt').write_text('verified')")
            self.assertEqual(result.returncode, 0, result.stderr+result.stdout)
            self.assertEqual((root/'result.txt').read_text(), 'verified')
            self.assertEqual(json.loads((root/'local-resource-report.json').read_text())['status'], 'passed')

    def test_timeout_stops_owned_descendant(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            result = self.invoke(root, "import subprocess,sys,time; from pathlib import Path; p=subprocess.Popen([sys.executable,'-c','import time; time.sleep(60)']); (Path(sys.argv[1])/'child.pid').write_text(str(p.pid)); time.sleep(60)", '--seconds', '.5')
            self.assertEqual(result.returncode, 1)
            report = json.loads((root/'local-resource-report.json').read_text())
            self.assertIn('time budget', report['failure'])
            pid = int((root/'child.pid').read_text())
            status = subprocess.run(['ps', '-o', 'stat=', '-p', str(pid)], text=True, capture_output=True).stdout.strip()
            self.assertTrue(not status or status.startswith('Z'), status)

    def test_file_limit_refuses_growth(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            result = self.invoke(root, "import sys; from pathlib import Path; (Path(sys.argv[1])/'large').write_bytes(b'x'*(2<<20))")
            self.assertEqual(result.returncode, 1)
            self.assertLessEqual((root/'large').stat().st_size, 1<<20)
            self.assertEqual(json.loads((root/'local-resource-report.json').read_text())['status'], 'failed')

    def test_aggregate_limit_and_existing_evidence(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            result = self.invoke(root, "import sys,time; from pathlib import Path; [(Path(sys.argv[1])/str(i)).write_bytes(b'x'*(1<<20)) for i in range(3)]; time.sleep(60)")
            self.assertEqual(result.returncode, 1)
            self.assertIn('output budget', json.loads((root/'local-resource-report.json').read_text())['failure'])
            # A second invocation must not overwrite the earlier failure.
            original = (root/'local-resource-report.json').read_bytes()
            result = self.invoke(root, 'pass')
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual((root/'local-resource-report.json').read_bytes(), original)

    def test_long_local_run_refused_before_launch(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            result = self.invoke(root, 'pass', '--seconds', '121')
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(root.exists())

    def test_inherited_soft_file_limit_is_not_raised(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            previous = resource.getrlimit(resource.RLIMIT_FSIZE)
            try:
                resource.setrlimit(resource.RLIMIT_FSIZE, (1<<20, previous[1]))
                result = self.invoke(root, "import sys; from pathlib import Path; (Path(sys.argv[1])/'large').write_bytes(b'x'*(2<<20))",
                                     '--max-output-mib', '3', '--max-file-mib', '2')
            finally:
                resource.setrlimit(resource.RLIMIT_FSIZE, previous)
            self.assertEqual(result.returncode, 1)
            report = json.loads((root/'local-resource-report.json').read_text())
            self.assertEqual(report['effective_file_limit_bytes'], 1<<20)
            self.assertLessEqual((root/'large').stat().st_size, 1<<20)

    def test_final_write_after_last_sample_cannot_pass(self):
        spec = importlib.util.spec_from_file_location('local_guard', SCRIPT)
        guard = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(guard)
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            code = "import sys; from pathlib import Path; p=Path(sys.argv[1]); [(p/str(i)).write_bytes(b'x'*(1<<20)) for i in range(3)]; (p/'finished').touch()"
            args = SimpleNamespace(out=root, seconds=3, max_output_mib=2, max_file_mib=1,
                min_free_gib=2, command=[sys.executable, '-c', code, '{out}'])
            actual_size = guard.directory_bytes
            sampled = False
            def delayed_first_sample(path):
                nonlocal sampled
                if not sampled:
                    sampled = True
                    deadline = time.monotonic()+2
                    while not (root/'finished').exists():
                        if time.monotonic() >= deadline:
                            raise TimeoutError('test writer did not finish')
                        time.sleep(.01)
                    # Model a scan taken before the writer completed, returned
                    # after its last write and immediately before poll sees exit.
                    time.sleep(.05)
                    return 0
                return actual_size(path)
            with patch.object(guard, 'directory_bytes', delayed_first_sample):
                self.assertEqual(guard.run(args), 1)
            report = json.loads((root/'local-resource-report.json').read_text())
            self.assertEqual(report['command_exit_code'], 0)
            self.assertIn('output budget', report.get('failure', '')+report.get('cleanup_failure', ''))
            self.assertGreater(report['peak_output_bytes'], 2<<20)

    @unittest.skipIf(os.geteuid() == 0, 'root can traverse mode-000 directories')
    def test_unreadable_output_directory_cannot_pass(self):
        # Keep this deliberately unreadable 3 MiB fixture outside an enclosing
        # wrapper's monitored root, so only the guard under test sees it.
        with tempfile.TemporaryDirectory(dir=permission_fixture_parent(), prefix='keel-unreadable-guard-') as temp:
            root = Path(temp)/'run'
            try:
                result = self.invoke(root, "from pathlib import Path; import sys; p=Path(sys.argv[1])/'hidden'; p.mkdir(); [(p/str(i)).write_bytes(b'x'*(1<<20)) for i in range(3)]; p.chmod(0)")
                self.assertEqual(result.returncode, 1)
                report = json.loads((root/'local-resource-report.json').read_text())
                self.assertEqual(report['status'], 'failed')
                self.assertIn('PermissionError', report.get('failure', '')+report.get('cleanup_failure', ''))
            finally:
                if (root/'hidden').exists():
                    (root/'hidden').chmod(0o700)

    def test_free_space_reserves_final_report_before_launch(self):
        spec = importlib.util.spec_from_file_location('free_guard', SCRIPT)
        guard = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(guard)
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            args = SimpleNamespace(out=root, seconds=3, max_output_mib=2, max_file_mib=1,
                min_free_gib=2, command=[sys.executable, '-c', 'pass'])
            with patch.object(guard.shutil, 'disk_usage', return_value=SimpleNamespace(free=(2<<30)+guard.REPORT_RESERVE_BYTES-1)):
                with self.assertRaisesRegex(ValueError, 'insufficient free disk'):
                    guard.run(args)
            self.assertFalse(root.exists())

    def test_sampled_free_space_includes_report_reserve(self):
        spec = importlib.util.spec_from_file_location('sample_guard', SCRIPT)
        guard = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(guard)
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            args = SimpleNamespace(out=root, seconds=3, max_output_mib=2, max_file_mib=1,
                min_free_gib=2, command=[sys.executable, '-c', 'pass'])
            checks = 0
            def disk_usage(path):
                nonlocal checks
                checks += 1
                return SimpleNamespace(free=(3<<30) if checks == 1 else (2<<30)+guard.REPORT_RESERVE_BYTES-1)
            with patch.object(guard.shutil, 'disk_usage', disk_usage):
                self.assertEqual(guard.run(args), 1)
            self.assertGreaterEqual(checks, 2)
            report = json.loads((root/'local-resource-report.json').read_text())
            self.assertIn('minimum free-space reserve', report['failure'])

    @unittest.skipIf(os.geteuid() == 0, 'root can traverse mode-000 directories')
    def test_unreadable_root_preserves_failure_report(self):
        with tempfile.TemporaryDirectory(dir=permission_fixture_parent()) as temp:
            root = Path(temp)/'run'
            try:
                result = self.invoke(root, "from pathlib import Path; import sys; Path(sys.argv[1]).chmod(0)")
                self.assertEqual(result.returncode, 1, result.stdout+result.stderr)
                self.assertEqual(json.loads((root/'local-resource-report.json').read_text())['status'], 'failed')
            finally:
                if root.exists():
                    root.chmod(0o700)

    def test_command_created_report_directory_uses_fallback(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            result = self.invoke(root, "from pathlib import Path; import sys; (Path(sys.argv[1])/'local-resource-report.json').mkdir()")
            self.assertEqual(result.returncode, 1, result.stdout+result.stderr)
            report = json.loads(result.stdout)
            fallback = Path(report['fallback_report_path'])
            self.assertEqual(fallback.parent, root.parent)
            self.assertEqual(json.loads(fallback.read_text())['status'], 'failed')
            self.assertTrue((root/'local-resource-report.json').is_dir())

    def test_failure_prunes_go_temporary_builds_and_keeps_other_evidence(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            result = self.invoke(root, "from pathlib import Path; import os,sys; (Path(os.environ['GOTMPDIR'])/'build.a').write_bytes(b'x'*(1<<20)); (Path(os.environ['TMPDIR'])/'failure.txt').write_text('recovery evidence'); sys.exit(2)")
            self.assertEqual(result.returncode, 1, result.stdout+result.stderr)
            self.assertFalse((root/'go-cache').exists())
            self.assertFalse((root/'go-tmp').exists())
            self.assertEqual((root/'tmp/failure.txt').read_text(), 'recovery evidence')
            report = json.loads((root/'local-resource-report.json').read_text())
            self.assertTrue(report['go_cache_pruned'])
            self.assertTrue(report['go_temporary_files_pruned'])
            self.assertEqual(report['status'], 'failed')

    def test_directory_removed_during_scan_preserves_other_usage(self):
        spec = importlib.util.spec_from_file_location('churn_guard', SCRIPT)
        guard = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(guard)
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            disappearing = root/'temporary'
            disappearing.mkdir()
            (root/'retained').write_bytes(b'x'*4096)
            original_scandir = guard.os.scandir
            def concurrent_cleanup(path):
                if Path(path) == disappearing:
                    disappearing.rmdir()
                return original_scandir(path)
            with patch.object(guard.os, 'scandir', concurrent_cleanup):
                self.assertEqual(guard.directory_bytes(root), 4096)
            self.assertFalse(disappearing.exists())

    def test_exited_parent_does_not_leave_owned_child(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            result = self.invoke(root, "import subprocess,sys; from pathlib import Path; p=subprocess.Popen([sys.executable,'-c','import time; time.sleep(60)']); (Path(sys.argv[1])/'child.pid').write_text(str(p.pid))")
            self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
            pid = int((root/'child.pid').read_text())
            status = subprocess.run(['ps', '-o', 'stat=', '-p', str(pid)], text=True, capture_output=True).stdout.strip()
            self.assertTrue(not status or status.startswith('Z'), status)

    def test_report_headroom_is_included_in_budget(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            result = self.invoke(root, "from pathlib import Path; import sys; (Path(sys.argv[1])/'payload').write_bytes(b'x'*((1<<20)-1))", '--max-output-mib', '1')
            self.assertEqual(result.returncode, 1)
            report = json.loads((root/'local-resource-report.json').read_text())
            self.assertEqual(report['status'], 'failed')
            self.assertIn('output budget', report.get('failure', '')+report.get('cleanup_failure', ''))


if __name__ == '__main__':
    unittest.main()
