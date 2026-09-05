#!/usr/bin/env python3
"""Measure RSS with all verified client connections still open; owns only child PIDs."""
import csv, os, socket, subprocess, time, itertools, json, hashlib, platform
from pathlib import Path

binary = os.environ['BIN']
output = Path(os.getenv('OUT', 'bench/results/memory.csv'))
output.parent.mkdir(parents=True, exist_ok=True)
counts = [int(n) for n in os.getenv('CONNECTIONS', '0 100 500 1000 5000').split()]

def rss(pid):
    return int(subprocess.check_output(['ps', '-o', 'rss=', '-p', str(pid)], text=True).strip())

with output.open('w') as f:
    writer = csv.writer(f)
    writer.writerow(['server', 'conns', 'rss_kb', 'baseline_rss_kb', 'rss_delta_kb_per_conn', 'rep'])
    for mode in os.getenv('SERVERS', 'redis kqueue net').split():
        for count, rep in itertools.product(counts, range(1, int(os.getenv('REPS', '3'))+1)):
            with socket.socket() as reserve:
                reserve.bind(('127.0.0.1', 0))
                port = reserve.getsockname()[1]
            argv = (['redis-server', '--port', str(port), '--bind', '127.0.0.1', '--save', '', '--appendonly', 'no']
                    if mode == 'redis' else [binary, '-host', '127.0.0.1', '-port', str(port), '-mode', mode])
            clients = []
            with subprocess.Popen(argv, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL) as process:
                try:
                    deadline = time.monotonic() + 5
                    while True:
                        if process.poll() is not None:
                            raise RuntimeError(f'{mode} exited before readiness')
                        try:
                            with socket.create_connection(('127.0.0.1', port), timeout=.1) as probe:
                                probe.sendall(b'*1\r\n$4\r\nPING\r\n')
                                assert probe.recv(64) == b'+PONG\r\n'
                            break
                        except OSError:
                            if time.monotonic() > deadline: raise
                            time.sleep(.02)
                    owner = subprocess.check_output(['lsof', '-nP', '-tiTCP:'+str(port), '-sTCP:LISTEN'], text=True).strip()
                    if owner != str(process.pid): raise RuntimeError('listener PID mismatch')
                    time.sleep(.1)
                    base = rss(process.pid)
                    for _ in range(count):
                        conn = socket.create_connection(('127.0.0.1', port), timeout=5)
                        clients.append(conn)
                        conn.sendall(b'*1\r\n$4\r\nPING\r\n')
                        if conn.recv(64) != b'+PONG\r\n': raise RuntimeError('client handshake failed')
                    time.sleep(.25)
                    # Measure before clients are closed, using the number actually opened.
                    used = rss(process.pid)
                    writer.writerow([mode, len(clients), used, base, (used-base)/len(clients) if clients else 0, rep])
                    f.flush()
                finally:
                    for conn in clients: conn.close()
                    if process.poll() is None:
                        process.terminate()
                        try: process.wait(timeout=6)
                        except subprocess.TimeoutExpired: process.kill(); process.wait()

metadata = {'platform': platform.platform(), 'binary': str(Path(binary).resolve()),
            'binary_sha256': hashlib.sha256(Path(binary).read_bytes()).hexdigest(),
            'go': subprocess.check_output(['go', 'version'], text=True).strip(),
            'counts': counts, 'reps': int(os.getenv('REPS', '3')),
            'note': 'Idle live connections after PING; separate fresh server per row. Not a cache workload or throughput comparison.'}
output.with_suffix('.metadata.json').write_text(json.dumps(metadata, indent=2)+'\n')
