"""Tests for validation_lib's port reservation and readiness check."""
import os
import socket
import stat
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import validation_lib  # noqa: E402
from validation_lib import Server, reserve_port  # noqa: E402

# A stand-in for keel: it serves PING, AUTH and INFO server, reporting its own
# process id, on the port after -port, and exits on SIGTERM. When the port is
# taken it logs the bind error, as keel does, and stays up a while before it
# exits, so that a readiness check meets whatever holds the port first.
FAKE_SERVER = textwrap.dedent('''\
    import os, signal, socket, sys, threading, time
    signal.signal(signal.SIGTERM, lambda *_: os._exit(0))
    port = int(sys.argv[sys.argv.index('-port') + 1])
    listener = socket.socket()
    try:
        listener.bind(('127.0.0.1', port))
    except OSError as exc:
        print(f'bind 127.0.0.1:{port}: {exc}', flush=True)
        time.sleep(3)
        sys.exit(1)
    listener.listen()

    def serve(conn):
        stream = conn.makefile('rb')
        while True:
            header = stream.readline()
            if not header:
                break
            parts = []
            for _ in range(int(header[1:])):
                n = int(stream.readline()[1:])
                parts.append(stream.read(n + 2)[:-2])
            name = parts[0].upper()
            if name == b'INFO' and '-fake-info-error' in sys.argv:
                conn.sendall(b'-ERR unknown section\\r\\n')
            elif name == b'INFO':
                body = b'# Server\\r\\nprocess_id:%d\\r\\n' % os.getpid()
                conn.sendall(b'$%d\\r\\n%s\\r\\n' % (len(body), body))
            elif name == b'PING':
                conn.sendall(b'+PONG\\r\\n')
            else:
                conn.sendall(b'+OK\\r\\n')
        conn.close()

    while True:
        conn, _ = listener.accept()
        threading.Thread(target=serve, args=(conn,), daemon=True).start()
''')


class FakeSocket:
    """A socket whose bind is a no-op and whose port comes from a script."""

    def __init__(self, ports, opened):
        self.port = next(ports)
        self.closed = False
        opened.append(self)

    def bind(self, address):
        pass

    def getsockname(self):
        return ('127.0.0.1', self.port)

    def close(self):
        self.closed = True


class ReservePortTest(unittest.TestCase):
    def setUp(self):
        patcher = mock.patch.object(validation_lib, '_given_ports', set())
        patcher.start()
        self.addCleanup(patcher.stop)

    def scripted(self, *ports):
        ports, opened = iter(ports), []
        patcher = mock.patch.object(validation_lib.socket, 'socket', lambda: FakeSocket(ports, opened))
        patcher.start()
        self.addCleanup(patcher.stop)
        return opened

    def test_a_port_given_once_is_not_given_again(self):
        opened = self.scripted(41000, 41000, 41000, 41001)
        self.assertEqual(reserve_port(), 41000)
        self.assertEqual(reserve_port(), 41001)
        self.assertTrue(all(s.closed for s in opened), 'every reservation socket is closed')
        self.assertEqual(len(opened), 4)

    def test_a_port_a_server_was_given_explicitly_is_not_reserved(self):
        self.scripted(42000, 42001)
        with tempfile.TemporaryDirectory() as root:
            Server(sys.executable, Path(root) / 'given', port=42000)
            self.assertEqual(Server(sys.executable, Path(root) / 'reserved').port, 42001)

    def test_real_reservations_are_distinct(self):
        ports = [reserve_port() for _ in range(50)]
        self.assertEqual(len(set(ports)), len(ports))
        with socket.socket() as probe:
            probe.bind(('127.0.0.1', ports[-1]))


class ReadinessTest(unittest.TestCase):
    def setUp(self):
        self.root = Path(tempfile.mkdtemp())
        self.binary = self.root / 'fake-keel'
        self.binary.write_text(f'#!{sys.executable}\n' + FAKE_SERVER)
        self.binary.chmod(self.binary.stat().st_mode | stat.S_IXUSR)

    def test_a_server_that_answers_as_itself_is_ready(self):
        with Server(self.binary, self.root / 'one', startup_timeout=10) as server:
            self.assertEqual(server.client.call('PING'), b'PONG')

    def test_a_server_whose_port_another_process_answers_is_refused(self):
        with Server(self.binary, self.root / 'holder', startup_timeout=10) as holder:
            # The collision soak.py met: a second server on the first one's
            # port, which cannot bind it, while the first answers PING.
            second = Server(self.binary, self.root / 'second', port=holder.port,
                            password=holder.password, startup_timeout=10)
            with self.assertRaisesRegex(RuntimeError, rf'port {holder.port} is answered by process \d+'):
                second.start()
            self.assertIsNone(second.process, 'the refused server is stopped')

    def test_a_server_that_refuses_info_is_refused(self):
        server = Server(self.binary, self.root / 'mute', extra=['-fake-info-error'], startup_timeout=10)
        with self.assertRaisesRegex(RuntimeError, r'answered PING but refused INFO server'):
            server.start()
        self.assertIsNone(server.process, 'the refused server is stopped')


if __name__ == '__main__':
    unittest.main()
