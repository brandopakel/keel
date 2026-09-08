"""Exercise the diagnostic's result gates under optimized Python with fake I/O."""
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


class ShutdownDiagnosticTests(unittest.TestCase):
    def invoke(self, mode):
        # Mock only the server boundary. The real writer, final-value checks,
        # replay gate, evidence retention and report path run unchanged under -O.
        code = r"""
import importlib.util, json, sys, time
from pathlib import Path
from types import SimpleNamespace
folder, mode, script = sys.argv[1:]
sys.path.insert(0, str(Path(script).parent))
spec = importlib.util.spec_from_file_location('diagnostic', script)
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
state = {}
class Client:
    def __init__(self, *args): pass
    def close(self): pass
    def call(self, command, *args):
        if command == 'SET':
            time.sleep(.001)
            if mode == 'bad_ack': return b'ERROR'
            state[args[0]] = args[1]
            return b'OK'
        if command == 'GET': return state.get(args[0])
        raise RuntimeError('unexpected fake command')
class Server:
    def __init__(self, binary, directory, **kwargs):
        self.directory = Path(directory)
        self.directory.mkdir(parents=True)
        (self.directory/'store.aof').write_bytes(b'fake closed AOF')
        self.port, self.password, self.starts = 1, 'synthetic', 0
        self.client = Client()
    def start(self):
        self.starts += 1
        if self.starts == 2 and mode == 'bad_replay': state['writer:0'] = b'old sequence'
    def stop(self, **kwargs): pass
m.Client, m.Server, m.info = Client, Server, lambda *args: {}
root = Path(folder)
binary = root/'binary'
binary.write_bytes(b'fake runtime')
args = SimpleNamespace(out=root/'run', bin=binary, policy='no', mode='sync',
                       seconds=.05, shutdown_seconds=30)
result = m.run(args)
expected = 0 if mode == 'success' else 1
if result != expected: raise RuntimeError(f'expected result {expected}, got {result}')
report = json.loads((root/'run/report.json').read_text())
if mode == 'success':
    if report['recovered_sequences'] != report['acknowledged_writes']:
        raise RuntimeError('sequence validation missing')
    if not all(report['acknowledged_writes']): raise RuntimeError('writers did not run')
if mode == 'bad_replay':
    if 'replay lost' not in report['failure']: raise RuntimeError('wrong failure gate')
    if not (root/'run/closed-before-replay.aof.gz').exists(): raise RuntimeError('original failure AOF missing')
"""
        with tempfile.TemporaryDirectory() as temp:
            result = subprocess.run([sys.executable, '-O', '-c', code, temp, mode,
                str(Path(__file__).with_name('check-large-write-shutdown.py').resolve())],
                capture_output=True, text=True, timeout=8)
            self.assertEqual(result.returncode, 0, result.stdout+result.stderr)

    def test_success_still_executes_writes_under_optimization(self):
        self.invoke('success')

    def test_bad_write_ack_cannot_pass_under_optimization(self):
        self.invoke('bad_ack')

    def test_stale_replay_cannot_pass_under_optimization(self):
        self.invoke('bad_replay')


if __name__ == '__main__':
    unittest.main()
