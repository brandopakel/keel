#!/usr/bin/env python3
"""Compare the shape of every reply Keel gives with Redis's, in RESP2 and
RESP3, and with Redis's own reply schema.

The argument census (scripts/argument-census.py) asks whether Keel takes each
documented argument form. This census asks whether what Keel answers has
Redis's shape: the protocol types, nested as Redis nests them, in both
protocols. For every command and subcommand Keel has, every documented
argument form is sent, and then three more cases of its base form: with keys
that do not exist (the null and empty replies), with its first key holding a
value of another type (the WRONGTYPE error), and with one argument too many
(the syntax or count error). Each case goes to Keel and to Redis on a fresh
RESP2 connection and again on a RESP3 one.

A reply's shape is its protocol types with the values removed: +, :, $, _,
,(double), #, an array of its elements' shapes, a map of its pairs' shapes.
Keel's shape is compared with Redis's for each case and protocol. Replies
whose contents are the server's own (HELLO, INFO, CLIENT, MEMORY, a random
member, a SCAN page) are compared by the kinds of thing they hold rather than
element by element.

Each RESP3 reply is also checked against the command's reply_schema, the
JSON Schema Redis keeps for it in src/commands/*.json and validates its own
replies with (utils/req-res-log-validator.py in the Redis source). Redis's
reply is checked as the control; Keel's as the finding. The checker here
covers the keywords those schemas use and no more.

For each command the Redis function that answers it is located in the Redis
source, with the lines in it (and in the helpers it calls in the same file)
that build the reply, so that a mismatch can be read against the code that
produces Redis's reply.

Writes replies.json (every case with both shapes, the schema results and the
source locations) and replies.md (the counts by area and every mismatch), and
prints the summary. The run fails only when its method does: a case Redis
gives no reply to, or Keel gives no reply to.
"""
import argparse
import importlib.util
import json
from pathlib import Path
import re
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent))
import redis_commands as rc  # noqa: E402

_spec = importlib.util.spec_from_file_location('argument_census', Path(__file__).with_name('argument-census.py'))
ac = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(ac)

MISSING = ['nokey', 'nokey2', 'nokey3']
BOGUS = 'BOGUS'
REPLY_SITE = re.compile(r'\b(addReply\w*|setDeferred\w+)\s*\(')
# A function definition at the start of a line: its return type, its name and
# an opening brace on the line.
FUNCTION = re.compile(r'^(?:static\s+)?(?:\w[\w\s\*]*?)\b(\w+)\s*\([^;]*\)\s*\{')
CALL = re.compile(r'\b(\w+)\s*\(')


class Protocol:
    """A fresh connection that speaks one protocol."""

    def __init__(self, port, password, version):
        self.c = ac.Connection(port, password)
        if version == 3:
            reply = self.c.call(['HELLO', '3'])
            if reply[0] != '%':
                raise RuntimeError(f'HELLO 3 answered {rc.text(reply)}')

    def call(self, argv):
        return self.c.call(argv)

    def close(self):
        self.c.close()


def send(side, prelude, argv, version):
    try:
        p = Protocol(side.port, side.password, version)
    except (OSError, RuntimeError, ConnectionError, ValueError) as e:
        return None, f'{type(e).__name__}: {e}'
    try:
        for command in prelude:
            p.call(command)
        return p.call(argv), ''
    except (OSError, ConnectionError, ValueError) as e:
        return None, f'{type(e).__name__}: {e}'
    finally:
        p.close()


def shape(reply):
    """A reply's types with its values removed."""
    kind, value = reply
    if kind in '*~>':
        return kind + ('-1' if value is None else '[' + ' '.join(shape(v) for v in value) + ']')
    if kind == '%':
        return '%{' + ' '.join(f'{shape(k)}:{shape(v)}' for k, v in value) + '}'
    if kind == '|':
        return '|{' + ' '.join(f'{shape(k)}:{shape(v)}' for k, v in value[0]) + '}' + shape(value[1])
    if kind in '$=!' and value is None:
        return kind + '-1'
    if kind == '=':
        return '=' + value[:3]
    return kind


