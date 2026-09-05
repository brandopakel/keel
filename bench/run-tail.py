#!/usr/bin/env python3
"""Isolated closed-loop mixed workload plus independent scheduled PING probe.
Records all attempts, including errors. Probe latency includes scheduling delay.
No third-party dependencies. Not an open-loop saturation/production claim.
"""
import argparse, csv, gzip, hashlib, json, math, os, platform, secrets, socket, subprocess, tempfile, threading, time
from pathlib import Path

class RunFailure(RuntimeError):
    def __init__(self, message, rows):
        super().__init__(message)
        self.rows = rows

class Client:
    def __init__(self, port, password):
        self.sock = socket.create_connection(('127.0.0.1', port), timeout=3)
        self.stream = self.sock.makefile('rb')
        try:
            if self.call('AUTH',password) != b'OK': raise RuntimeError('benchmark server AUTH mismatch')
        except Exception:
            self.close(); raise
    def close(self):
        self.stream.close(); self.sock.close()
    def call(self, *parts):
        parts = [str(p).encode() for p in parts]
        self.sock.sendall(b'*%d\r\n' % len(parts) + b''.join(b'$%d\r\n' % len(p)+p+b'\r\n' for p in parts))
        return self.read()
    def read(self):
        line = self.stream.readline()
        if not line: raise EOFError('server closed connection')
        kind, body = line[:1], line[1:-2]
        if kind == b'-': raise RuntimeError(body.decode())
        if kind == b'$':
            n = int(body)
            if n < 0: return None
            value = self.stream.read(n+2)
            if len(value) != n+2: raise EOFError('short bulk reply')
            return value[:-2]
        if kind == b'*': return [self.read() for _ in range(int(body))]
        return body

def run(args, policy, repetition):
    rows = []
    with tempfile.TemporaryDirectory(prefix='keel-tail-') as directory:
        with socket.socket() as reservation:
            reservation.bind(('127.0.0.1', 0)); port = reservation.getsockname()[1]
        password=secrets.token_hex(32)
        env=dict(os.environ,KEEL_BENCH_PASSWORD=password)
        if getattr(args, 'telemetry', False):
            env['GODEBUG'] = 'gctrace=1'
        argv = [args.bin, '-port', str(port), '-requirepass-env', 'KEEL_BENCH_PASSWORD']
        if policy != 'off':
            argv += ['-appendonly', '-appendfilename', directory+'/log.aof', '-appendfsync', policy]
            if args.async_append: argv += ['-aof-async-append']
        log_path = str(args.out)+f'.{policy}.{repetition}.server.log'
        with open(log_path, 'w+') as log:
            process = subprocess.Popen(argv, env=env, stdout=log, stderr=log)
            clients = []
            telemetry = []
            stop_monitor = threading.Event()
            monitor = None
            try:
                deadline = time.monotonic()+5
                while True:
                    try:
                        load = Client(port,password); clients.append(load); break
                    except OSError as exc:
                        if process.poll() is not None or time.monotonic() > deadline:
                            log.seek(0); raise RuntimeError(f'server readiness failed: {type(exc).__name__}: {exc}\n{log.read()}') from exc
                        time.sleep(.01)
                probe = Client(port,password); clients.append(probe)
                for i in range(1000): load.call('SET', 'cache:'+str(i), 'v'*256, 'PX', 60000)
                # Keep setup requests bounded even when the measured list is large.
                for first in range(0, args.members, 256):
                    load.call('RPUSH', 'large', *(['v'*128]*min(256, args.members-first)))
                # Warmup has the same small-key read/write distribution.
                for i in range(200):
                    if i%5==0: load.call('SET','cache:'+str(i),'v'*256,'PX',60000)
                    else: load.call('GET','cache:'+str(i))
                start = time.monotonic(); end = start+args.seconds
                if getattr(args, 'telemetry', False):
                    def observe():
                        while not stop_monitor.wait(.5):
                            try:
                                stats = subprocess.check_output(
                                    ['ps','-o','rss=,pcpu=','-p',str(process.pid)],
                                    text=True, timeout=2).strip().split()
                                telemetry.append({'seconds':time.monotonic()-start,
                                                  'rss_kib':int(stats[0]),
                                                  'cpu_percent_lifetime':float(stats[1])})
                            except Exception as exc:
                                telemetry.append({'seconds':time.monotonic()-start,'error':str(exc)})
                    monitor = threading.Thread(target=observe)
                    monitor.start()
                def sample(client, role, operation, parts, scheduled):
                    before = time.monotonic(); error = ''
                    try: client.call(*parts)
                    except Exception as exc: error = str(exc)
                    after = time.monotonic()
                    rows.append([policy,repetition,role,operation,scheduled-start,(after-before)*1000,(after-scheduled)*1000,error])
                def probes():
                    scheduled = start
                    while scheduled < end:
                        if time.monotonic() > end+3:
                            while scheduled < end:
                                rows.append([policy,repetition,'probe','PING',scheduled-start,0,(time.monotonic()-scheduled)*1000,'dropped: probe drain deadline'])
                                scheduled += .01
                            break
                        time.sleep(max(0, scheduled-time.monotonic()))
                        sample(probe, 'probe', 'PING', ['PING'], scheduled)
                        scheduled += .01
                thread = threading.Thread(target=probes); thread.start()
                i = 0; rewritten = False
                while time.monotonic() < end:
                    if policy != 'off' and not rewritten and time.monotonic()-start > args.seconds/3:
                        sample(load,'load','BGREWRITEAOF',['BGREWRITEAOF'],time.monotonic()); rewritten=True
                    key = 'cache:'+str(i%1000)
                    parts = ['LRANGE','large',0,-1] if i%100 == 0 else (['SET',key,'v'*256,'PX',1000] if i%5==0 else ['GET',key])
                    sample(load,'load',parts[0],parts,time.monotonic()); i+=1
                thread.join()
            finally:
                stop_monitor.set()
                if monitor is not None:
                    monitor.join()
                    Path(log_path+'.telemetry.json').write_text(json.dumps(telemetry,indent=2)+'\n')
                for client in clients: client.close()
                process.terminate()
                try: process.wait(timeout=7)
                except subprocess.TimeoutExpired: process.kill(); process.wait(); raise RunFailure('shutdown timeout', rows)
                if process.returncode != 0:
                    log.seek(0); raise RunFailure(log.read(), rows)
    return rows

