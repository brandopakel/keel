"""Small synchronous RESP3 client that keeps the type of every reply.

A RESP3 differential has to tell a map from a flattened array, a double from a
bulk string, and a RESP3 null from a RESP2 one. A decoder that returns plain
Python values cannot: each of those pairs comes back as the same list, bytes or
None. Every reply here is a tuple whose first element names its type, so two
servers' replies compare equal only when their types do.
"""
import socket

AGGREGATES = {b'*': 'array', b'~': 'set', b'>': 'push'}


class RESP3Client:
    def __init__(self, host, port, password=None):
        self.socket = socket.create_connection((host, port), timeout=3)
        try:
            self.stream = self.socket.makefile('rb')
            # One HELLO that both switches protocol and logs in, as redis-py 8
            # and ioredis 6 connect.
            auth = ('AUTH', 'default', password) if password else ()
            self.hello = self.call('HELLO', 3, *auth)
            if self.hello[0] != 'map':
                raise RuntimeError(f'HELLO 3 answered {self.hello!r}')
        except Exception:
            self.close()
            raise

    def close(self):
        if hasattr(self, 'stream'):
            self.stream.close()
        self.socket.close()

    def call(self, *parts):
        parts = [p if isinstance(p, bytes) else str(p).encode() for p in parts]
        self.socket.sendall(b'*%d\r\n' % len(parts) + b''.join(b'$%d\r\n' % len(p) + p + b'\r\n' for p in parts))
        value = self.read()
        if value[0] == 'error':
            raise RuntimeError(value[1].decode(errors='replace'))
        return value

    def read(self, depth=0):
        if depth > 16:
            raise ValueError('RESP nesting limit')
        line = self.stream.readline(65537)
        if not line.endswith(b'\r\n') or len(line) > 65536:
            raise ValueError('invalid RESP header')
        kind, body = line[:1], line[1:-2]
        # An error inside an aggregate stands in its position (BF.MADD, CMS.INCRBY),
        # so it is a value here; call() raises only for a reply that is one.
        if kind == b'-':
            return ('error', body)
        if kind == b'+':
            return ('simple', body)
        if kind == b':':
            return ('int', int(body))
        if kind == b'_':
            if body:
                raise ValueError('invalid null')
            return ('null',)
        if kind == b',':
            return ('double', body)
        if kind == b'#':
            if body not in (b't', b'f'):
                raise ValueError('invalid boolean')
            return ('bool', body == b't')
        if kind == b'(':
            return ('bignum', body)
        n = int(body)
        # RESP2's nulls on a RESP3 connection are a mismatch this exists to
        # find, so they are kept apart from RESP3's null rather than read alike.
        if n == -1 and kind == b'$':
            return ('resp2-null-bulk',)
        if n == -1 and kind == b'*':
            return ('resp2-null-array',)
        if n < 0 or n > 64 * 1024 * 1024:
            raise ValueError('RESP length limit')
        if kind in (b'$', b'=', b'!'):
            value = self.stream.read(n + 2)
            if len(value) != n + 2 or value[-2:] != b'\r\n':
                raise ValueError('invalid bulk reply')
            value = value[:-2]
            if kind == b'$':
                return ('bulk', value)
            if kind == b'!':
                return ('error', value)
            if value[3:4] != b':':
                raise ValueError('invalid verbatim string')
            return ('verbatim', value[:3], value[4:])
        if kind in AGGREGATES:
            return (AGGREGATES[kind], [self.read(depth + 1) for _ in range(n)])
        if kind == b'%':
            pairs = []
            for _ in range(n):
                key = self.read(depth + 1)
                pairs.append((key, self.read(depth + 1)))
            return ('map', pairs)
        raise ValueError(f'unknown RESP kind {kind!r}')