def compatible(keel, redis):
    """Whether two replies hold the same kinds of thing, lengths aside: the
    same outer kind, and every element of Keel's aggregate has an element of
    Redis's with a compatible shape. An empty aggregate on either side is
    compatible with any of its kind (Keel lists no modules in HELLO)."""
    if keel[0] != redis[0]:
        return False
    kind, kv, rv = keel[0], keel[1], redis[1]
    if kind in '*~>':
        if kv is None or rv is None:
            return kv is rv
        return all(any(compatible(k, r) for r in rv) for k in kv)
    if kind == '%':
        return all(any(compatible(kk, rk) and compatible(kvv, rvv) for rk, rvv in rv) for kk, kvv in kv)
    if kind == '|':
        return compatible(keel[1][1], redis[1][1])
    return shape(keel) == shape(redis)


def to_json(reply):
    """A RESP3 reply as the JSON value Redis's validator makes of it."""
    kind, value = reply
    if kind in '*~>':
        return [to_json(v) for v in value] if value is not None else None
    if kind == '%':
        return {k if isinstance(k, str) else json.dumps(k): v for k, v in ((to_json(k), to_json(v)) for k, v in value)}
    if kind == '|':
        return to_json(value[1])
    if kind == ':':
        return int(value)
    if kind == ',':
        return float(value)
    if kind == '=':
        return value[4:]
    if kind in '$+(':
        return value
    return value  # None, bool


def check(value, schema, path=''):
    """The ways a value fails a reply schema; empty when it conforms. Covers
    what Redis's schemas use: type, const, enum, items (one schema or a
    list), additionalItems, minItems, maxItems, uniqueItems, minimum,
    maximum, pattern, properties, required, additionalProperties,
    patternProperties, anyOf, oneOf, allOf and not."""
    errors = []
    if 'type' in schema:
        types = schema['type'] if isinstance(schema['type'], list) else [schema['type']]
        if not any(is_type(value, t) for t in types):
            return [f"{path or '/'}: {describe(value)} is not {' or '.join(types)}"]
    if 'const' in schema and value != schema['const']:
        errors.append(f"{path or '/'}: {describe(value)} is not the constant {schema['const']!r}")
    if 'enum' in schema and value not in schema['enum']:
        errors.append(f"{path or '/'}: {describe(value)} is not one of {schema['enum']!r}")
    if isinstance(value, bool):
        pass
    elif isinstance(value, (int, float)):
        if 'minimum' in schema and value < schema['minimum']:
            errors.append(f"{path or '/'}: {value} is below {schema['minimum']}")
        if 'maximum' in schema and value > schema['maximum']:
            errors.append(f"{path or '/'}: {value} is above {schema['maximum']}")
    elif isinstance(value, str):
        if 'pattern' in schema and not re.search(schema['pattern'], value):
            errors.append(f"{path or '/'}: {value!r} does not match {schema['pattern']!r}")
    elif isinstance(value, list):
        if 'minItems' in schema and len(value) < schema['minItems']:
            errors.append(f"{path or '/'}: {len(value)} items, fewer than {schema['minItems']}")
        if 'maxItems' in schema and len(value) > schema['maxItems']:
            errors.append(f"{path or '/'}: {len(value)} items, more than {schema['maxItems']}")
        if schema.get('uniqueItems') and len({json.dumps(v, sort_keys=True) for v in value}) != len(value):
            errors.append(f"{path or '/'}: items are not unique")
        items = schema.get('items')
        if isinstance(items, dict):
            for i, v in enumerate(value):
                errors += check(v, items, f'{path}/{i}')
        elif isinstance(items, list):
            for i, v in enumerate(value):
                if i < len(items):
                    errors += check(v, items[i], f'{path}/{i}')
                elif schema.get('additionalItems') is False:
                    errors.append(f'{path}/{i}: more items than the schema lists')
                elif isinstance(schema.get('additionalItems'), dict):
                    errors += check(v, schema['additionalItems'], f'{path}/{i}')
    elif isinstance(value, dict):
        properties = schema.get('properties', {})
        for k in schema.get('required', []):
            if k not in value:
                errors.append(f"{path or '/'}: {k!r} is missing")
        for k, v in value.items():
            if k in properties:
                errors += check(v, properties[k], f'{path}/{k}')
                continue
            matched = False
            for pattern, sub in schema.get('patternProperties', {}).items():
                if re.search(pattern, k):
                    matched = True
                    errors += check(v, sub, f'{path}/{k}')
            if matched:
                continue
            additional = schema.get('additionalProperties', True)
            if additional is False:
                errors.append(f'{path}/{k}: not a listed field')
            elif isinstance(additional, dict):
                errors += check(v, additional, f'{path}/{k}')
    for sub in schema.get('allOf', []):
        errors += check(value, sub, path)
    if 'anyOf' in schema and not any(not check(value, sub, path) for sub in schema['anyOf']):
        errors.append(f"{path or '/'}: {describe(value)} matches none of the alternatives")
    if 'oneOf' in schema:
        matches = sum(not check(value, sub, path) for sub in schema['oneOf'])
        if matches != 1:
            errors.append(f"{path or '/'}: {describe(value)} matches {matches} alternatives, not one")
    if 'not' in schema and not check(value, schema['not'], path):
        errors.append(f"{path or '/'}: matches what the schema excludes")
    return errors


