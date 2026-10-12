#!/usr/bin/env python3
"""Compare Redis's configuration directives with Keel's settings.

Redis keeps every configuration directive in one table, static_configs in
src/config.c (read here as data from the Redis source), and documents most of
them in redis.conf. Keel reports its settings under Redis's names through
CONFIG GET (docs/info-compatibility.md) and takes them as command-line flags.

For every directive in Redis's table this census records:

- whether redis.conf documents it, and Redis's own reply to CONFIG GET for it
  (the control: every directive in the table must answer; hidden ones answer
  by exact name);
- whether Keel answers CONFIG GET for it (has), has a flag for a setting
  that covers it in part or under another meaning (mapped, from MAPPED, with
  the flag checked against the binary's -help), or neither (lacks);
- who is known to read or set it: a short, checked list of clients, exporters
  and operators (USED_BY), and how many files of Redis's own test suite set
  it (from tests/unit and tests/integration, read as data), which is what the
  Redis-test oracle will run into.

Both servers are started as the argument census starts them (a password, the
log on). Writes config.json and config.md, and prints the summary. The run
fails only when its method does: a directive Redis does not answer, or a
mapped flag the binary does not have.
"""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent))
import redis_commands as rc  # noqa: E402

_spec = importlib.util.spec_from_file_location('argument_census', Path(__file__).with_name('argument-census.py'))
ac = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(ac)

ENTRY = re.compile(r'create(\w+)Config\(\s*"([^"]+)"\s*,\s*(?:"([^"]+)"|NULL)\s*,\s*([A-Z_| ]+?)\s*,')

# Redis directives Keel's CONFIG GET does not report but a Keel flag covers,
# in part or with another meaning. Checked against the binary's -help.
MAPPED = {
    'lfu-decay-time': ('lfu-decay-period', 'Redis decays an LFU counter by idle minutes; Keel by accesses'),
    'active-expire-effort': ('active-expire-samples', 'Redis scales an effort of 1 to 10; Keel sets keys sampled per cycle'),
    'masterauth': ('primary-password-env', "the primary's password, named by an environment variable"),
    'shutdown-timeout': ('shutdown-timeout', 'Redis waits for replicas to catch up; Keel for cleanup and the log sync'),
    'protected-mode': ('host', 'no mode: Keel listens on 127.0.0.1 unless -host says otherwise'),
}

# Who reads or sets a directive, with the evidence, checked October 12, 2026.
USED_BY = {
    'maxmemory': ['redis_exporter (config_maxmemory, from CONFIG GET *: exporter/exporter.go extractConfigMetrics)'],
    'maxclients': ['redis_exporter (config_maxclients)'],
    'io-threads': ['redis_exporter (config_io_threads)'],
    'repl-backlog-size': ['redis_exporter (config_repl_backlog_size)', 'Bitnami redis chart (an extraFlags example)'],
    'client-output-buffer-limit': ['redis_exporter (config_client_output_buffer_limit_bytes and _overcome_seconds)'],
    'databases': ['redis_exporter (per-database metrics)', 'redis-cli (CONFIG GET databases, src/redis-cli.c:9734)'],
    'maxmemory-policy': ['Sidekiq (warns unless noeviction, read from INFO maxmemory_policy: lib/sidekiq/cli.rb)',
                         'BullMQ (same warning, from INFO: src/classes/redis-connection.ts)',
                         'Bitnami redis chart (an extraFlags example)'],
    'appendonly': ['Bitnami redis chart (commonConfiguration: appendonly yes)'],
    'save': ['Bitnami redis chart (commonConfiguration: save "")', 'spotahome redis-operator (save 900 1, save 300 10)',
             'the official Docker image docs (redis-server --save 60 1)'],
    'loglevel': ['the official Docker image docs (--loglevel warning)'],
    'requirepass': ['spotahome redis-operator (written with the password)', 'redis_exporter (redacted)'],
    'masterauth': ['spotahome redis-operator (written with the password)', 'redis_exporter (redacted)'],
    'replicaof': ['spotahome redis-operator (slaveof 127.0.0.1 <port> in every pod)'],
    'port': ['spotahome redis-operator'],
    'tcp-keepalive': ['spotahome redis-operator (tcp-keepalive 60)'],
}

