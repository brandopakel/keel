#!/usr/bin/env python3
"""Exercise redis-py's RESP2 callbacks, binary values, ordinary pipelines and transactions."""
import json
import os
from pathlib import Path
import sys
import time
import redis

fixture = json.loads(Path(sys.argv[1]).read_text())
host, port = os.environ['KEEL_COMPAT_ADDR'].rsplit(':', 1)


def connect():
    return redis.Redis(host=host, port=int(port), protocol=2, decode_responses=False,
                       password=os.environ.get('KEEL_COMPAT_PASSWORD') or None,
                       socket_connect_timeout=3, socket_timeout=3)


def argument(value):
    return bytes.fromhex(value['hex']) if isinstance(value, dict) else value


def normalize(value):
    if isinstance(value, bytes):
        try:
            return value.decode('utf-8')
        except UnicodeDecodeError:
            return {'hex': value.hex()}
    if isinstance(value, (tuple, list)):
        return [normalize(x) for x in value]
    if isinstance(value, bool):
        return int(value)
    return value


def check_case(case):
    args = [argument(x) for x in case['args']]
    try:
        result = client.execute_command(*args)
    except redis.ResponseError as exc:
        assert case.get('error') and case['error'] in str(exc), (case['name'], str(exc))
        return
    assert 'error' not in case, case['name']
    if args[0] == 'PING' and result is True:
        result = 'PONG'
    if args[0] == 'SET' and result is True:
        result = 'OK'
    result = normalize(result)
    if 'range' in case:
        assert case['range'][0] <= result <= case['range'][1], (case['name'], result)
    else:
        assert result == case['expected'], (case['name'], result, case['expected'])


def check_transaction(x):
    """redis-py's pipeline() is a MULTI/EXEC transaction unless told otherwise."""
    with client.pipeline() as pipeline:
        pipeline.set(x['string_key'], argument(x['value']))
        pipeline.incr(x['counter_key'])
        pipeline.lpush(x['string_key'], 'x')
        pipeline.get(x['string_key'])
        results = pipeline.execute(raise_on_error=False)
    assert results[0] is True and results[1] == 1, ('transaction', results)
    assert isinstance(results[2], redis.ResponseError) and 'WRONGTYPE' in str(results[2]), ('transaction', results)
    assert normalize(results[3]) == x['value'], ('transaction', results)
    with client.pipeline() as pipeline:
        pipeline.set(x['aborted_key'], 'never')
        pipeline.execute_command('NOSUCHCOMMAND')
        try:
            pipeline.execute()
        except redis.ResponseError as exc:
            assert 'unknown command' in str(exc) or 'EXECABORT' in str(exc), ('aborted transaction', str(exc))
        else:
            raise AssertionError('aborted transaction executed')


client = connect()
try:
    if not fixture['verify_only']:
        for case in fixture['commands']:
            check_case(case)
        deadline = time.monotonic() + 3
        while client.get(fixture['expiry_key']) is not None:
            assert time.monotonic() < deadline, 'expiry did not occur'
            time.sleep(.01)
        with client.pipeline(transaction=False) as pipeline:
            for _ in range(257):
                pipeline.incr(fixture['counter_key'])
            assert pipeline.execute() == list(range(1, 258)), 'pipeline order'
        assert client.get(fixture['counter_key']) == b'257'
        found = set(client.scan_iter(match=fixture['prefix']+'*', count=7, _type='STRING'))
        assert found == {x.encode() for x in fixture['scan_keys']}, found
        check_transaction(fixture['transaction'])
    for case in fixture['verification']:
        check_case(case)
    client.close()
    client.connection_pool.disconnect()
    client = connect()
    assert normalize(client.get(fixture['marker_key'])) == fixture['marker_value'], 'reconnect'
    print(json.dumps({'library': 'redis-py', 'version': redis.__version__, 'status': 'passed',
                      'verify_only': fixture['verify_only']}))
finally:
    client.close()
    client.connection_pool.disconnect()
