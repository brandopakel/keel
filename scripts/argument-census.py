#!/usr/bin/env python3
"""Send every documented argument form of the commands Keel has to Keel and to
Redis, and compare the replies.

The command census (scripts/command-census.py) asks whether Keel knows each
command's name. This census asks whether Keel takes each command's documented
arguments: for every command and subcommand Keel has, every argument form in
Redis's own command specifications (src/commands/*.json in the Redis source,
read as data by scripts/redis_commands.py): the base form, each optional
argument, each alternative of each choice, each repeated argument given twice,
including the options Redis 8 added.

Each form is sent on a fresh connection to Keel and to Redis, after both are
flushed and given the same small fixture (a string, a list, a hash, a set, a
sorted set, a geo set and a HyperLogLog, each with known contents), and the
replies are compared. A form is:

- accepted: Keel's reply is Redis's. Replies whose contents legitimately differ
  between servers (HELLO, INFO, CLIENT ID, MEMORY USAGE, a random member) are
  compared by shape; unordered replies (SMEMBERS, HGETALL, KEYS) are sorted.
- refused: Keel answers an error where Redis answers a value. The error is
  recorded; a syntax, argument-count or unknown-option error means the form is
  not parsed, another error that it is parsed and then refused.
- wrong reply: both answer, differently; or both answer an error, differently.

Redis is the control: a form Redis itself refuses is a flaw in this script's
choice of values, not a finding about Keel. Those forms are listed as control
failures and fail the run, as does a form Keel gives no reply to. Commands
that would stop, block or reconfigure a server (SHUTDOWN, SYNC, REPLICAOF,
DEBUG, CLIENT KILL and the like) are not sent; they are listed as not probed.

Both servers run with a password and the append-only file on, so that AUTH,
HELLO AUTH and BGREWRITEAOF have their documented replies. The script starts
them itself from --bin and --redis-server, in directories under --out.

It writes arguments.json (every form with both replies and its class, and the
counts by area) and arguments.md (the summary by area and every refused or
wrong form), and prints the summary.
"""
import argparse
import json
import os
from pathlib import Path
import re
import secrets
import socket
import subprocess
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parent))
import redis_commands as rc  # noqa: E402

# Replies compared by shape: their contents name a server, a connection, a
# moment or a random choice. A subcommand's container covers every subcommand.
LOOSE = {'hello', 'info', 'client', 'memory', 'config|get', 'config|help', 'bgrewriteaof', 'lolwut', 'time',
         'lastsave', 'object', 'command', 'slowlog', 'latency', 'role', 'cluster', 'srandmember', 'spop',
         'hrandfield', 'zrandmember', 'randomkey', 'scan', 'sscan', 'hscan', 'zscan', 'acl|getuser', 'acl|list',
         'acl|log', 'module|list', 'wait', 'waitaof', 'function|stats', 'dump', 'digest'}
# Replies whose elements come in no defined order.
UNORDERED = {'smembers', 'sinter', 'sunion', 'sdiff', 'hkeys', 'hvals', 'keys', 'hgetall', 'pubsub|channels',
             'pubsub|shardchannels', 'smismember'}
PAIRED = {'hgetall'}  # RESP2 alternating keys and values: sorted as pairs.

SYNTAX = re.compile(r'^ERR (syntax error|wrong number of arguments|unknown subcommand|unknown command|'
                    r'[Uu]nsupported option|[Uu]nknown option)')
# A command name without arguments asks whether a server has it; a few need
# arguments that make them do nothing (as in command-census.py).
SAFE_PROBES = {'shutdown': ['SHUTDOWN', 'ABORT'], 'sync': ['SYNC', 'probe']}


class Connection:
    def __init__(self, port, password, timeout=5):
        self.socket = socket.create_connection(('127.0.0.1', port), timeout=timeout)
        self.stream = self.socket.makefile('rb')
        if password is not None:
            reply = self.call(['AUTH', password])
            if reply != ('+', 'OK'):
                raise RuntimeError(f'AUTH answered {rc.text(reply)}')

    def call(self, parts):
        self.socket.sendall(rc.encode(parts))
        return rc.read_reply(self.stream)

    def close(self):
        self.stream.close()
        self.socket.close()


def free_port():
    with socket.socket() as reservation:
        reservation.bind(('127.0.0.1', 0))
        return reservation.getsockname()[1]