TEST_SET = re.compile(r'(?:config set|config_set|config_get_set|CONFIG SET|--config|CONFIG GET|config get)\s+'
                      r'([a-z][a-z0-9-]*)|overrides\s*\{([^}]*)\}|start_server\s*\{[^{}]*\{([^}]*)\}')


def parse_table(config_c):
    """Every directive of static_configs: name, alias, kind, flags, and the
    build condition (#ifdef) it sits under, if any."""
    text = Path(config_c).read_text(errors='replace')
    table = text[text.index('standardConfig static_configs[]'):]
    rows, conditions = [], []
    for line in table.splitlines():
        stripped = line.strip()
        if stripped.startswith(('#ifdef', '#ifndef', '#if ')):
            conditions.append(stripped)
        elif stripped.startswith('#endif'):
            conditions.pop()
        elif stripped.startswith('#else'):
            conditions[-1] = 'not ' + conditions[-1]
        m = ENTRY.search(line)
        if m:
            kind, name, alias, flags = m.groups()
            rows.append({'name': name, 'alias': alias or '', 'kind': kind.lower(),
                         'flags': [f.strip() for f in flags.split('|') if f.strip() != 'MODIFIABLE_CONFIG'],
                         'condition': ' and '.join(conditions)})
        if stripped == '};':
            break
    return rows


def parse_conf(redis_conf, names):
    """The directives redis.conf documents, active or as a commented example,
    each with the first line that shows it."""
    documented = {}
    for line in Path(redis_conf).read_text(errors='replace').splitlines():
        m = re.match(r'^(#\s?)?([a-z][a-z0-9-]*)(\s.*)?$', line)
        if m and m[2] in names and m[2] not in documented:
            documented[m[2]] = ('example' if m[1] else 'active', line.strip()[:80])
    return documented


def test_uses(tests, names):
    """How many files of Redis's test suite set each directive."""
    files = {}
    for directory in ('unit', 'integration'):
        for dirpath, _, filenames in os.walk(Path(tests) / directory):
            for filename in filenames:
                if not filename.endswith('.tcl'):
                    continue
                text = (Path(dirpath) / filename).read_text(errors='replace')
                for m in TEST_SET.finditer(text):
                    found = [m[1]] if m[1] else re.findall(r'(?:^|\s)([a-z][a-z0-9-]+)\s', ' ' + (m[2] or m[3]) + ' ')
                    for name in found:
                        if name in names:
                            files.setdefault(name, set()).add(filename)
    return {name: len(found) for name, found in files.items()}


def keel_flags(binary):
    out = subprocess.run([str(binary), '-help'], capture_output=True, text=True)
    return set(re.findall(r'^\s+-([a-z0-9-]+)', out.stdout + out.stderr, re.M))


def config_get(port, password, name):
    """A server's CONFIG GET reply for one name: the value, or None."""
    c = ac.Connection(port, password)
    try:
        reply = c.call(['CONFIG', 'GET', name])
    finally:
        c.close()
    if reply[0] == '-':
        return None, reply[1]
    pairs = reply[1] or []
    if reply[0] == '%':
        return (pairs[0][1][1] if pairs else None), ''
    return (pairs[1][1] if len(pairs) >= 2 else None), ''


def classify(row, keel_value, flags):
    if keel_value is not None:
        return 'has', ''
    if row['name'] in MAPPED:
        flag, note = MAPPED[row['name']]
        return 'mapped', f'-{flag}: {note}'
    return 'lacks', ''


