"""Run the documented example against an isolated authenticated Keel process."""
import os
import socket
import subprocess
import sys
import tempfile
import time
from pathlib import Path

with socket.socket() as reserve:
    reserve.bind(('127.0.0.1', 0))
    port = reserve.getsockname()[1]
env = dict(os.environ, KEEL_PORT=str(port), KEEL_PASSWORD='example-test-only')
binary = str(Path(os.environ.get('BIN', './keel')).resolve())
with tempfile.TemporaryDirectory(prefix='keel-client-') as directory:
    with subprocess.Popen([binary, '-port', str(port), '-requirepass-env', 'KEEL_PASSWORD',
                           '-appendonly', '-appendfilename', directory+'/test.aof'], env=env,
                          stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL) as server:
        try:
            deadline = time.monotonic()+5
            while True:
                if server.poll() is not None: raise RuntimeError('Keel exited before readiness')
                try:
                    with socket.create_connection(('127.0.0.1', port), timeout=.1): break
                except OSError:
                    if time.monotonic() > deadline: raise
                    time.sleep(.02)
            subprocess.run([sys.executable, str(Path(__file__).with_name('cache_analytics.py'))],
                           env=env, check=True, timeout=15)
        finally:
            server.terminate()
            try: server.wait(timeout=6)
            except subprocess.TimeoutExpired: server.kill(); server.wait()
        if server.returncode != 0: raise RuntimeError(f'Keel shutdown failed: {server.returncode}')
