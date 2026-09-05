"""Owned-process helpers for release and operational checks; no external service."""
import hashlib
import os
import resource
import secrets
import socket
import subprocess
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'bench/external/aws'))
from resp_client import Client


def sha256(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


class Server:
    def __init__(self, binary, directory, *, policy='always', async_append=False,
                 port=None, extra=(), file_limit=None, password=None):
        self.binary = str(Path(binary).resolve())
        self.directory = Path(directory)
        self.directory.mkdir(parents=True, exist_ok=True)
        self.password = password or secrets.token_hex(24)
        if port is None:
            with socket.socket() as reservation:
                reservation.bind(('127.0.0.1', 0))
                port = reservation.getsockname()[1]
        self.port = port
        self.args = [self.binary, '-host', '127.0.0.1', '-port', str(port),
                     '-requirepass-env', 'KEEL_VALIDATION_PASSWORD', '-appendonly',
                     '-appendfilename', str((self.directory / 'store.aof').resolve()),
                     '-appendfsync', policy]
        if async_append:
            self.args.append('-aof-async-append')
        self.args.extend(extra)
        self.env = dict(os.environ, KEEL_VALIDATION_PASSWORD=self.password)
        self.file_limit = file_limit
        self.process = None
        self.client = None
        self.log = None

    def start(self):
        self.log = (self.directory / 'server.log').open('ab')
        limit = self.file_limit
        def set_limit():
            resource.setrlimit(resource.RLIMIT_FSIZE, (limit, limit))
        try:
            self.process = subprocess.Popen(
                self.args, env=self.env, stdout=self.log, stderr=self.log,
                preexec_fn=set_limit if limit is not None else None)
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline:
                if self.process.poll() is not None:
                    raise RuntimeError(f'server exited during startup; see {self.directory}/server.log')
                try:
                    self.client = Client('127.0.0.1', self.port, self.password)
                    assert self.client.call('PING') == b'PONG'
                    return self
                except OSError:
                    time.sleep(.02)
            raise TimeoutError('server did not become ready')
        except BaseException:
            self.stop(check=False)
            raise

    def stop(self, *, crash=False, check=True):
        if self.client is not None:
            self.client.close()
            self.client = None
        if self.process is not None:
            if self.process.poll() is None:
                self.process.kill() if crash else self.process.terminate()
            try:
                result = self.process.wait(timeout=8)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait()
                raise RuntimeError('server failed to stop within eight seconds')
            finally:
                self.log.close()
            self.process = None
            if check and not crash and result != 0:
                raise RuntimeError(f'server exited with {result}; see {self.directory}/server.log')

    def __enter__(self):
        return self.start()

    def __exit__(self, kind, value, tb):
        self.stop(check=kind is None)


def info(client, section):
    raw = client.call('INFO', section)
    if isinstance(raw, bytes):
        raw = raw.decode()
    return dict(line.split(':', 1) for line in raw.splitlines()
                if ':' in line and not line.startswith('#'))


def rewrite(client):
    before = int(info(client, 'persistence')['aof_rewrites'])
    client.call('BGREWRITEAOF')
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        if int(info(client, 'persistence')['aof_rewrites']) > before:
            return
        time.sleep(.01)
    raise TimeoutError('rewrite did not complete')
