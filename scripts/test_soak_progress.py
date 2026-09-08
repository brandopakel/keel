import json
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
from unittest.mock import patch, MagicMock
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
                name='test', output=str(root), pid=123,
                pid_identity='new start /different/process',
                pid_identity_after_exec='old start /owned/keel')])))
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


    def test_request_failure_keeps_origin_before_process_diagnostics(self):
        import soak
        with tempfile.TemporaryDirectory() as directory:
            args = SimpleNamespace(out=directory, bin='unused', replication_protocol=2,
                                   concurrent=True, cycle_seconds=10)
            primary, replica = MagicMock(), MagicMock()
            primary.password, primary.port = 'private-password', 12345
            def failed_request(*parts):
                raise TimeoutError('timed out')
            primary.client.call.side_effect = failed_request
            report = {}
            def diagnostics(server):
                self.assertGreater(report['failure_observed_unix_seconds'], 0)
                self.assertEqual(report['failure_stack'][-1]['function'], 'failed_request')
                return {'captured': True}
            with patch.object(soak, 'Server', side_effect=[primary, replica]), \
                 patch.object(soak, 'capture_failed_process', side_effect=diagnostics):
                with self.assertRaises(TimeoutError):
                    soak.run(args, report, MagicMock())
            self.assertNotIn('private-password', json.dumps(report))
            self.assertGreaterEqual(report['elapsed_seconds'], 0)
            primary.stop.assert_called_once_with(check=False)
            replica.stop.assert_called_once_with(check=False)


if __name__ == '__main__':
    unittest.main()
