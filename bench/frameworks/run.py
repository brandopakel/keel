#!/usr/bin/env python3
"""Run real frameworks against Redis and Keel, and record what each one needs.

Each check (ruby/*_check.rb, python/*_check.py, node/*_check.mjs) is a small,
real workload for one framework: Sidekiq's jobs, Rails' cache store,
ActionCable's broadcasts, Celery's tasks, BullMQ's queue. It reads the server
from REDIS_URL, exits 0 when the workload did what it should, and prints why
not otherwise.

Every check runs against Redis first, as the control: a check Redis fails is a
broken check, not a finding, and fails this run. Before each control run the
command statistics are reset, and afterwards INFO commandstats lists every
command the framework sent. The check then runs against Keel. For each
framework this records whether Keel passed, the first command Keel refused as
unknown, and which of the commands the framework sent Keel does not have,
asked the way the command census asks (scripts/command-census.py).

It writes frameworks.json and frameworks.md under --out, with each run's
output, and prints the summary. The exit status is 1 only when a control
failed.
"""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('command_census', HERE.parents[1] / 'scripts' / 'command-census.py')
census = importlib.util.module_from_spec(spec)
spec.loader.exec_module(census)

UNKNOWN = re.compile(r"unknown command [`'](\S+?)[`']", re.I)
# What this script sends itself, which is not the framework's.
OWN = {'config|resetstat'}


def checks(python, ruby_bundle):
    """Each framework's check: its name and the command that runs it."""
    rb = lambda name: [*ruby_bundle, 'exec', 'ruby', str(HERE / 'ruby' / f'{name}_check.rb')]
    return [
        ('Rails cache store', rb('rails_cache')),
        ('ActionCable', rb('actioncable')),
        ('Sidekiq', rb('sidekiq')),
        ('Celery', [python, str(HERE / 'python' / 'celery_check.py')]),
        ('BullMQ', ['node', str(HERE / 'node' / 'bullmq_check.mjs')]),
    ]


def first_unknown(output):
    m = UNKNOWN.search(output)
    return m[1].lower() if m else ''


def commandstats(text):
    """INFO commandstats as {command: calls}, subcommands written container|sub."""
    used = {}
    for line in text.splitlines():
        m = re.match(r'^cmdstat_([^:]+):calls=(\d+)', line)
        if m and m[1] not in OWN:
            used[m[1]] = int(m[2])
    return used


def run_check(command, url, out, cwd, timeout=120):
    began = time.monotonic()
    env = {**os.environ, 'REDIS_URL': url}
    try:
        done = subprocess.run(command, env=env, cwd=cwd, capture_output=True, text=True, timeout=timeout)
        output, code = done.stdout + done.stderr, done.returncode
    except subprocess.TimeoutExpired as e:
        output = (e.stdout or b'').decode(errors='replace') + (e.stderr or b'').decode(errors='replace')
        output, code = output + f'\nkilled after {timeout} s', 'timeout'
    out.write_text(output)
    return {'passed': code == 0, 'exit': code, 'seconds': round(time.monotonic() - began, 1),
            'first_unknown': first_unknown(output), 'last_line': (output.strip().splitlines() or [''])[-1][:200]}


def markdown(results):
    out = ['| Framework | Redis (control) | Keel | First command Keel refused | Commands it sent | Of those, Keel lacks |',
           '| --- | --- | --- | --- | ---: | --- |']
    for r in results:
        lacks = ', '.join(f'`{c}`' for c in r['keel_lacks']) or '-'
        out.append(f"| {r['framework']} | {'pass' if r['redis']['passed'] else '**FAIL**'} | "
                   f"{'pass' if r['keel']['passed'] else 'fail'} | "
                   f"{('`' + r['keel']['first_unknown'] + '`') if r['keel']['first_unknown'] else '-'} | "
                   f"{len(r['commands_used'])} | {lacks} |")
    passed = sum(r['keel']['passed'] for r in results)
    out += ['', f'Keel passes {passed} of {len(results)} frameworks. The control is Redis: a framework it '
            'fails is a broken check, and fails this run.']
    return '\n'.join(out) + '\n'


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--redis', default='127.0.0.1:6392')
    ap.add_argument('--keel', default='127.0.0.1:6391')
    ap.add_argument('--python', default=sys.executable, help='the interpreter with python/requirements.txt installed')
    ap.add_argument('--bundle', default='bundle', help="Bundler, with ruby/Gemfile's gems installed")
    ap.add_argument('--only', action='append', help='run only this framework (repeatable)')
    ap.add_argument('--out', type=Path, required=True)
    args = ap.parse_args(argv)
    args.out.mkdir(parents=True, exist_ok=True)
    redis = args.redis.rsplit(':', 1)
    keel = args.keel.rsplit(':', 1)
    redis, keel = (redis[0], int(redis[1])), (keel[0], int(keel[1]))
    os.environ.setdefault('BUNDLE_GEMFILE', str(HERE / 'ruby' / 'Gemfile'))

    results = []
    for name, command in checks(args.python, [args.bundle]):
        if args.only and name not in args.only:
            continue
        slug = re.sub(r'\W+', '-', name.lower())
        census.call(*redis, ['CONFIG', 'RESETSTAT'])
        control = run_check(command, f'redis://{redis[0]}:{redis[1]}/0', args.out / f'{slug}-redis.log', HERE)
        used = commandstats(census.call(*redis, ['INFO', 'commandstats']))
        census.call(*redis, ['FLUSHALL'])
        candidate = run_check(command, f'redis://{keel[0]}:{keel[1]}/0', args.out / f'{slug}-keel.log', HERE)
        census.call(*keel, ['FLUSHDB'])
        lacks = sorted(c for c in used if census.probe(*keel, c)[0] != 'present')
        results.append({'framework': name, 'redis': control, 'keel': candidate, 'commands_used': used,
                        'keel_lacks': lacks})
        print(f"{name}: Redis {'pass' if control['passed'] else 'FAIL'}, Keel "
              f"{'pass' if candidate['passed'] else 'fail ' + (candidate['first_unknown'] or candidate['last_line'])}",
              file=sys.stderr, flush=True)

    broken = [r['framework'] for r in results if not r['redis']['passed']]
    (args.out / 'frameworks.json').write_text(json.dumps({'results': results, 'broken_checks': broken}, indent=2) + '\n')
    text = markdown(results)
    if broken:
        text += '\n**FAILED**: the control failed for ' + ', '.join(broken) + '; see their logs.\n'
    (args.out / 'frameworks.md').write_text(text)
    print(text)
    return 1 if broken else 0


if __name__ == '__main__':
    sys.exit(main())
