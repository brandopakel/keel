import json
import importlib.util
import os
from pathlib import Path
import resource
import signal
import subprocess
import sys
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).with_name('run-local-validation.py')


def load_guard(name):
    spec = importlib.util.spec_from_file_location(name, SCRIPT)
    guard = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(guard)
    return guard


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

    def assertLimitHit(self, result, root, limit, flag, confidence='confirmed'):
        # A wrapper limit is a harness outcome, kept apart from command failure.
        self.assertEqual(result.returncode, 3 if confidence != 'possible' else 1,
                         result.stdout+result.stderr)
        report = json.loads((root/'local-resource-report.json').read_text())
        self.assertEqual(report['status'], 'failed')
        hit = report['limit_hit']
        self.assertEqual((hit['limit'], hit['flag']), (limit, flag), hit)
        if isinstance(confidence, tuple):
            self.assertIn(hit['confidence'], confidence)
        else:
            self.assertEqual(hit['confidence'], confidence)
        last = result.stderr.strip().splitlines()[-1]
        self.assertTrue(last.startswith('run-local-validation: '), last)
        self.assertIn(flag, last)
        return report

    def test_success_preserves_evidence(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            result = self.invoke(root, "from pathlib import Path; import sys; (Path(sys.argv[1])/'result.txt').write_text('verified')")
            self.assertEqual(result.returncode, 0, result.stderr+result.stdout)
            self.assertEqual((root/'result.txt').read_text(), 'verified')
            report = json.loads((root/'local-resource-report.json').read_text())
            self.assertEqual(report['status'], 'passed')
            self.assertNotIn('limit_hit', report)
            self.assertGreaterEqual(report['peak_file_bytes'], len('verified'))
            self.assertEqual(result.stderr, '')

    def test_timeout_stops_owned_descendant(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            result = self.invoke(root, "import subprocess,sys,time; from pathlib import Path; p=subprocess.Popen([sys.executable,'-c','import time; time.sleep(60)']); (Path(sys.argv[1])/'child.pid').write_text(str(p.pid)); time.sleep(60)", '--seconds', '.5')
            report = self.assertLimitHit(result, root, 'time', '--seconds')
            self.assertIn('time budget', report['failure'])
            pid = int((root/'child.pid').read_text())
            status = subprocess.run(['ps', '-o', 'stat=', '-p', str(pid)], text=True, capture_output=True).stdout.strip()
            self.assertTrue(not status or status.startswith('Z'), status)

    def test_file_limit_refuses_growth(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            result = self.invoke(root, "import sys; from pathlib import Path; (Path(sys.argv[1])/'large').write_bytes(b'x'*(2<<20))")
            self.assertLessEqual((root/'large').stat().st_size, 1<<20)
            report = self.assertLimitHit(result, root, 'file_size', '--max-file-mib')
            self.assertEqual(report['peak_file'], 'large')
            self.assertIn('large reached', report['limit_hit']['evidence'][0])

    def test_aggregate_limit_and_existing_evidence(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            result = self.invoke(root, "import sys,time; from pathlib import Path; [(Path(sys.argv[1])/str(i)).write_bytes(b'x'*(1<<20)) for i in range(3)]; time.sleep(60)")
            report = self.assertLimitHit(result, root, 'output', '--max-output-mib')
            self.assertIn('output budget', report['failure'])
            self.assertIn('usage by entry: ', report['limit_hit']['evidence'][1])
            self.assertIn('0 1.0 MiB', report['limit_hit']['evidence'][1])
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
            report = self.assertLimitHit(result, root, 'file_size', '--max-file-mib')
            self.assertEqual(report['effective_file_limit_bytes'], 1<<20)
            self.assertIn('ulimit -f', report['limit_hit']['advice'])
            self.assertLessEqual((root/'large').stat().st_size, 1<<20)

    def test_final_write_after_last_sample_cannot_pass(self):
        guard = load_guard('local_guard')
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            code = "import sys; from pathlib import Path; p=Path(sys.argv[1]); [(p/str(i)).write_bytes(b'x'*(1<<20)) for i in range(3)]; (p/'finished').touch()"
            args = SimpleNamespace(out=root, seconds=3, max_output_mib=2, max_file_mib=1,
                min_free_gib=2, command=[sys.executable, '-c', code, '{out}'])
            actual_scan = guard.scan_output
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
                    return 0, 0, None
                return actual_scan(path)
            with patch.object(guard, 'scan_output', delayed_first_sample):
                self.assertEqual(guard.run(args), 3)
            report = json.loads((root/'local-resource-report.json').read_text())
            self.assertEqual(report['command_exit_code'], 0)
            self.assertIn('output budget', report.get('failure', '')+report.get('cleanup_failure', ''))
            self.assertEqual(report['limit_hit']['limit'], 'output')
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
        guard = load_guard('free_guard')
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            args = SimpleNamespace(out=root, seconds=3, max_output_mib=2, max_file_mib=1,
                min_free_gib=2, command=[sys.executable, '-c', 'pass'])
            with patch.object(guard.shutil, 'disk_usage', return_value=SimpleNamespace(free=(2<<30)+guard.REPORT_RESERVE_BYTES-1)):
                with self.assertRaisesRegex(ValueError, 'insufficient free disk.*--min-free-gib'):
                    guard.run(args)
            self.assertFalse(root.exists())

    def test_sampled_free_space_includes_report_reserve(self):
        guard = load_guard('sample_guard')
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
                self.assertEqual(guard.run(args), 3)
            self.assertGreaterEqual(checks, 2)
            report = json.loads((root/'local-resource-report.json').read_text())
            self.assertIn('minimum free-space reserve', report['failure'])
            self.assertEqual((report['limit_hit']['limit'], report['limit_hit']['flag']),
                             ('free_space', '--min-free-gib'))

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
            result = self.invoke(root, "from pathlib import Path; import os,sys; (Path(os.environ['GOTMPDIR'])/'build.a').write_bytes(b'x'*((1<<20)-1)); (Path(os.environ['TMPDIR'])/'failure.txt').write_text('recovery evidence'); sys.exit(2)")
            self.assertEqual(result.returncode, 1, result.stdout+result.stderr)
            self.assertFalse((root/'go-cache').exists())
            self.assertFalse((root/'go-tmp').exists())
            self.assertEqual((root/'tmp/failure.txt').read_text(), 'recovery evidence')
            report = json.loads((root/'local-resource-report.json').read_text())
            self.assertTrue(report['go_cache_pruned'])
            self.assertTrue(report['go_temporary_files_pruned'])
            self.assertEqual(report['status'], 'failed')
            self.assertNotIn('limit_hit', report)

    def test_directory_removed_during_scan_preserves_other_usage(self):
        guard = load_guard('churn_guard')
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
            report = self.assertLimitHit(result, root, 'output', '--max-output-mib')
            self.assertIn('output budget', report.get('failure', '')+report.get('cleanup_failure', ''))

    def test_sigxfsz_child_is_reported_as_file_limit(self):
        # Unlike Go and Python, most C programs keep SIGXFSZ's default action
        # and are killed by it. Model one, writing to the captured log.
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            # A write that crosses the ceiling is shortened; the next one, at
            # the ceiling, raises the signal.
            result = self.invoke(root, "import os,signal; signal.signal(signal.SIGXFSZ, signal.SIG_DFL)\nwhile True: os.write(1, b'x'*(1<<20))")
            report = self.assertLimitHit(result, root, 'file_size', '--max-file-mib')
            self.assertEqual(report['command_exit_code'], -signal.SIGXFSZ)
            self.assertIn('SIGXFSZ', report['limit_hit']['evidence'][0])
            self.assertLessEqual((root/'command.log').stat().st_size, 1<<20)

    def test_passing_run_with_a_file_at_the_ceiling_warns(self):
        # The kernel shortens the write that crosses the ceiling without an
        # error; a command that ignores the short count still exits zero.
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            result = self.invoke(root, "import os; os.write(1, b'x'*(2<<20))")
            self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
            report = json.loads((root/'local-resource-report.json').read_text())
            self.assertEqual(report['status'], 'passed')
            self.assertIn('command.log reached', report['file_limit_warning'])
            self.assertIn('--max-file-mib', result.stderr)

    def test_removed_file_that_hit_the_limit_is_still_named(self):
        # An abandoned AOF rewrite deletes the file that reached the ceiling,
        # then logs EFBIG. The run must not read as a product failure.
        code = ("import os,sys,time; from pathlib import Path; p=Path(sys.argv[1])/'tmp'/'store.aof.rewrite'; "
                "fd=os.open(p, os.O_WRONLY|os.O_CREAT); os.write(fd, b'x'*(600<<10)); time.sleep(.8)\n"
                "try:\n while True: os.write(fd, b'x'*(600<<10))\n"
                "except OSError as e:\n os.close(fd); p.unlink(); print(f'rewrite abandoned: write {p}: {e.strerror.lower()}'); sys.exit(1)")
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            result = self.invoke(root, code)
            report = self.assertLimitHit(result, root, 'file_size', '--max-file-mib', ('confirmed', 'likely'))
            self.assertFalse((root/'tmp'/'store.aof.rewrite').exists())
            self.assertEqual(report['peak_file'], 'tmp/store.aof.rewrite')
            self.assertTrue(any('file too large' in line for line in report['limit_hit']['evidence']), report)

    def test_shell_status_for_sigxfsz_names_file_limit(self):
        # A shell reports a child killed by SIGXFSZ as 128+25 and goes on.
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            result = self.invoke(root, f"import sys; sys.exit({128+signal.SIGXFSZ})")
            report = self.assertLimitHit(result, root, 'file_size', '--max-file-mib', 'likely')
            self.assertIn('SIGXFSZ', report['limit_hit']['evidence'][0])

    def test_file_limit_before_a_hang_is_kept_beside_the_time_limit(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            result = self.invoke(root, "import time; print('write store.aof: file too large', flush=True); time.sleep(60)",
                                 '--seconds', '.6')
            report = self.assertLimitHit(result, root, 'time', '--seconds')
            self.assertIn('--max-file-mib', report['limit_hit']['evidence'][-1])
            self.assertIn('file too large', report['limit_hit']['evidence'][-1])

    def test_interrupted_run_is_not_a_limit_hit(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            wrapper = subprocess.Popen([sys.executable, str(SCRIPT), '--out', str(root), '--seconds', '8',
                '--max-output-mib', '2', '--max-file-mib', '1', '--min-free-gib', '2', '--',
                sys.executable, '-c', "import sys,time; from pathlib import Path; (Path(sys.argv[1])/'full').write_bytes(b'x'*(1<<20)); time.sleep(60)",
                '{out}'], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            deadline = time.monotonic()+5
            while not ((root/'full').exists() and (root/'full').stat().st_size == 1<<20):
                self.assertLess(time.monotonic(), deadline)
                time.sleep(.05)
            time.sleep(.4)  # Let a sample see the file at the ceiling.
            wrapper.send_signal(signal.SIGINT)
            stdout, stderr = wrapper.communicate(timeout=10)
            self.assertEqual(wrapper.returncode, 1, stdout+stderr)
            report = json.loads((root/'local-resource-report.json').read_text())
            self.assertIn('InterruptedError', report['failure'])
            self.assertNotIn('limit_hit', report)

    def test_unrelated_file_size_error_is_only_possible(self):
        # A test may impose a far smaller limit of its own. Name the wrapper's
        # limit as a possibility without claiming it was the cause.
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            result = self.invoke(root, "import sys; print('OSError: [Errno 27] File too large'); sys.exit(1)")
            report = self.assertLimitHit(result, root, 'file_size', '--max-file-mib', 'possible')
            self.assertIn('File too large', report['limit_hit']['evidence'][0])

    def test_passing_command_that_mentions_file_errors_passes(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)/'run'
            result = self.invoke(root, "print('expected: write failed: file too large')")
            self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
            self.assertNotIn('limit_hit', json.loads((root/'local-resource-report.json').read_text()))

    def test_signature_scan_is_chunked_and_spans_chunk_edges(self):
        guard = load_guard('signature_guard')
        with tempfile.TemporaryDirectory() as temp:
            log = Path(temp)/'command.log'
            # No newline anywhere, and the message straddles the 1 MiB read.
            log.write_bytes(b'x'*((1<<20)-5) + b'File too large' + b'y'*(2<<20))
            lines = guard.signature_lines(log)
            self.assertEqual(len(lines), 1)
            self.assertIn('File too large', lines[0])
            self.assertLessEqual(len(lines[0]), 400)
            log.write_bytes(b'ok\nsignal: file size limit exceeded\n' * 10)
            self.assertEqual(guard.signature_lines(log), ['signal: file size limit exceeded']*3)

    def test_help_names_every_limit_flag_and_exit_status(self):
        result = subprocess.run([sys.executable, str(SCRIPT), '--help'], text=True,
                                capture_output=True, timeout=8)
        self.assertEqual(result.returncode, 0)
        for text in ('--seconds', '--max-output-mib', '--max-file-mib', '--min-free-gib',
                     'default 256', 'default 512', '3 when one of this wrapper'):
            self.assertIn(text, ' '.join(result.stdout.split()))


if __name__ == '__main__':
    unittest.main()
