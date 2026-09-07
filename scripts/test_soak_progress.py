import json
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
from unittest.mock import patch
import unittest

from soak_status import classify, inspect


class ProgressTests(unittest.TestCase):
    def test_stale_live_process_is_not_healthy(self):
        self.assertEqual(classify({'status': 'running'}, False, True, 121, 120),
                         'stalled_or_stale_progress')
        self.assertEqual(classify({'status': 'running'}, False, True, 119, 120), 'running')

    def test_missing_process_and_terminal_reports(self):
        self.assertEqual(classify({'status': 'running'}, False, False, 0, 120),
                         'interrupted_or_missing_process')
        self.assertEqual(classify({'status': 'passed', 'passed': True}, True, False, 999, 120), 'passed')
        self.assertEqual(classify({'status': 'passed', 'passed': True}, False, True, 0, 120),
                         'invalid_pass_report')
        self.assertEqual(classify({'status': 'passed'}, True, False, 0, 120), 'invalid_pass_report')
        self.assertEqual(classify({'status': 'failed'}, True, False, 999, 120), 'failed')

    def test_reused_pid_is_not_the_owned_process(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root/'progress.json').write_text(json.dumps(dict(status='running')))
            (root/'launch.json').write_text(json.dumps(dict(runs=[dict(
                name='test', output=str(root), pid=123, pid_identity='old start /owned/keel')])) )
            with patch('soak_status.subprocess.run', return_value=SimpleNamespace(stdout='new start /different/process')):
                row = inspect(root/'launch.json')['runs'][0]
            self.assertEqual(row['status'], 'interrupted_or_missing_process')
            self.assertFalse(row['owned_process_alive'])

    def test_watchdog_interrupts_sleep_and_runs_cleanup(self):
        code = '''
import json, signal, tempfile, time
from progress_watchdog import ProgressWatchdog
cleaned = False
with tempfile.TemporaryFile() as trace:
    try:
        with ProgressWatchdog(.05, trace) as watchdog:
            watchdog.beat('injected stalled operation')
            try:
                time.sleep(60)
            finally:
                cleaned = True
    except TimeoutError as exc:
        trace.seek(0)
        print(json.dumps(dict(cleaned=cleaned, error=str(exc), trace=bool(trace.read()),
                              timer=signal.getitimer(signal.ITIMER_REAL))))
    else:
        raise AssertionError('stalled operation did not time out')
'''
        result = subprocess.run([sys.executable, '-c', code], cwd=Path(__file__).parent,
                                text=True, capture_output=True, timeout=5, check=True)
        report = json.loads(result.stdout)
        self.assertTrue(report['cleaned'])
        self.assertTrue(report['trace'])
        self.assertIn('injected stalled operation', report['error'])
        self.assertEqual(report['timer'], [0, 0])


if __name__ == '__main__':
    unittest.main()
