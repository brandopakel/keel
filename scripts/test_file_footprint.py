import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).with_name('test-file-footprint.py')

# Model go test: a t.TempDir parent under GOTMPDIR named after its subtest,
# a go-build work directory beside it, and a small file in ordinary TMPDIR.
WRITER = """
import os, sys, time
from pathlib import Path
test = Path(os.environ['GOTMPDIR'])/'TestLargeLogbarrierset2682266309'/'001'
test.mkdir(parents=True)
(test/'store.aof').write_bytes(b'x'*(3<<20))
(test/'small').write_bytes(b'x'*(1<<20))
build = Path(os.environ['GOTMPDIR'])/'go-build123'/'b001'
build.mkdir(parents=True)
(build/'keel.test').write_bytes(b'x'*(5<<20))
(Path(os.environ['TMPDIR'])/'scratch').write_bytes(b'x'*1024)
time.sleep(.5)
for path in (test/'store.aof', test/'small'):
    path.unlink()
sys.exit(int(sys.argv[1]))
"""


class FootprintTests(unittest.TestCase):
    def record(self, out, code, *flags, env=None):
        return subprocess.run([sys.executable, str(SCRIPT), '--out', str(out), '--interval', '.05',
                               *flags, '--', sys.executable, '-c', WRITER, str(code)],
                              text=True, capture_output=True, timeout=20, env=env)

    def test_records_removed_test_files_and_keeps_exit_status(self):
        with tempfile.TemporaryDirectory() as temp:
            out = Path(temp)/'footprint'
            summary = Path(temp)/'summary.md'
            result = self.record(out, 4, '--warn-file-mib', '2', '--warn-test-mib', '100',
                                 env=dict(os.environ, GITHUB_STEP_SUMMARY=str(summary)))
            self.assertEqual(result.returncode, 4, result.stdout+result.stderr)
            record = json.loads((out/'test-file-footprint.json').read_text())
            test = record['tests'][0]
            self.assertEqual(test['name'], 'TestLargeLogbarrierset')
            self.assertEqual(test['peak_file_bytes'], 3<<20)
            self.assertEqual(test['peak_bytes'], 4<<20)
            self.assertEqual(test['peak_file'], os.path.join('TestLargeLogbarrierset2682266309', '001', 'store.aof'))
            # Build work is reported apart from the tests' own files.
            self.assertEqual(record['peak_build_bytes'], 5<<20)
            self.assertNotIn('go-build', ' '.join(t['name'] for t in record['tests']))
            self.assertGreaterEqual(record['peak_concurrent_test_bytes'], (4<<20)+1024)
            self.assertEqual(len(record['warnings']), 1)
            self.assertIn('::warning title=Test outgrows the local validation budget::TestLargeLogbarrierset wrote a 3.0 MiB file',
                          result.stdout)
            self.assertIn('--max-file-mib', record['warnings'][0])
            self.assertIn('| TestLargeLogbarrierset | 4.0 MiB | 3.0 MiB |', summary.read_text())

    def test_default_thresholds_follow_the_local_wrapper(self):
        with tempfile.TemporaryDirectory() as temp:
            out = Path(temp)/'footprint'
            result = self.record(out, 0)
            self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
            record = json.loads((out/'test-file-footprint.json').read_text())
            defaults = record['local_wrapper_defaults']
            self.assertEqual(record['warn_file_bytes'], defaults['max_file_mib']*2**20//2)
            self.assertEqual(record['warn_test_bytes'], defaults['max_output_mib']*2**20//4)
            self.assertEqual(record['warnings'], [])
            self.assertNotIn('::warning', result.stdout)

    def test_unreadable_directory_does_not_stop_the_record(self):
        if os.geteuid() == 0:
            self.skipTest('root can traverse mode-000 directories')
        code = ("import os,sys,time; from pathlib import Path; p=Path(os.environ['TMPDIR'])/'TestHidden1'; "
                "p.mkdir(); (p/'f').write_bytes(b'x'); p.chmod(0); time.sleep(.3); p.chmod(0o700)")
        # Keep the deliberately unreadable fixture outside an enclosing local
        # validation wrapper's monitored root, which would refuse it.
        enclosing = os.environ.get('KEEL_LOCAL_VALIDATION_ROOT')
        with tempfile.TemporaryDirectory(dir=Path(enclosing).parent if enclosing else None,
                                         prefix='keel-unreadable-footprint-') as temp:
            out = Path(temp)/'footprint'
            result = subprocess.run([sys.executable, str(SCRIPT), '--out', str(out), '--interval', '.05',
                                     '--', sys.executable, '-c', code], text=True, capture_output=True, timeout=20)
            self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
            record = json.loads((out/'test-file-footprint.json').read_text())
            self.assertEqual(record['sampling_errors'], [])
            self.assertIn('TestHidden', [t['name'] for t in record['tests']])

    def test_existing_output_is_refused(self):
        with tempfile.TemporaryDirectory() as temp:
            result = self.record(Path(temp), 0)
            self.assertNotEqual(result.returncode, 0)


if __name__ == '__main__':
    unittest.main()