class Servers:
    """A Keel and a Redis, each with a password and the log on, started under
    the output directory and stopped at exit."""

    def __init__(self, out):
        self.out = out
        self.password = secrets.token_hex(16)
        self.processes = []
        self.env = {k: os.environ[k] for k in ('PATH', 'HOME', 'TMPDIR') if k in os.environ}

    def start_keel(self, binary):
        directory = self.out / 'keel'
        directory.mkdir(parents=True)
        port = free_port()
        env = dict(self.env, KEEL_ARGUMENT_CENSUS_PASSWORD=self.password)
        log = (directory / 'server.log').open('wb')
        command = [str(Path(binary).resolve()), '-host', '127.0.0.1', '-port', str(port), '-requirepass-env',
                   'KEEL_ARGUMENT_CENSUS_PASSWORD', '-appendonly', '-appendfilename', str(directory / 'store.aof')]
        process = subprocess.Popen(command, cwd=directory, env=env, stdout=log, stderr=log)
        self.processes.append((process, log))
        self.wait(process, port, directory / 'server.log')
        return port

    def start_redis(self, binary):
        directory = self.out / 'redis'
        directory.mkdir(parents=True)
        port = free_port()
        log = (directory / 'server.log').open('wb')
        process = subprocess.Popen([str(Path(binary).resolve()), '-'], cwd=directory, env=self.env,
                                   stdin=subprocess.PIPE, stdout=log, stderr=log)
        config = ['bind 127.0.0.1', f'port {port}', 'save ""', 'appendonly yes', f'dir {directory}',
                  f'requirepass {self.password}', 'enable-debug-command no']
        process.stdin.write(('\n'.join(config) + '\n').encode())
        process.stdin.close()
        self.processes.append((process, log))
        self.wait(process, port, directory / 'server.log')
        return port

    def wait(self, process, port, log):
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            if process.poll() is not None:
                raise RuntimeError(f'server exited during startup; see {log}')
            try:
                c = Connection(port, self.password, timeout=2)
                try:
                    if c.call(['PING']) == ('+', 'PONG'):
                        return
                finally:
                    c.close()
            except (OSError, RuntimeError, ValueError, ConnectionError):
                pass
            time.sleep(.05)
        raise TimeoutError(f'server on {port} not ready; see {log}')

    def stop(self):
        for process, log in self.processes:
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=8)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
            log.close()


def probe_present(port, password, specs):
    """Which commands and subcommands a server knows, by name: an unknown
    command or subcommand error means it does not, any other reply means it
    does."""
    present = set()
    for name, spec in specs.items():
        if spec['is_container'] or name in rc.UNSAFE:
            continue
        parts = SAFE_PROBES.get(name) or [w.upper() for w in name.split('|')]
        c = Connection(port, password)
        try:
            reply = c.call(parts)
        finally:
            c.close()
        if not (reply[0] == '-' and rc.UNKNOWN.match(reply[1])):
            present.add(name)
    return present


class Side:
    """One server: an admin connection that resets the fixture, and a fresh
    connection for every form."""

    def __init__(self, name, port, password, fixture):
        self.name, self.port, self.password = name, port, password
        self.admin = Connection(port, password)
        self.flush = ['FLUSHALL'] if 'flushall' in fixture.present else ['FLUSHDB']
        self.fixture = fixture.commands
        self.fixture_failures = {}

    def reset(self):
        self.admin.call(self.flush)
        for command in self.fixture:
            reply = self.admin.call(command)
            if reply[0] == '-':
                self.fixture_failures[' '.join(command)] = reply[1]
        self.settle()

    def settle(self):
        """Wait for a log rewrite the previous form started to finish, so that
        BGREWRITEAOF has its documented reply rather than 'already in
        progress'."""
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            info = self.admin.call(['INFO', 'persistence'])
            if info[0] == '-' or ('aof_rewrite_in_progress:1' not in str(info[1]) and
                                  'aof_rewrite_scheduled:1' not in str(info[1])):
                return
            time.sleep(.02)

    def send(self, prelude, argv):
        """The reply to argv on a fresh connection, after the prelude; None
        with the reason when there is no reply."""
        try:
            c = Connection(self.port, self.password)
        except OSError as e:
            return None, f'connect: {e}'
        try:
            for command in prelude:
                c.call(command)
            return c.call(argv), ''
        except (OSError, ConnectionError, ValueError) as e:
            return None, f'{type(e).__name__}: {e}'
        finally:
            c.close()


class Fixture:
    def __init__(self, present):
        self.present = present
        self.commands = rc.fixture_commands(present)


def loose(name):
    return name in LOOSE or name.split('|')[0] in LOOSE


