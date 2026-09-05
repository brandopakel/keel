#!/usr/bin/env python3
"""Run actual provider adapters against an isolated authenticated Keel process."""
import argparse,os,socket,subprocess,tempfile,time,sys
from pathlib import Path
sys.path.insert(0,str(Path(__file__).parent/'aws'))
from resp_client import Client
p=argparse.ArgumentParser();p.add_argument('--bin',required=True);p.add_argument('--k6');p.add_argument('--locust');a=p.parse_args()
if not a.k6 and not a.locust:p.error('provide at least one adapter executable')
with socket.socket() as s:s.bind(('127.0.0.1',0));port=s.getsockname()[1]
env=dict(os.environ,KEEL_HOST='127.0.0.1',KEEL_PORT=str(port),KEEL_PASSWORD='local-adapter-test',DURATION='2s',RATE='5',BATCH='4',VUS='2',MAX_VUS='4')
root=Path(__file__).resolve().parent
with tempfile.TemporaryDirectory(prefix='keel-adapters-') as directory:
    with open(directory+'/server.log','w+') as log:
        server=subprocess.Popen([a.bin,'-port',str(port),'-requirepass-env','KEEL_PASSWORD','-appendonly','-appendfilename',directory+'/log.aof'],env=env,stdout=log,stderr=log)
        try:
            deadline=time.monotonic()+5
            while True:
                try:c=Client('127.0.0.1',port,env['KEEL_PASSWORD']);break
                except OSError:
                    if time.monotonic()>deadline:raise RuntimeError('server startup timeout')
                    time.sleep(.01)
            assert c.call('PING')==b'PONG';c.close()
            if a.k6:subprocess.run([a.k6,'run','--out','json='+directory+'/k6.json',str(root/'k6/keel.js')],env=env,check=True,timeout=30)
            if a.locust:subprocess.run([a.locust,'-f',str(root/'aws/locustfile.py'),'--headless','-u','2','-r','2','-t','2s','--csv',directory+'/locust'],env=env,check=True,timeout=30)
        finally:
            server.terminate()
            try:server.wait(timeout=7)
            except subprocess.TimeoutExpired:server.kill();server.wait();raise
            if server.returncode:log.seek(0);raise RuntimeError(log.read())