def main():
    parser=argparse.ArgumentParser()
    parser.add_argument('--bin', required=True)
    parser.add_argument('--out', required=True)
    parser.add_argument('--async-append', action='store_true', help='enable worker appends for persistent policies')
    parser.add_argument('--seconds', type=float, default=10)
    parser.add_argument('--reps', type=int, default=3)
    parser.add_argument('--members', type=int, default=10000)
    parser.add_argument('--telemetry', action='store_true', help='retain ps RSS/CPU samples and Go GC trace logs')
    parser.add_argument('--policies', nargs='+', default=['off','everysec','always'])
    args=parser.parse_args()
    if not math.isfinite(args.seconds) or args.seconds<1 or args.reps<1 or args.members<1: parser.error('duration >= 1 second and positive repetitions/members required')
    args.bin=str(Path(args.bin).resolve()); out=Path(args.out); out.parent.mkdir(parents=True,exist_ok=True)
    metadata={'arguments':vars(args),'platform':platform.platform(),'python':platform.python_version(),'binary_sha256':hashlib.sha256(Path(args.bin).read_bytes()).hexdigest(),'harness_sha256':hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),'timestamp_utc':time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime()),'authenticated':True,'model':'closed-loop load; 100Hz scheduled independent probe; probe scheduled latency includes queueing'}
    out.with_suffix('.metadata.json').write_text(json.dumps(metadata,indent=2)+'\n')
    failures=0
    with (gzip.open(out, 'wt') if out.suffix == '.gz' else out.open('w')) as file:
        writer=csv.writer(file); writer.writerow(['policy','rep','role','command','scheduled_seconds','service_ms','scheduled_ms','error'])
        for rep in range(args.reps):
            for policy in args.policies:
                try: rows=run(args,policy,rep)
                except RunFailure as exc:
                    writer.writerows(exc.rows or [[policy,rep,'setup','INIT',0,0,0,f'{type(exc).__name__}: {exc}']])
                    file.flush(); raise
                except Exception as exc:
                    writer.writerow([policy,rep,'setup','INIT',0,0,0,f'{type(exc).__name__}: {exc}'])
                    file.flush(); raise
                writer.writerows(rows); file.flush()
                for role in ['load','probe']:
                    selected=[r for r in rows if r[2]==role]; values=sorted(r[6] for r in selected)
                    errors=sum(bool(r[7]) for r in selected); failures+=errors
                    percentiles={str(p):round(values[min(len(values)-1,int((len(values)-1)*p))],3) for p in [.5,.95,.99,.999]}
                    print(json.dumps({'policy':policy,'rep':rep,'role':role,'attempts':len(values),'errors':errors,'scheduled_latency_ms':percentiles}),flush=True)
    if failures: raise SystemExit(f'{failures} failed attempts; retained in CSV')
if __name__=='__main__': main()