def canonical(name, reply):
    """A reply with its undefined order removed: sorted, or paired and sorted."""
    kind, value = reply
    if kind in '*~' and value is not None and name in UNORDERED:
        if name in PAIRED and kind == '*':
            pairs = sorted(zip(value[::2], value[1::2]), key=lambda p: rc.text(p[0]))
            value = [v for pair in pairs for v in pair]
        else:
            value = sorted(value, key=lambda v: rc.text(v))
        return kind, value
    if kind == '%' and value is not None and name in UNORDERED:
        return kind, sorted(value, key=lambda p: rc.text(p[0]))
    return reply


def shape(reply):
    """A reply's outer type alone: an array, a map, a bulk string, an integer,
    a null. What a loose reply holds is the server's own (its name, its
    memory, a random member); whether it is the same kind of thing is what
    an argument form can be checked for here. The reply census compares
    shapes in full."""
    kind, value = reply
    return kind if value is not None or kind not in '$*=!~>%' else kind + '-1'


def classify(name, keel, redis):
    """The class of a form from the two replies, and a detail."""
    if keel is None:
        return 'no reply', ''
    if redis is None:
        return 'control', 'Redis gave no reply'
    keel_error, redis_error = keel[0] == '-', redis[0] == '-'
    if redis_error and keel_error:
        if keel[1] == redis[1]:
            return ('accepted', 'the documented reply is this error') if name in rc.EXPECT_ERROR else \
                ('control', 'Redis refused the form, and so did Keel')
        return 'wrong reply', 'a different error'
    if redis_error:
        return 'control', 'Redis refused the form'
    if keel_error:
        return 'refused', 'not parsed' if SYNTAX.match(keel[1]) else 'parsed, then refused'
    if canonical(name, keel) == canonical(name, redis):
        return 'accepted', ''
    if loose(name):
        return ('accepted', 'same kind of reply') if shape(keel) == shape(redis) else \
            ('wrong reply', 'a different kind of reply')
    return 'wrong reply', ''


def area_of(spec):
    return spec.get('group', '')


def census(specs, present, keel, redis, password):
    rows = []
    names = sorted(n for n in specs if not specs[n]['is_container'])
    for name in names:
        spec = specs[name]
        if name in rc.UNSAFE:
            rows.append({'command': name, 'area': area_of(spec), 'label': '', 'since': spec.get('since', ''),
                         'argv': [], 'class': 'not probed', 'detail': rc.UNSAFE[name], 'keel': '', 'redis': ''})
            continue
        if name not in present:
            continue
        for form in rc.forms_of(spec, password):
            keel.reset()
            redis.reset()
            prelude = rc.PRELUDE.get(name, [])
            keel_reply, keel_why = keel.send(prelude, form['argv'])
            redis_reply, redis_why = redis.send(prelude, form['argv'])
            cls, detail = classify(name, keel_reply, redis_reply)
            if cls == 'no reply':
                detail = keel_why
            rows.append({'command': name, 'area': area_of(spec), 'label': form['label'], 'since': form['since'],
                         'argv': [a.replace(password, '<password>') for a in form['argv']], 'class': cls,
                         'detail': detail, 'keel': rc.text(keel_reply, 400) if keel_reply else '',
                         'redis': rc.text(redis_reply, 400) if redis_reply else ''})
    return rows


CLASSES = ['accepted', 'refused', 'wrong reply', 'no reply', 'control', 'not probed']


def summarise(rows):
    """Counts by area and in total, over probed forms; the forms Redis 8
    added counted again apart."""
    areas = {}
    for row in rows:
        if row['class'] == 'not probed':
            continue
        a = areas.setdefault(row['area'], {'commands': set(), 'forms': 0, 'accepted': 0, 'refused': 0,
                                            'wrong_reply': 0, 'no_reply': 0, 'control': 0,
                                            'redis8_forms': 0, 'redis8_accepted': 0})
        a['commands'].add(row['command'])
        a['forms'] += 1
        a[row['class'].replace(' ', '_')] += 1
        if rc.version(row['since']) >= (8,):
            a['redis8_forms'] += 1
            a['redis8_accepted'] += row['class'] == 'accepted'
    for a in areas.values():
        a['commands'] = len(a['commands'])
    keys = ['commands', 'forms', 'accepted', 'refused', 'wrong_reply', 'no_reply', 'control', 'redis8_forms',
            'redis8_accepted']
    total = {k: sum(a[k] for a in areas.values()) for k in keys}
    return {'areas': dict(sorted(areas.items())), 'total': total}