# A directive's family: the first pattern that matches its name.
FAMILIES = [
    ('cluster', r'^cluster-'),
    ('tls', r'^tls-'),
    ('replication', r'^(repl-|replica-|replicaof|slaveof|slave-|masterauth|masteruser|min-replicas-|'
                    r'min-slaves-|propagation-|shutdown-timeout)'),
    ('persistence', r'^(append|aof-|auto-aof-|rdb|save$|dir$|dbfilename|no-appendfsync|stop-writes-on-bgsave|'
                    r'sanitize-dump|key-load-delay|loading-process|backup-|rdb-)'),
    ('memory and eviction', r'^(maxmemory|lfu-|active-defrag|activedefrag|lazyfree-|oom-score|jemalloc|'
                            r'active-expire|lazy|hz$|dynamic-hz)'),
    ('encodings', r'^(hash-|list-|set-max-|zset-|stream-|hll-|array-|vset-)'),
    ('clients and network', r'^(bind|port$|tcp-|timeout$|maxclients|client-|proto-|io-threads|unixsocket|'
                            r'socket-|max-new-|tracking-|busy-reply|enable-protected|protected-mode|'
                            r'ignore-warnings|shutdown-on-sig|close-on-)'),
    ('logging and process', r'^(loglevel|logfile|syslog-|daemonize|pidfile|supervised|set-proc-title|'
                            r'proc-title|crash-|always-show-logo|locale-collate|hide-user-data|watchdog|'
                            r'server-cpulist|bio-cpulist|aof-rewrite-cpulist|bgsave-cpulist|extended-redis|'
                            r'log-)'),
    ('security', r'^(requirepass|acl|aclfile|enable-debug|enable-module|rename-command|user$)'),
    ('scripting', r'^(lua-|script-)'),
    ('monitoring', r'^(slowlog-|latency-|hotkeys|key-memory|notify-keyspace|cluster-slot-stats)'),
]


def family(name):
    for label, pattern in FAMILIES:
        if re.match(pattern, name):
            return label
    return 'other'


def census(rows, keel_port, redis_port, password, flags, documented, uses):
    for row in rows:
        row['documented'] = documented.get(row['name'], ('no', ''))[0]
        row['conf_line'] = documented.get(row['name'], ('', ''))[1]
        row['test_files'] = uses.get(row['name'], 0)
        row['redis_value'], row['redis_error'] = config_get(redis_port, password, row['name'])
        row['keel_value'], _ = config_get(keel_port, password, row['name'])
        row['class'], row['note'] = classify(row, row['keel_value'], flags)
        row['used_by'] = USED_BY.get(row['name'], [])
        row['family'] = family(row['name'])
        if row['class'] == 'mapped' and MAPPED[row['name']][0] not in flags:
            row['control'] = f"Keel has no -{MAPPED[row['name']][0]} flag"
        elif row['redis_value'] is None and not row['condition']:
            row['control'] = 'Redis did not answer CONFIG GET'
        else:
            row['control'] = ''
    return rows


def summarise(rows):
    """Counts over the directives this build of Redis has (one under a build
    condition it was built without is left out)."""
    rows = [r for r in rows if r['redis_value'] is not None or not r['condition']]
    total = {'configs': len(rows), 'has': 0, 'mapped': 0, 'lacks': 0, 'lacks_used': 0, 'lacks_tested': 0,
             'documented': 0, 'same_value': 0}
    families = {}
    for row in rows:
        total[row['class']] += 1
        total['documented'] += row['documented'] != 'no'
        total['same_value'] += row['class'] == 'has' and row['keel_value'] == row['redis_value']
        if row['class'] == 'lacks':
            total['lacks_used'] += bool(row['used_by'])
            total['lacks_tested'] += row['test_files'] > 0
        f = families.setdefault(row['family'], {'configs': 0, 'has': 0, 'mapped': 0, 'lacks': 0})
        f['configs'] += 1
        f[row['class']] += 1
    return {'total': total, 'families': dict(sorted(families.items()))}


