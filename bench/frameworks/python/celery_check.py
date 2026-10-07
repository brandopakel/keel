"""Celery against the server at REDIS_URL, as its broker and its result
backend: run a worker, send it a task, a delayed task and a group, and read
their results back."""
import os
import signal
import sys
import traceback

from celery import Celery, group
from celery.contrib.testing.worker import start_worker

url = os.environ['REDIS_URL']
app = Celery('check', broker=url, backend=url)
# Fail rather than retry forever when the server refuses something.
app.conf.update(broker_connection_retry_on_startup=False, broker_connection_max_retries=0, result_expires=60)


@app.task
def add(x, y):
    return x + y


def give_up(*_):
    # A worker that died on a refused command leaves the results unanswered,
    # and stopping it can wait as long; this ends the check either way.
    print('timed out', file=sys.stderr, flush=True)
    os._exit(1)


signal.signal(signal.SIGALRM, give_up)
signal.alarm(60)
try:
    with start_worker(app, pool='solo', perform_ping_check=False, shutdown_timeout=10):
        assert add.delay(2, 3).get(timeout=20) == 5
        assert add.apply_async((4, 5), countdown=1).get(timeout=20) == 9
        assert group(add.s(i, i) for i in range(5))().get(timeout=20) == [0, 2, 4, 6, 8]
except BaseException:
    traceback.print_exc()
    sys.stderr.flush()
    os._exit(1)
print('ok: a task, a delayed task and a group ran', flush=True)
os._exit(0)
