"""Owned-process helpers for release and operational checks; no external service."""
import hashlib
import math
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
    digest = hashlib.sha256()
    with Path(path).open('rb') as source:
        while chunk := source.read(1 << 20):
            digest.update(chunk)
    return digest.hexdigest()


# The ports this process has given its servers. A port is reserved by binding
# port 0 and reading the one the kernel chose, and released at once so that the
# server can bind it; released, it is free again, and the kernel may choose it
# for the next reservation before the first server has started. Two Servers
# made before either starts, as soak.py makes its primary and replica, then
# share a port: the second fails to bind it, and the first answers the second's
# readiness check (#183's "Recovery on xfs", run 38341148820).
_given_ports = set()


def reserve_port():
    """A loopback port free now that no server of this process was given."""
    held = []
    try:
        for _ in range(64):
            reservation = socket.socket()
            held.append(reservation)
            reservation.bind(('127.0.0.1', 0))
            port = reservation.getsockname()[1]
            if port not in _given_ports:
                _given_ports.add(port)
                return port
        raise RuntimeError('no loopback port this process has not already given out')
    finally:
        # Held until a new port is found, so that the kernel cannot choose
        # the same given one again while it is looked for.
        for reservation in held:
            reservation.close()


def check_owner(client, pid, port):
    """Refuse a server on port that is not process pid, as INFO server says.

    A server that cannot bind its port leaves whatever holds it to answer for
    it, and a readiness check that only pings is answered. Builds before
    INFO's process_id (#140) cannot say who they are, and are taken as they
    answer."""
    try:
        answered = info(client, 'server').get('process_id')
    except RuntimeError:
        return
    if answered is not None and answered.strip() != str(pid):
        raise RuntimeError(f'port {port} is answered by process {answered.strip()}, '
                           f'not the server started as {pid}')


class Server:
    def __init__(self, binary, directory, *, policy='always', async_append=False,
                 port=None, extra=(), file_limit=None, password=None, startup_timeout=10,
                 shutdown_timeout=8, reply_timeout=None):
        if not math.isfinite(startup_timeout) or startup_timeout <= 0:
            raise ValueError("startup_timeout must be finite and positive")
        self.startup_timeout = startup_timeout
        # How long the harness's own connection waits for a reply once the
        # server is up; None keeps the client's three-second connect timeout.
        self.reply_timeout = reply_timeout
        if not math.isfinite(shutdown_timeout) or shutdown_timeout <= 0:
            raise ValueError("shutdown_timeout must be finite and positive")
        self.shutdown_timeout = shutdown_timeout
        self.binary = str(Path(binary).resolve())
        self.directory = Path(directory)
        self.directory.mkdir(parents=True, exist_ok=True)
        self.password = password or secrets.token_hex(24)
        if port is None:
            port = reserve_port()
        else:
            # Given, so that no reservation hands it to another server.
            _given_ports.add(port)
        self.port = port
        self.args = [self.binary, '-host', '127.0.0.1', '-port', str(port),
                     '-requirepass-env', 'KEEL_VALIDATION_PASSWORD', '-appendonly',
                     '-appendfilename', str((self.directory / 'store.aof').resolve()),
                     '-appendfsync', policy]
        if async_append:
            self.args.append('-aof-async-append')
        self.args.extend(extra)
        self.env = {key: os.environ[key] for key in ('PATH', 'HOME', 'TMPDIR') if key in os.environ}
        self.env['KEEL_VALIDATION_PASSWORD'] = self.password
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
            deadline = time.monotonic() + self.startup_timeout
            while time.monotonic() < deadline:
                if self.process.poll() is not None:
                    raise RuntimeError(f'server exited during startup; see {self.directory}/server.log')
                try:
                    self.client = Client('127.0.0.1', self.port, self.password)
                    if self.client.call('PING') != b'PONG':
                        raise RuntimeError('server returned an invalid readiness response')
                    check_owner(self.client, self.process.pid, self.port)
                    if self.reply_timeout is not None:
                        self.client.socket.settimeout(self.reply_timeout)
                    return self
                except OSError:
                    time.sleep(.02)
            try:
                aof_bytes = (self.directory / 'store.aof').stat().st_size
            except FileNotFoundError:
                aof_bytes = 0
            raise TimeoutError(f'server did not become ready within {self.startup_timeout}s; AOF bytes={aof_bytes}')
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
                result = self.process.wait(timeout=self.shutdown_timeout)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait()
                raise RuntimeError(f'server failed to stop within {self.shutdown_timeout} seconds')
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