def is_type(value, t):
    if t == 'null':
        return value is None
    if t == 'boolean':
        return isinstance(value, bool)
    if t == 'integer':
        return isinstance(value, int) and not isinstance(value, bool)
    if t == 'number':
        return isinstance(value, (int, float)) and not isinstance(value, bool)
    if t == 'string':
        return isinstance(value, str)
    if t == 'array':
        return isinstance(value, list)
    if t == 'object':
        return isinstance(value, dict)
    return False


def describe(value):
    if value is None:
        return 'null'
    if isinstance(value, bool):
        return 'a boolean'
    if isinstance(value, (int, float)):
        return 'a number' if isinstance(value, float) else 'an integer'
    if isinstance(value, str):
        return 'a string'
    if isinstance(value, list):
        return f'an array of {len(value)}'
    return f'a map of {len(value)}'


class Source:
    """Where in the Redis source each command function is, and the lines in
    it that build a reply."""

    def __init__(self, src):
        self.src = Path(src)
        self.files = {}
        self.defined = {}  # file -> name -> (start, end), 1-based lines
        self.functions = {}  # name -> (file, start, end), the first definition
        for path in sorted(self.src.glob('*.c')):
            lines = path.read_text(errors='replace').splitlines()
            self.files[path.name] = lines
            self.defined[path.name] = {}
            for i, line in enumerate(lines):
                m = FUNCTION.match(line)
                if m and not line.startswith(('if', 'for', 'while', 'switch', 'else', 'return')):
                    end = i
                    while end < len(lines) - 1 and lines[end] != '}':
                        end += 1
                    self.defined[path.name].setdefault(m[1], (i + 1, end + 1))
                    self.functions.setdefault(m[1], (path.name, i + 1, end + 1))

    def sites(self, file, name, depth=2, seen=None):
        """Lines that build a reply inside a function, and inside the
        functions it calls that are defined in the same file, two levels."""
        seen = seen if seen is not None else set()
        if name in seen or name not in self.defined.get(file, {}):
            return []
        seen.add(name)
        start, end = self.defined[file][name]
        lines = self.files[file]
        found, called = [], []
        for n in range(start, end + 1):
            line = lines[n - 1]
            if REPLY_SITE.search(line):
                found.append(f'{file}:{n}: {line.strip()[:100]}')
            called += [c for c in CALL.findall(line) if c in self.defined[file] and c != name]
        if depth:
            for call in dict.fromkeys(called):
                found += self.sites(file, call, depth - 1, seen)
        return found

    def where(self, spec):
        function = spec.get('function', '')
        if function not in self.functions:
            return {'function': function, 'at': '', 'sites': []}
        file, start, _ = self.functions[function]
        return {'function': function, 'at': f'{file}:{start}', 'sites': self.sites(file, function)[:16]}


def cases_of(spec, password):
    """Every case of a command: its documented forms, then the base form with
    missing keys, with a key of another type, and with an argument too many."""
    forms = rc.forms_of(spec, password)
    cases = [dict(f, case='form') for f in forms]
    base = forms[0]['argv']
    fixture = {name: type_ for type_, (names, _) in rc.FIXTURE.items() if type_ != 'dest' for name in names}
    keys = [i for i, a in enumerate(base) if a in fixture]
    if keys:
        argv = list(base)
        for n, i in enumerate(keys):
            argv[i] = MISSING[min(n, len(MISSING) - 1)]
        cases.append({'label': 'missing key', 'since': forms[0]['since'], 'argv': argv, 'case': 'missing key'})
        argv = list(base)
        other = 'list' if fixture[base[keys[0]]] in ('string', 'number') else 'str'
        argv[keys[0]] = other
        cases.append({'label': 'wrong type', 'since': forms[0]['since'], 'argv': argv, 'case': 'wrong type'})
    cases.append({'label': 'extra argument', 'since': forms[0]['since'], 'argv': base + [BOGUS],
                  'case': 'extra argument'})
    return cases