def markdown(summary, rows, redis_version):
    t = summary['total']
    out = [f"Keel accepts **{t['accepted']} of the {t['forms']} documented argument forms** of the {t['commands']} "
           f"commands it has (Redis {redis_version}): {t['refused']} forms refused, {t['wrong_reply']} answered "
           f"differently. Of the {t['redis8_forms']} forms Redis 8 added, {t['redis8_accepted']} are accepted.", '',
           '| Area | Commands | Forms | Accepted | Refused | Wrong reply | Refused or wrong |',
           '| --- | ---: | ---: | ---: | ---: | ---: | --- |']
    for area, a in summary['areas'].items():
        gaps = {}
        for r in rows:
            if r['area'] == area and r['class'] in ('refused', 'wrong reply'):
                gaps.setdefault(r['command'], []).append(r['label'] or 'base')
        named = '; '.join(f"`{c}`: {', '.join(labels)}" for c, labels in sorted(gaps.items()))
        out.append(f"| {area} | {a['commands']} | {a['forms']} | {a['accepted']} | {a['refused']} | "
                   f"{a['wrong_reply']} | {named or '-'} |")
    out += ['', 'A form is a command with one documented option, alternative or repetition added to its base '
                'form, or the base form itself. Accepted means Keel answers as Redis does. Not covered: module '
                'commands (no specification in src/commands), and commands never sent (see not probed).', '']
    gaps = [r for r in rows if r['class'] in ('refused', 'wrong reply')]
    if gaps:
        out += ['| Command | Form | Since | Sent | Class | Keel | Redis |', '| --- | --- | --- | --- | --- | --- | --- |']
        for r in gaps:
            sent = ' '.join(r['argv'])
            out.append(f"| `{r['command']}` | {r['label'] or 'base'} | {r['since']} | `{sent}` | {r['class']}"
                       f"{' (' + r['detail'] + ')' if r['detail'] else ''} | `{cell(r['keel'])}` | `{cell(r['redis'])}` |")
        out.append('')
    return '\n'.join(out) + '\n'


def cell(s):
    return s.replace('|', '\\|').replace('`', "'")[:80]


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--bin', type=Path, required=True, help='the Keel binary')
    ap.add_argument('--redis-server', type=Path, required=True, help='the redis-server binary')
    ap.add_argument('--commands', type=Path, required=True, help="the Redis source's src/commands directory")
    ap.add_argument('--out', type=Path, required=True)
    args = ap.parse_args(argv)
    args.out.mkdir(parents=True, exist_ok=True)
    specs = rc.load_specs(args.commands)
    servers = Servers(args.out / 'servers')
    try:
        keel_port = servers.start_keel(args.bin)
        redis_port = servers.start_redis(args.redis_server)
        c = Connection(redis_port, servers.password)
        try:
            info = c.call(['INFO', 'server'])[1]
        finally:
            c.close()
        redis_version = re.search(r'redis_version:(\S+)', info)[1]
        present = probe_present(keel_port, servers.password, specs)
        redis_present = probe_present(redis_port, servers.password, specs)
        fixture = Fixture(present & redis_present)
        keel = Side('keel', keel_port, servers.password, fixture)
        redis = Side('redis', redis_port, servers.password, fixture)
        rows = census(specs, present, keel, redis, servers.password)
        fixture_failures = {'keel': keel.fixture_failures, 'redis': redis.fixture_failures}
    finally:
        servers.stop()
    summary = summarise(rows)
    control = [r for r in rows if r['class'] == 'control']
    no_reply = [r for r in rows if r['class'] == 'no reply']
    result = {'redis_version': redis_version, 'commands_dir': str(args.commands), 'summary': summary,
              'control_failures': [{k: r[k] for k in ('command', 'label', 'argv', 'detail', 'keel', 'redis')}
                                   for r in control],
              'keel_no_reply': [{k: r[k] for k in ('command', 'label', 'argv', 'detail')} for r in no_reply],
              'fixture_failures': fixture_failures, 'forms': rows}
    (args.out / 'arguments.json').write_text(json.dumps(result, indent=1) + '\n')
    text = markdown(summary, rows, redis_version)
    failed = control or no_reply or any(fixture_failures.values())
    if failed:
        text += '\n**FAILED**\n\n'
        text += ''.join(f"- Redis refused `{' '.join(r['argv'])}`: {r['redis']}\n" for r in control)
        text += ''.join(f"- Keel gave no reply to `{' '.join(r['argv'])}`: {r['detail']}\n" for r in no_reply)
        for side, failures in fixture_failures.items():
            text += ''.join(f'- {side} refused the fixture command `{c}`: {e}\n' for c, e in failures.items())
    (args.out / 'arguments.md').write_text(text)
    print(text)
    return 1 if failed else 0


if __name__ == '__main__':
    sys.exit(main())
