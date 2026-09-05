"""Small synchronous RESP2 client; stdlib sockets are gevent-patchable."""
import socket
import ssl

class Client:
    def __init__(self, host, port, password=None, tls=False):
        self.socket=socket.create_connection((host,port),timeout=3)
        try:
            if tls: self.socket=ssl.create_default_context().wrap_socket(self.socket,server_hostname=host)
            self.stream=self.socket.makefile('rb')
            if password and self.call('AUTH',password)!=b'OK': raise RuntimeError('AUTH failed')
        except Exception:
            self.close();raise
    def close(self):
        if hasattr(self,'stream'): self.stream.close()
        self.socket.close()
    def call(self,*parts):
        parts=[p if isinstance(p,bytes) else str(p).encode() for p in parts]
        self.socket.sendall(b'*%d\r\n'%len(parts)+b''.join(b'$%d\r\n'%len(p)+p+b'\r\n' for p in parts))
        return self.read()
    def read(self,depth=0):
        if depth>16: raise ValueError('RESP nesting limit')
        line=self.stream.readline(65537)
        if not line.endswith(b'\r\n') or len(line)>65536: raise ValueError('invalid RESP header')
        kind,body=line[:1],line[1:-2]
        if kind==b'-': raise RuntimeError(body.decode(errors='replace'))
        if kind==b'+': return body
        n=int(body)
        if kind==b':': return n
        if n==-1 and kind in (b'$',b'*'): return None
        if n<0 or n>16*1024*1024: raise ValueError('RESP length limit')
        if kind==b'$':
            value=self.stream.read(n+2)
            if len(value)!=n+2 or value[-2:]!=b'\r\n': raise ValueError('invalid bulk reply')
            return value[:-2]
        if kind==b'*': return [self.read(depth+1) for _ in range(n)]
        raise ValueError('unknown RESP kind')
