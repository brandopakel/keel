#!/usr/bin/env python3
"""Fresh-process, repeated scheduled-arrival sweeps with explicit generator limits."""
import argparse
import json
import os
from pathlib import Path
import platform
import socket
import subprocess
import sys
import threading
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'scripts'))
from validation_lib import Client, sha256


def cases():
    return {
        'read-64': [dict(size=64, keys=10000, writes=5, share=1)],
        'balanced-1k': [dict(size=1024, keys=10000, writes=50, share=1)],
        'expiry-storm': [dict(size=64, keys=10000, writes=50, share=1, expiry=True)],
        'large-1m': [dict(size=1048576, keys=32, writes=5, share=1)],
        'large-hash': [dict(size=64, keys=4096, writes=0, share=1, collection='hash')],
        'large-list': [dict(size=64, keys=4096, writes=0, share=1, collection='list')],
        # Separate processes/queues/connections prevent one generator queue
        # from masquerading as interference between tenants inside the server.
        'mixed-tenants': [dict(size=64, keys=10000, writes=5, share=.8),
                          dict(size=16384, keys=1024, writes=5, share=.1),
                          dict(size=1024, keys=2500, writes=90, share=.1)],
        # Independent processes/queues: one deep reader competes with ordinary
        # single-command traffic. Reports distinguish batches from commands.
        'competing-pipeline-64k': [dict(size=65536, keys=1, writes=0, share=.5, connections=1, pipeline=256),
                                  dict(size=64, keys=1000, writes=0, share=.5, connections=1)],
        'competing-pipeline-1m': [dict(size=1048576, keys=1, writes=0, share=.5, connections=1, pipeline=32),
                                 dict(size=64, keys=1000, writes=0, share=.5, connections=1)],
    }


def pin(command, cpus):
    return ['taskset', '-c', cpus, *command] if cpus else command


def info(client, section):
    raw = client.call('INFO', section)
    return dict(line.split(':', 1) for line in raw.decode().splitlines()
                if ':' in line and not line.startswith('#'))


