#!/usr/bin/env python3
"""Ask Redis for every command it has, and Keel whether it has each one.

Redis lists its commands, with each one's group, the version it arrived in and
its subcommands, in COMMAND DOCS. This script reads that list from a running
Redis, then asks a running Keel for every command and every subcommand, each
on a fresh connection, with no arguments:

- ERR unknown command, or ERR unknown subcommand, means Keel does not have it;
- any other reply, an argument count error included, means it does.

Keel answers an unknown command or subcommand with Redis's error before it
counts arguments (docs/error-replies.md), so this tells "absent" from "present"
without running anything. Options within a command (SET's KEEPTTL, ZADD's GT)
are not checked; a command counts as present when Keel knows its name.

Redis is asked the same way, as a control: the method is sound only if Redis
reports every command of its own as present. Any that it does not are listed,
and fail the run.

A few commands do something even with no arguments. Those are sent with
arguments that make them do nothing: SHUTDOWN ABORT instead of SHUTDOWN, for
instance. Both servers are disposable instances.

It writes census.json (every command and subcommand, with its group, its
module when it comes from one, and whether Keel has it) and census.md (the
summary by group), and prints the summary.
"""
import argparse
import json
from pathlib import Path
import re
import socket
import sys

# Sent instead of the bare name. SHUTDOWN ABORT cannot stop the server. SYNC
# with an argument is refused for its count at once; bare, it starts a
# replica's full sync, which Redis delays by repl-diskless-sync-delay.
SAFE_PROBES = {
    'shutdown': ['SHUTDOWN', 'ABORT'],
    'sync': ['SYNC', 'probe'],
}
# Probed last, in case a server ignores the arguments above.
LAST = ['shutdown']

UNKNOWN = re.compile(r'^ERR unknown (command|subcommand)\b', re.I)


class Error(str):
    """A RESP error reply."""


def encode(parts):
    out = [f'*{len(parts)}\r\n'.encode()]
    for part in parts:
        data = part if isinstance(part, bytes) else str(part).encode()
        out.append(b'$%d\r\n%s\r\n' % (len(data), data))
    return b''.join(out)


def read(stream):
    """One RESP2 or RESP3 reply. Maps become lists of alternating keys and values,
    as RESP2 sends them, so that COMMAND DOCS reads the same either way."""
    line = stream.readline()
    if not line.endswith(b'\r\n'):
        raise ConnectionError('connection closed mid-reply')
    kind, body = line[:1], line[1:-2]
    if kind == b'+':
        return body.decode()
    if kind == b'-':
        return Error(body.decode('utf-8', 'replace'))
    if kind in (b':', b','):
        return body.decode()
    if kind == b'_':
        return None
    if kind == b'#':
        return body == b't'
    if kind in (b'$', b'=', b'!'):
        size = int(body)
        if size < 0:
            return None
        data = stream.read(size + 2)[:-2]
        return Error(data.decode('utf-8', 'replace')) if kind == b'!' else data.decode('utf-8', 'replace')
    if kind in (b'*', b'~', b'>'):
        count = int(body)
        return None if count < 0 else [read(stream) for _ in range(count)]
    if kind == b'%':
        return [read(stream) for _ in range(2 * int(body))]
    raise ConnectionError(f'unexpected reply type {kind!r}')


def call(host, port, parts, timeout=3):
    """Send one command on a fresh connection and return its reply."""
    with socket.create_connection((host, port), timeout=timeout) as s:
        s.sendall(encode(parts))
        with s.makefile('rb') as stream:
            return read(stream)


def pairs(flat):
    return dict(zip(flat[::2], flat[1::2]))


def parse_docs(reply):
    """COMMAND DOCS as a list of commands, each with its subcommands."""
    commands = []
    for name, doc in pairs(reply).items():
        doc = pairs(doc)
        entry = {'name': name.lower(), 'group': doc.get('group', ''), 'module': doc.get('module', ''),
                 'since': doc.get('since', ''), 'deprecated': 'deprecated' in (doc.get('doc_flags') or []),
                 'subcommands': []}
        for sub, subdoc in pairs(doc.get('subcommands') or []).items():
            subdoc = pairs(subdoc)
            entry['subcommands'].append({'name': sub.lower(), 'group': subdoc.get('group', entry['group']),
                                         'since': subdoc.get('since', ''),
                                         'deprecated': 'deprecated' in (subdoc.get('doc_flags') or [])})
        entry['subcommands'].sort(key=lambda s: s['name'])
        commands.append(entry)
    return sorted(commands, key=lambda c: (c['name'] in LAST, c['name']))