def compare(name, keel, redis):
    """same, differs, refused (Keel answers an error where Redis answers a
    value: an argument gap, counted by the argument census) or no reply."""
    if keel is None or redis is None:
        return 'no reply', ''
    if keel[0] in '-!' and redis[0] not in '-!':
        return 'refused', keel[1]
    if ac.loose(name):
        return ('same', 'by the kinds of thing it holds') if compatible(keel, redis) else ('differs', '')
    return ('same', '') if shape(keel) == shape(redis) else ('differs', '')


def census(specs, present, keel, redis, password, source):
    rows, commands = [], {}
    for name in sorted(n for n in specs if not specs[n]['is_container']):
        spec = specs[name]
        if name in rc.UNSAFE or name not in present:
            continue
        commands[name] = {'area': spec.get('group', ''), **source.where(spec),
                          'schema': 'reply_schema' in spec}
        schema = spec.get('reply_schema')
        for case in cases_of(spec, password):
            prelude = rc.PRELUDE.get(name, [])
            for version in (2, 3):
                keel.reset()
                redis.reset()
                keel_reply, keel_why = send(keel, prelude, case['argv'], version)
                redis_reply, redis_why = send(redis, prelude, case['argv'], version)
                result, detail = compare(name, keel_reply, redis_reply)
                row = {'command': name, 'area': spec.get('group', ''), 'case': case['case'], 'label': case['label'],
                       'since': case['since'], 'argv': [a.replace(password, '<password>') for a in case['argv']],
                       'protocol': version, 'result': result, 'detail': detail or keel_why or redis_why,
                       'keel': rc.text(keel_reply, 300) if keel_reply else '',
                       'redis': rc.text(redis_reply, 300) if redis_reply else '',
                       'keel_shape': shape(keel_reply) if keel_reply else '',
                       'redis_shape': shape(redis_reply) if redis_reply else '',
                       'keel_schema': '', 'redis_schema': ''}
                if version == 3 and schema is not None:
                    for side, reply in (('keel', keel_reply), ('redis', redis_reply)):
                        if reply is None:
                            continue
                        if reply[0] in '-!':
                            row[f'{side}_schema'] = 'error'
                        else:
                            errors = check(to_json(reply), schema)
                            row[f'{side}_schema'] = 'ok' if not errors else 'violation: ' + '; '.join(errors)[:300]
                elif version == 3:
                    row['keel_schema'] = row['redis_schema'] = 'no schema'
                rows.append(row)
    return rows, commands


def summarise(rows):
    areas = {}
    for row in rows:
        a = areas.setdefault(row['area'], {'commands': set(), 'cases': 0, 'same': 0, 'differs': 0, 'refused': 0,
                                            'no_reply': 0, 'resp3_cases': 0, 'resp3_same': 0,
                                            'keel_schema_violations': 0, 'redis_schema_violations': 0})
        a['commands'].add(row['command'])
        a['cases'] += 1
        a[row['result'].replace(' ', '_')] += 1
        if row['protocol'] == 3:
            a['resp3_cases'] += 1
            a['resp3_same'] += row['result'] == 'same'
            a['keel_schema_violations'] += row['keel_schema'].startswith('violation')
            a['redis_schema_violations'] += row['redis_schema'].startswith('violation')
    for a in areas.values():
        a['commands'] = len(a['commands'])
    keys = ['commands', 'cases', 'same', 'differs', 'refused', 'no_reply', 'resp3_cases', 'resp3_same',
            'keel_schema_violations', 'redis_schema_violations']
    total = {k: sum(a[k] for a in areas.values()) for k in keys}
    return {'areas': dict(sorted(areas.items())), 'total': total}


