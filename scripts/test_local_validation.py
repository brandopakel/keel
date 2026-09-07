import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).with_name('run-local-validation.py')


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


if __name__ == '__main__':
    unittest.main()