def probe_parts(name):
    """What to send to ask for a command or a container|sub subcommand."""
    if name in SAFE_PROBES:
        return SAFE_PROBES[name]
    return [part.upper() for part in name.split('|')]


def classify(reply):
    if isinstance(reply, Error) and UNKNOWN.match(reply):
        return 'missing'
    return 'present'


def probe(host, port, name):
    try:
        reply = call(host, port, probe_parts(name))
    except (OSError, ConnectionError, ValueError) as e:
        return 'no reply', f'{type(e).__name__}: {e}'
    return classify(reply), (reply[:120] if isinstance(reply, str) else type(reply).__name__)


def census(redis, keel, commands):
    """Probe both servers for every command and subcommand; returns the entries,
    each with keel, redis (the control) and keel_reply."""
    rows = []
    for command in commands:
        for item, kind in [(command, 'command')] + [(s, 'subcommand') for s in command['subcommands']]:
            keel_state, keel_reply = probe(*keel, item['name'])
            redis_state, _ = probe(*redis, item['name'])
            rows.append({'name': item['name'], 'kind': kind, 'group': item['group'], 'module': command['module'],
                         'since': item['since'], 'deprecated': item['deprecated'], 'keel': keel_state,
                         'redis': redis_state, 'keel_reply': keel_reply})
    return rows


def summarise(rows):
    """Counts per area: a module's commands under the module's name, the rest by group."""
    areas = {}
    for row in rows:
        area = f"module:{row['module']}" if row['module'] else row['group']
        counts = areas.setdefault(area, {'commands': 0, 'commands_present': 0,
                                         'subcommands': 0, 'subcommands_present': 0})
        key = 'commands' if row['kind'] == 'command' else 'subcommands'
        counts[key] += 1
        counts[key + '_present'] += row['keel'] == 'present'
    total = {k: sum(a[k] for a in areas.values()) for k in
             ('commands', 'commands_present', 'subcommands', 'subcommands_present')}
    return {'areas': dict(sorted(areas.items())), 'total': total}


def markdown(summary, rows, redis_version):
    t = summary['total']
    out = [f"Keel has **{t['commands_present']} of Redis {redis_version}'s {t['commands']} commands** and "
           f"{t['subcommands_present']} of their {t['subcommands']} subcommands.", '',
           '| Area | Commands | Subcommands | Missing commands |', '| --- | ---: | ---: | --- |']
    for area, a in summary['areas'].items():
        missing = [r['name'] for r in rows if r['kind'] == 'command' and r['keel'] == 'missing' and
                   (f"module:{r['module']}" if r['module'] else r['group']) == area]
        subs = f"{a['subcommands_present']}/{a['subcommands']}" if a['subcommands'] else '-'
        out.append(f"| {area} | {a['commands_present']}/{a['commands']} | {subs} | "
                   f"{', '.join(f'`{m}`' for m in missing) or '-'} |")
    out += ['', 'A command counts as present when Keel knows its name; its options are not checked.']
    return '\n'.join(out) + '\n'


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--redis', default='127.0.0.1:6392', help='host:port of the Redis to list commands from')
    ap.add_argument('--keel', default='127.0.0.1:6391', help='host:port of the Keel to ask')
    ap.add_argument('--out', type=Path, required=True)
    args = ap.parse_args(argv)
    redis = args.redis.rsplit(':', 1)
    keel = args.keel.rsplit(':', 1)
    redis, keel = (redis[0], int(redis[1])), (keel[0], int(keel[1]))

    info = call(*redis, ['INFO', 'server'])
    version = re.search(r'redis_version:(\S+)', info)[1]
    commands = parse_docs(call(*redis, ['COMMAND', 'DOCS'], timeout=10))
    rows = census(redis, keel, commands)
    summary = summarise(rows)
    unsound = [r['name'] for r in rows if r['redis'] != 'present']
    unreachable = [r['name'] for r in rows if r['keel'] == 'no reply']
    result = {'redis_version': version, 'summary': summary, 'control_failures': unsound,
              'keel_no_reply': unreachable, 'commands': rows}
    args.out.mkdir(parents=True, exist_ok=True)
    (args.out / 'census.json').write_text(json.dumps(result, indent=2) + '\n')
    text = markdown(summary, rows, version)
    if unsound or unreachable:
        text += '\n**FAILED**\n\n'
        text += ''.join(f'- Redis did not report its own `{n}` as present\n' for n in unsound)
        text += ''.join(f'- Keel gave no reply to `{n}`\n' for n in unreachable)
    (args.out / 'census.md').write_text(text)
    print(text)
    return 1 if unsound or unreachable else 0


if __name__ == '__main__':
    sys.exit(main())