def markdown(summary, rows, commands, redis_version):
    t = summary['total']
    out = [f"Keel's replies have **Redis {redis_version}'s shape in {t['same']} of {t['cases']} cases** "
           f"({t['cases'] // 2} cases of {t['commands']} commands, each in RESP2 and RESP3): {t['differs']} differ, "
           f"and {t['refused']} are forms Keel refuses (the argument census counts those). Against Redis's own "
           f"reply schemas, Keel's RESP3 replies have {t['keel_schema_violations']} violations and Redis's "
           f"{t['redis_schema_violations']}.", '',
           '| Area | Commands | Cases | Same shape | Differ | Refused | Keel schema violations | Commands that differ |',
           '| --- | ---: | ---: | ---: | ---: | ---: | ---: | --- |']
    for area, a in summary['areas'].items():
        names = sorted({r['command'] for r in rows if r['area'] == area and r['result'] == 'differs'})
        out.append(f"| {area} | {a['commands']} | {a['cases']} | {a['same']} | {a['differs']} | {a['refused']} | "
                   f"{a['keel_schema_violations']} | {', '.join(f'`{n}`' for n in names) or '-'} |")
    out += ['', 'A case is one documented argument form, or the base form with missing keys, with a key of another '
                'type, or with an argument too many, in one protocol. A shape is the reply\'s protocol types with '
                'the values removed.', '']
    gaps = [r for r in rows if r['result'] == 'differs']
    if gaps:
        out += ['| Command | Case | RESP | Sent | Keel | Redis | Where Redis answers |',
                '| --- | --- | ---: | --- | --- | --- | --- |']
        for r in gaps:
            where = commands.get(r['command'], {})
            sites = where.get('sites') or []
            place = where.get('at', '')
            if sites:
                place += ' (' + ', '.join(s.split(': ')[0] for s in sites[:4]) + ')'
            out.append(f"| `{r['command']}` | {r['label']} | {r['protocol']} | `{' '.join(r['argv'])}` | "
                       f"`{ac.cell(r['keel'])}` | `{ac.cell(r['redis'])}` | {place} |")
        out.append('')
    violations = [r for r in rows if r['keel_schema'].startswith('violation')]
    if violations:
        out += ['Keel replies that violate the reply schema:', '']
        out += [f"- `{' '.join(r['argv'])}`: `{ac.cell(r['keel'])}`: {r['keel_schema'][11:]}" for r in violations]
        out.append('')
    control = [r for r in rows if r['redis_schema'].startswith('violation')]
    if control:
        out += ["Redis replies that violate Redis's own reply schema (the control; a schema flaw, not a Keel one):", '']
        out += [f"- `{' '.join(r['argv'])}`: `{ac.cell(r['redis'])}`: {r['redis_schema'][11:]}" for r in control]
        out.append('')
    return '\n'.join(out) + '\n'


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--bin', type=Path, required=True, help='the Keel binary')
    ap.add_argument('--redis-server', type=Path, required=True, help='the redis-server binary')
    ap.add_argument('--source', type=Path, required=True, help="the Redis source's src directory")
    ap.add_argument('--out', type=Path, required=True)
    args = ap.parse_args(argv)
    args.out.mkdir(parents=True, exist_ok=True)
    specs = rc.load_specs(args.source / 'commands')
    source = Source(args.source)
    servers = ac.Servers(args.out / 'servers')
    try:
        keel_port = servers.start_keel(args.bin)
        redis_port = servers.start_redis(args.redis_server)
        version = ac.redis_version(redis_port, servers.password)
        present = ac.probe_present(keel_port, servers.password, specs)
        redis_present = ac.probe_present(redis_port, servers.password, specs)
        fixture = ac.Fixture(present & redis_present)
        keel = ac.Side('keel', keel_port, servers.password, fixture)
        redis = ac.Side('redis', redis_port, servers.password, fixture)
        rows, commands = census(specs, present, keel, redis, servers.password, source)
    finally:
        servers.stop()
    summary = summarise(rows)
    no_reply = [r for r in rows if r['result'] == 'no reply']
    result = {'redis_version': version, 'source': str(args.source), 'summary': summary,
              'no_reply': [{k: r[k] for k in ('command', 'label', 'protocol', 'argv', 'detail')} for r in no_reply],
              'commands': commands, 'cases': rows}
    (args.out / 'replies.json').write_text(json.dumps(result, indent=1) + '\n')
    text = markdown(summary, rows, commands, version)
    if no_reply:
        text += '\n**FAILED**\n\n' + ''.join(
            f"- no reply to `{' '.join(r['argv'])}` (RESP{r['protocol']}): {r['detail']}\n" for r in no_reply)
    (args.out / 'replies.md').write_text(text)
    print(text)
    return 1 if no_reply else 0


if __name__ == '__main__':
    sys.exit(main())