def run_arm(args, name, specs, rate, repetition, arm, binary, directory):
    directory.mkdir()
    report = dict(status='running', workload=name, requested_rate=rate, repetition=repetition,
                  arm=arm, binary_sha256=sha256(binary), policy=args.policy,
                  append_mode='disabled' if args.policy == 'off' else args.append_mode)
    with socket.socket() as reservation:
        reservation.bind(('127.0.0.1', 0))
        port = reservation.getsockname()[1]
    env = {key: os.environ[key] for key in ('PATH', 'HOME', 'TMPDIR') if key in os.environ}
    command = [str(binary), '-host', '127.0.0.1', '-port', str(port), '-maxkeys', '2000000',
               '-auto-aof-rewrite-percentage', '0']
    if args.policy != 'off':
        command += ['-appendonly', '-appendfilename', str(directory/'store.aof'), '-appendfsync', args.policy]
        if args.append_mode in ('barrier', 'concurrent'): command += ['-aof-async-append']
        if args.append_mode == 'concurrent': command += ['-aof-concurrent-append']
    command = pin(command, args.server_cpus)
    server = client = None
    loads, files, samples = [], [], []
    stop = threading.Event()
    monitor = None
    try:
        log = (directory/'server.log').open('wb'); files.append(log)
        server = subprocess.Popen(command, env=env, stdout=log, stderr=log)
        deadline = time.monotonic()+10
        while time.monotonic() < deadline:
            if server.poll() is not None: raise RuntimeError('owned server exited during startup')
            try:
                client = Client('127.0.0.1', port)
                assert client.call('PING') == b'PONG'
                break
            except OSError:
                if client is not None: client.close(); client = None
                time.sleep(.02)
        if client is None: raise TimeoutError('owned server did not listen')
        load_commands = []
        assigned = 0
        for tenant, spec in enumerate(specs):
            tenant_rate = rate-assigned if tenant == len(specs)-1 else max(1, int(rate*spec['share']))
            assigned += tenant_rate
            connections = args.connections if len(specs) == 1 else max(2, args.connections//len(specs))
            connections = spec.get('connections', connections)
            load = [str(args.load), '-address', f'127.0.0.1:{port}', '-rate', str(tenant_rate),
                    '-seconds', str(args.seconds), '-connections', str(connections), '-queue', str(8*connections),
                    '-size', str(spec['size']), '-keys', str(spec['keys']), '-writes', str(spec['writes']),
                    '-prefix', f'capacity:{tenant}:']
            if spec.get('expiry'): load += ['-cohort-expiry']
            if spec.get('collection'): load += ['-collection', spec['collection']]
            if spec.get('pipeline'): load += ['-pipeline', str(spec['pipeline'])]
            load = pin(load, args.client_cpus)
            subprocess.run([*load, '-preload'], env=env, check=True, stdout=subprocess.DEVNULL,
                           stderr=subprocess.PIPE, timeout=180)
            load_commands.append(load)
        report['before_stats'] = info(client, 'stats')
        report['before_memory'] = info(client, 'memory')
        common_start = time.time_ns()+2_000_000_000
        for tenant, load in enumerate(load_commands):
            out = (directory/f'tenant-{tenant}.json').open('wb')
            err = (directory/f'tenant-{tenant}.log').open('wb')
            files += [out, err]
            loads.append(subprocess.Popen([*load, '-start-ns', str(common_start)], env=env, stdout=out, stderr=err))
        pids = ','.join(str(p.pid) for p in [server, *loads])
        def telemetry():
            while not stop.wait(.25):
                try:
                    body = subprocess.check_output(['ps', '-o', 'pid=,rss=,pcpu=', '-p', pids], text=True, timeout=2)
                    samples.append(dict(unix_ns=time.time_ns(), processes=body))
                except (OSError, subprocess.SubprocessError) as exc:
                    samples.append(dict(unix_ns=time.time_ns(), failure=repr(exc)))
        monitor = threading.Thread(target=telemetry, daemon=True); monitor.start()
        for load in loads:
            if load.wait(timeout=args.seconds+20) != 0: raise RuntimeError('arrival generator failed; see tenant log/report')
        for file in files: file.flush()
        report['tenants'] = [json.loads((directory/f'tenant-{i}.json').read_text()) for i in range(len(specs))]
        report['after_stats'] = info(client, 'stats')
        report['after_memory'] = info(client, 'memory')
        if name == 'expiry-storm' and args.seconds >= 4:
            assert int(report['after_stats'].get('expired_keys', 0)) > int(report['before_stats'].get('expired_keys', 0)), 'expiry cohort did not execute'
        assert sum(t['scheduled'] for t in report['tenants']) == int(rate*args.seconds)
        report['status'] = 'completed'
    except BaseException as exc:
        report['status'], report['failure'] = 'failed', repr(exc)
        raise
    finally:
        stop.set()
        if monitor is not None: monitor.join(timeout=3)
        if client is not None: client.close()
        for process in [*loads, server]:
            if process is not None:
                if process.poll() is None: process.terminate()
                try: process.wait(timeout=8)
                except subprocess.TimeoutExpired: process.kill(); process.wait()
        for file in files: file.close()
        report['telemetry'] = samples
        (directory/'report.json').write_text(json.dumps(report, indent=2)+'\n')
    return report


def main(args):
    args.out.mkdir(parents=True, exist_ok=False)
    selected = args.cases.split(',') if args.cases else list(cases())
    assert set(selected) <= cases().keys(), 'unknown workload'
    rates = [int(value) for value in args.rates.split(',')]
    assert all(10 <= rate <= 1000000 for rate in rates)
    manifest = dict(status='running', platform=platform.platform(), harness_sha256=sha256(__file__),
                    generator_sha256=sha256(args.load), arguments={k:str(v) if isinstance(v, Path) else v for k,v in vars(args).items()},
                    method='Fresh server per rate/arm/repetition, rotated arms, deterministic independent scheduled arrivals, separate tenant queues, bounded overload drops, no concurrent profiling.',
                    limitations='Public VM tenancy remains uncontrolled. Scheduler lag, generator CPU and dropped requests are part of interpretation; this is not a dedicated-host capacity guarantee.', runs=[])
    try:
        for repetition in range(1, args.reps+1):
            for name in selected:
                for index, rate in enumerate(rates):
                    arms = [('candidate', args.candidate)]
                    if args.baseline: arms.insert(0, ('baseline', args.baseline))
                    if (repetition+index)%2 == 0: arms.reverse()
                    for arm, binary in arms:
                        run_name = f'r{repetition}-{name}-rate{rate}-{arm}'
                        report = run_arm(args, name, cases()[name], rate, repetition, arm, binary, args.out/run_name)
                        manifest['runs'].append(dict(name=run_name,status=report['status']))
                        print(run_name, report['status'], flush=True)
                        (args.out/'manifest.json').write_text(json.dumps(manifest,indent=2)+'\n')
        manifest['status'] = 'completed'
    except BaseException as exc:
        manifest['status'], manifest['failure'] = 'failed', repr(exc)
        raise
    finally: (args.out/'manifest.json').write_text(json.dumps(manifest,indent=2)+'\n')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--candidate', type=Path, required=True)
    parser.add_argument('--baseline', type=Path)
    parser.add_argument('--load', type=Path, required=True)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--rates', default='1000,10000,50000,100000,150000')
    parser.add_argument('--cases')
    parser.add_argument('--seconds', type=int, default=10)
    parser.add_argument('--reps', type=int, default=3)
    parser.add_argument('--connections', type=int, default=32)
    parser.add_argument('--policy', choices=['off','no','everysec','always'], default='off')
    parser.add_argument('--append-mode', choices=['sync','barrier','concurrent'], default='concurrent')
    parser.add_argument('--server-cpus')
    parser.add_argument('--client-cpus')
    args = parser.parse_args()
    if not (1 <= args.seconds <= 60 and 1 <= args.reps <= 10 and 1 <= args.connections <= 512): parser.error('invalid measurement bounds')
    for name in ['candidate','baseline','load','out']:
        value = getattr(args,name)
        if value is not None: setattr(args,name,value.resolve())
    main(args)
