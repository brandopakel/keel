#!/usr/bin/env python3
"""Real TCP pipeline ordering, concurrent counters and acknowledged crash recovery."""
import argparse
import concurrent.futures
import json
import os
import threading
import time
from pathlib import Path
from validation_lib import Client, Server, info, rewrite, sha256


def encode(*parts):
    values=[p if isinstance(p,bytes) else str(p).encode() for p in parts]
    return b'*%d\r\n'%len(values)+b''.join(b'$%d\r\n'%len(v)+v+b'\r\n' for v in values)


def run(args):
    os.umask(0o077)
    root=args.out.resolve();root.mkdir(parents=True,exist_ok=False)
    report={'status':'running','binary_sha256':sha256(args.bin),'policy':args.policy,
            'clients':args.clients,'iterations_per_client':args.iterations,'concurrent':True}
    server=Server(args.bin,root/'server',policy=args.policy,async_append=True,extra=['-aof-concurrent-append'])
    try:
        server.start()
        barrier=threading.Barrier(args.clients)
        def client_work(worker):
            c=Client('127.0.0.1',server.port,server.password)
            key=f'worker:{worker}';seen=[]
            try:
                barrier.wait(timeout=10)
                for i in range(args.iterations):
                    value=f'{worker}:{i}:'.encode()+b'x'*16384
                    # The read must return this connection's preceding write,
                    # even while other clients have pending append/reply batches.
                    c.socket.sendall(encode('SET',key,value)+encode('GET',key)+encode('INCR','shared'))
                    assert c.read()==b'OK'
                    assert c.read()==value
                    seen.append(c.read())
                return seen
            finally:c.close()
        began=time.monotonic()
        with concurrent.futures.ThreadPoolExecutor(max_workers=args.clients) as pool:
            seen=[v for result in pool.map(client_work,range(args.clients)) for v in result]
        total=args.clients*args.iterations
        assert sorted(seen)==list(range(1,total+1)), 'duplicate or missing shared counter result'
        report.update(acknowledged_batches=total,commands=total*3,seconds=time.monotonic()-began)
        rewrite(server.client)
        report['persistence']=info(server.client,'persistence')
        for restart in range(2):
            server.stop(crash=True);server.start()
            assert server.client.call('GET','shared')==str(total).encode()
            for worker in range(args.clients):
                assert server.client.call('GET',f'worker:{worker}')==f'{worker}:{args.iterations-1}:'.encode()+b'x'*16384
        report.update(status='passed',crash_restarts=2,acknowledged_values_lost=0)
    except BaseException as exc:
        report.update(status='failed',failure=repr(exc));raise
    finally:
        server.stop(check=False)
        (root/'report.json').write_text(json.dumps(report,indent=2)+'\n')
        print(json.dumps(report,indent=2))

if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--bin',type=Path,required=True);p.add_argument('--out',type=Path,required=True)
    p.add_argument('--policy',choices=['always','everysec','no'],default='always')
    p.add_argument('--clients',type=int,default=32);p.add_argument('--iterations',type=int,default=250)
    args=p.parse_args()
    if not 1<=args.clients<=256 or not 1<=args.iterations<=100000:p.error('clients must be 1..256 and iterations 1..100000')
    run(args)
