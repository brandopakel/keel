#!/usr/bin/env python3
"""Observe generator TCP_NODELAY syscalls in a brief owned Linux server probe."""
import argparse
import json
from pathlib import Path
import re
import socket
import subprocess
import time

from validation_lib import Client, sha256


def check(binary, generators, out):
    out.mkdir(parents=True, exist_ok=False)
    with socket.socket() as reservation:
        reservation.bind(('127.0.0.1', 0))
        port = reservation.getsockname()[1]
    report = {'status': 'running', 'server_sha256': sha256(binary), 'generators': []}
    server = client = None
    try:
        with (out/'server.log').open('wb') as log:
            server = subprocess.Popen([str(binary), '-host', '127.0.0.1', '-port', str(port)],
                                      stdout=log, stderr=log)
            deadline = time.monotonic()+10
            while True:
                if server.poll() is not None:
                    raise RuntimeError('owned socket probe server exited')
                try:
                    client = Client('127.0.0.1', port)
                    assert client.call('PING') == b'PONG'
                    break
                except OSError:
                    if client is not None:
                        client.close()
                        client = None
                    if time.monotonic() >= deadline:
                        raise
                    time.sleep(.02)
            owners = subprocess.check_output(['lsof', '-nP', '-tiTCP:'+str(port), '-sTCP:LISTEN'], text=True).split()
            assert owners == [str(server.pid)], 'socket probe listener ownership mismatch'
            for name, generator in generators:
                trace = out/(name+'-setsockopt.log')
                with (out/(name+'-load.log')).open('wb') as output:
                    subprocess.run(['strace', '-ff', '-e', 'trace=setsockopt', '-o', str(trace),
                                    str(generator), '-s', '127.0.0.1', '-p', str(port),
                                    '-t', '2', '-c', '2', '--ratio=1:0', '--data-size=64',
                                    '--key-minimum=1', '--key-maximum=1', '--requests=10',
                                    '--hide-histogram'], stdout=output, stderr=subprocess.STDOUT,
                                   check=True, timeout=15)
                calls = [line for path in sorted(out.glob(trace.name+'.*'))
                         for line in path.read_text().splitlines() if 'TCP_NODELAY' in line]
                values = [int(value) for line in calls
                          for value in re.findall(r'TCP_NODELAY, \[(-?\d+)\], 4\)\s*= 0', line)]
                row = {'name': name, 'binary_sha256': sha256(generator),
                       'tcp_nodelay_calls': calls, 'successful_values': values}
                report['generators'].append(row)
                if name == 'prepared':
                    assert len(values) >= 4 and len(values) == len(calls), 'missing TCP_NODELAY observations'
                    assert all(value == 1 for value in values), 'prepared generator did not enable TCP_NODELAY'
            report['status'] = 'passed'
    except BaseException as error:
        report.update(status='failed', failure=repr(error))
        raise
    finally:
        if client is not None:
            client.close()
        if server is not None:
            server.terminate()
            try:
                server.wait(timeout=5)
            except subprocess.TimeoutExpired:
                server.kill()
                server.wait()
                report.update(status='failed', shutdown_failure='owned probe required SIGKILL')
        (out/'report.json').write_text(json.dumps(report, indent=2)+'\n')
    if report['status'] != 'passed':
        raise RuntimeError('socket probe failed')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bin', type=Path, required=True)
    parser.add_argument('--memtier', type=Path, required=True)
    parser.add_argument('--unpatched', type=Path)
    parser.add_argument('--out', type=Path, required=True)
    args = parser.parse_args()
    generators = [('unpatched', args.unpatched.resolve())] if args.unpatched else []
    generators.append(('prepared', args.memtier.resolve()))
    check(args.bin.resolve(), generators, args.out.resolve())