def markdown(summary, rows, redis_version):
    t = summary['total']
    out = [f"Of Redis {redis_version}'s **{t['configs']} configuration directives**, Keel reports **{t['has']}** "
           f"under Redis's name through CONFIG GET ({t['same_value']} with the same value at defaults, the rest "
           f"differing by default or by this harness), has a flag for **{t['mapped']}** more under another meaning, "
           f"and lacks **{t['lacks']}**. Of those, {t['lacks_used']} are read or set by a client, exporter or operator "
           f"checked here, and {t['lacks_tested']} are set by Redis's own test suite.", '',
           '| Family | Directives | Has | Mapped | Lacks |', '| --- | ---: | ---: | ---: | ---: |']
    for name, f in summary['families'].items():
        out.append(f"| {name} | {f['configs']} | {f['has']} | {f['mapped']} | {f['lacks']} |")
    out += ['', '### Has', '', '| Directive | Keel | Redis default |', '| --- | --- | --- |']
    for r in rows:
        if r['class'] == 'has':
            out.append(f"| `{r['name']}` | `{cell(r['keel_value'])}` | `{cell(r['redis_value'])}` |")
    out += ['', '### Mapped', '', '| Directive | Keel flag | Note |', '| --- | --- | --- |']
    for r in rows:
        if r['class'] == 'mapped':
            flag, note = r['note'].split(': ', 1)
            out.append(f"| `{r['name']}` | `{flag}` | {note} |")
    out += ['', '### Lacks, and used by a client, exporter or operator', '',
            '| Directive | Used by | Redis default | Test files |', '| --- | --- | --- | ---: |']
    for r in rows:
        if r['class'] == 'lacks' and r['used_by']:
            out.append(f"| `{r['name']}` | {'; '.join(r['used_by'])} | `{cell(r['redis_value'])}` | {r['test_files']} |")
    tested = sorted((r for r in rows if r['class'] == 'lacks' and not r['used_by'] and r['test_files']),
                    key=lambda r: (-r['test_files'], r['name']))
    out += ['', "### Lacks, and set by Redis's own test suite", '', '| Directive | Test files | Redis default |',
            '| --- | ---: | --- |']
    out += [f"| `{r['name']}` | {r['test_files']} | `{cell(r['redis_value'])}` |" for r in tested]
    out += ['', '### Lacks, the rest, by family', '']
    rest = {}
    for r in rows:
        if r['class'] == 'lacks' and not r['used_by'] and not r['test_files'] and (
                r['redis_value'] is not None or not r['condition']):
            rest.setdefault(r['family'], []).append(r['name'])
    out += [f"- **{f}**: {', '.join(f'`{n}`' for n in names)}" for f, names in sorted(rest.items())]
    out.append('')
    return '\n'.join(out) + '\n'


def cell(value):
    return (value if value is not None else '').replace('|', '\\|').replace('`', "'")[:60] or '""'


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--bin', type=Path, required=True, help='the Keel binary')
    ap.add_argument('--redis-server', type=Path, required=True, help='the redis-server binary')
    ap.add_argument('--source', type=Path, required=True, help='the unpacked Redis source tree')
    ap.add_argument('--out', type=Path, required=True)
    args = ap.parse_args(argv)
    args.out.mkdir(parents=True, exist_ok=True)
    rows = parse_table(args.source / 'src' / 'config.c')
    names = {r['name'] for r in rows}
    documented = parse_conf(args.source / 'redis.conf', names)
    uses = test_uses(args.source / 'tests', names)
    flags = keel_flags(args.bin)
    servers = ac.Servers(args.out / 'config-servers')
    try:
        keel_port = servers.start_keel(args.bin)
        redis_port = servers.start_redis(args.redis_server)
        version = ac.redis_version(redis_port, servers.password)
        rows = census(rows, keel_port, redis_port, servers.password, flags, documented, uses)
    finally:
        servers.stop()
    for row in rows:
        for key in ('redis_value', 'keel_value'):
            if row[key] and servers.password in row[key]:
                row[key] = row[key].replace(servers.password, '<password>')
    summary = summarise(rows)
    control = [r for r in rows if r['control']]
    result = {'redis_version': version, 'summary': summary, 'keel_flags': sorted(flags),
              'control_failures': [{'name': r['name'], 'why': r['control']} for r in control], 'configs': rows}
    (args.out / 'config.json').write_text(json.dumps(result, indent=1) + '\n')
    text = markdown(summary, rows, version)
    if control:
        text += '\n**FAILED**\n\n' + ''.join(f"- `{r['name']}`: {r['control']}\n" for r in control)
    (args.out / 'config.md').write_text(text)
    print(text)
    return 1 if control else 0


if __name__ == '__main__':
    sys.exit(main())
