"""AWS DLT-compatible RESP workload; also runs with standalone Locust.
Traffic Shape in DLT overrides local user count/ramp/duration. Closed-loop load.
"""
import json
import os
import time
import uuid
from pathlib import Path
from locust import User, task, constant, events
from locust.exception import StopUser
from resp_client import Client

config_path=Path(__file__).with_name('target.json')
config=json.loads(config_path.read_text()) if config_path.exists() else {}

class KeelUser(User):
    wait_time=constant(0)
    def on_start(self):
        self.client=None
        self.prefix='locust-'+uuid.uuid4().hex
        self.iteration=0
        start=time.perf_counter();error=None
        try:
            self.client=Client(os.environ.get('KEEL_HOST',config.get('host','127.0.0.1')),int(os.environ.get('KEEL_PORT',config.get('port',6379))),os.environ.get('KEEL_PASSWORD'),os.environ.get('KEEL_TLS','1' if config.get('tls') else '0')=='1')
        except Exception as exc: error=exc
        self.environment.events.request.fire(request_type='RESP',name='CONNECT_AUTH',response_time=(time.perf_counter()-start)*1000,response_length=0,exception=error,context={})
        if error: raise StopUser()
    def on_stop(self):
        if self.client: self.client.close()
    def measured(self,*parts,expected):
        start=time.perf_counter();error=None;reply=None
        try:
            reply=self.client.call(*parts)
            if reply!=expected: raise ValueError('reply mismatch')
        except Exception as exc: error=exc
        self.environment.events.request.fire(request_type='RESP',name=parts[0],response_time=(time.perf_counter()-start)*1000,response_length=len(reply) if isinstance(reply,bytes) else 0,exception=error,context={})
        if error: raise StopUser() # discard a potentially desynchronized connection
    @task
    def cache(self):
        # A bounded hot set per user, unique across distributed workers.
        key=f'{self.prefix}:{self.iteration%1000}'
        self.measured('SET',key,b'v'*256,'PX',60000,expected=b'OK')
        self.measured('GET',key,expected=b'v'*256)
        self.iteration+=1

@events.quitting.add_listener
def fail_on_errors(environment,**kwargs):
    if environment.stats.total.num_failures or not environment.stats.total.num_requests:
        environment.process_exit_code=1
