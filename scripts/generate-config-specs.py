#!/usr/bin/env python3
"""Generate Keel's table of Redis's configuration directives from Redis's source.

Redis declares every configuration directive in one table, static_configs in
src/config.c: its name and alias, its flags (immutable, protected, sensitive,
hidden, multi-argument), its type, and for a number its bounds, its default
and whether it is read as a memory size, a percentage or an octal number; for
an enum, the names it takes. CONFIG SET validates a value against exactly
that (configSetCommand and the type's set function), so Keel's CONFIG SET
answers from this table to give Redis's errors, word for word, for every
directive. This script writes it: internal/core/config_specs.go.

The defaults are written as CONFIG GET shows them: a number in its format (a
memory size in bytes, a percentage as N%, an octal number in octal), an enum
as its name or names, a bool as yes or no, a string as itself. Each default
and bound is evaluated from the C expression the table has, through the
#define macros of src/*.h and src/config.c; one this script cannot evaluate
stops it, rather than be written wrong. A special directive (save, dir,
bind, ...) has a set and get function of its own and no default here; Keel's
handling of each is in config.go.

Only facts are copied - names, numbers and flags - and none of Redis's
comments or documentation.

    python3 scripts/generate-config-specs.py --redis-src /path/to/redis-8.10.1
    python3 scripts/generate-config-specs.py --redis-src ... --check   # CI: the file is current

The Redis source is the release tarball CI already downloads, pinned by
checksum in .github/workflows/general-validation.yml; nothing of it is
checked in.
"""
import argparse
import ast
from pathlib import Path
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parent.parent
OUT = ROOT / 'internal' / 'core' / 'config_specs.go'
REDIS_VERSION = '8.10.1'

# The C limits and system macros the table uses, for a 64-bit build (LP64:
# long is 64 bits), which is what Redis is built as on the platforms Keel
# runs on.
C_CONSTANTS = {
    'INT_MAX': 2**31 - 1, 'INT_MIN': -2**31, 'UINT_MAX': 2**32 - 1,
    'LONG_MAX': 2**63 - 1, 'LONG_MIN': -2**63, 'ULONG_MAX': 2**64 - 1,
    'LLONG_MAX': 2**63 - 1, 'LLONG_MIN': -2**63, 'ULLONG_MAX': 2**64 - 1,
    'SIZE_MAX': 2**64 - 1, 'SSIZE_MAX': 2**63 - 1,
    # <syslog.h>: facilities are their code shifted left by 3.
    'LOG_USER': 1 << 3, **{f'LOG_LOCAL{i}': (16 + i) << 3 for i in range(8)},
}

# The C type each numeric kind stores its value in: its width in bits and
# whether it is signed. setNumericType casts to it, so a value is read back
# as this type holds it.
NUMERIC_TYPES = {kind: (f'num{kind}', bits, signed) for kind, bits, signed in [
    ('Int', 32, True), ('UInt', 32, False), ('Long', 64, True), ('ULong', 64, False),
    ('LongLong', 64, True), ('ULongLong', 64, False), ('SizeT', 64, False), ('SSizeT', 64, True),
    ('TimeT', 64, True), ('OffT', 64, True)]}

FLAGS = {
    'IMMUTABLE_CONFIG': 'configImmutable', 'SENSITIVE_CONFIG': 'configSensitive',
    'DEBUG_CONFIG': 'configDebug', 'MULTI_ARG_CONFIG': 'configMultiArg',
    'HIDDEN_CONFIG': 'configHidden', 'PROTECTED_CONFIG': 'configProtected',
    'DENY_LOADING_CONFIG': 'configDenyLoading', 'ALIAS_CONFIG': 'configAlias',
    'MODULE_CONFIG': 'configModule', 'VOLATILE_CONFIG': 'configVolatile',
}
NUMERIC_FLAGS = {'INTEGER_CONFIG': None, 'MEMORY_CONFIG': 'numMemory', 'PERCENT_CONFIG': 'numPercent',
                 'OCTAL_CONFIG': 'numOctal'}


def macros(redis_src):
    """Every object-like #define of src/*.h and src/config.c, name to its
    text. A name defined twice with different texts is kept as None, and
    refused if the table needs it."""
    out = {}
    paths = sorted((Path(redis_src) / 'src').glob('*.h')) + [Path(redis_src) / 'src' / 'config.c']
    for path in paths:
        for m in re.finditer(r'^[ \t]*#[ \t]*define[ \t]+([A-Za-z_]\w*)(?!\()[ \t]+(.*?)\s*$',
                             path.read_text(errors='replace'), re.M):
            name, text = m.group(1), re.sub(r'/\*.*?(?:\*/|$)|//.*$', '', m.group(2)).strip()
            if not text or text.endswith('\\'):
                continue
            if name in out and out[name] != text:
                out[name] = None
            else:
                out[name] = text
    # The members of C enums, which number from 0, or from the value the
    # last one was given, by one. A member given a value this reader cannot
    # read whole is left out, and so is every member after it.
    for path in paths:
        text = re.sub(r'/\*.*?\*/|//[^\n]*', '', path.read_text(errors='replace'), flags=re.S)
        for m in re.finditer(r'\benum\b\s*\w*\s*\{([^{}]*)\}', text):
            nxt = 0
            for member in (s.strip() for s in m.group(1).split(',')):
                em = re.fullmatch(r'([A-Za-z_]\w*)(?:\s*=\s*(-?\d+|0x[0-9a-fA-F]+|\([^()]*\)|[\w<> ()|]+))?', member)
                if not em:
                    break
                if em.group(2):
                    try:
                        nxt = Evaluator(out)(em.group(2))
                    except ValueError:
                        break
                name = em.group(1)
                if name in out and out[name] != str(nxt):
                    out[name] = None
                else:
                    out[name] = str(nxt)
                nxt += 1
    return out


class Evaluator:
    """Evaluates the integer C expressions the table's bounds and defaults
    are written in: literals with C's suffixes, macros, casts, unary minus
    and the arithmetic and bitwise operators."""

    def __init__(self, defines):
        self.defines = defines

    def __call__(self, text, seen=()):
        text = text.strip()
        text = re.sub(r'\((?:unsigned\s+)?(?:long\s+long|long|int|size_t|ssize_t|off_t|time_t)\)', '', text)
        text = re.sub(r'\b(0[0-7]+|0x[0-9a-fA-F]+|\d+)(?:ULL|ull|LL|ll|UL|ul|L|l|U|u)\b', r'\1', text)
        text = re.sub(r'\b0([0-7]+)\b', r'0o\1', text)
        try:
            tree = ast.parse(text, mode='eval')
        except SyntaxError as e:
            raise ValueError(f'cannot read C expression {text!r}') from e
        return self.node(tree.body, seen)

    def node(self, n, seen):
        if isinstance(n, ast.Constant) and isinstance(n.value, int):
            return n.value
        if isinstance(n, ast.Name):
            if n.id in C_CONSTANTS:
                return C_CONSTANTS[n.id]
            if n.id in seen:
                raise ValueError(f'macro {n.id} refers to itself')
            text = self.defines.get(n.id)
            if text is None:
                raise ValueError(f'no single definition of {n.id}')
            return self(text, seen + (n.id,))
        if isinstance(n, ast.UnaryOp) and isinstance(n.op, (ast.USub, ast.UAdd, ast.Invert)):
            v = self.node(n.operand, seen)
            return -v if isinstance(n.op, ast.USub) else (v if isinstance(n.op, ast.UAdd) else ~v)
        if isinstance(n, ast.BinOp):
            a, b = self.node(n.left, seen), self.node(n.right, seen)
            ops = {ast.Add: lambda: a + b, ast.Sub: lambda: a - b, ast.Mult: lambda: a * b,
                   ast.LShift: lambda: a << b, ast.RShift: lambda: a >> b, ast.BitOr: lambda: a | b,
                   ast.BitAnd: lambda: a & b, ast.Div: lambda: int(a / b), ast.FloorDiv: lambda: a // b}
            for op, f in ops.items():
                if isinstance(n.op, op):
                    return f()
        raise ValueError(f'cannot evaluate {ast.dump(n)}')


def c_cast(value, bits, signed):
    """value as a C integer of that width and signedness holds it."""
    value &= (1 << bits) - 1
    if signed and value >= 1 << (bits - 1):
        value -= 1 << bits
    return value


def as_long_long(value):
    return c_cast(value, 64, True)


def split_args(text):
    """The top-level, comma-separated arguments of a call's argument text."""
    args, depth, cur, quote = [], 0, '', False
    for i, ch in enumerate(text):
        if quote:
            cur += ch
            if ch == '"' and text[i - 1] != '\\':
                quote = False
            continue
        if ch == '"':
            quote = True
        elif ch in '([{':
            depth += 1
        elif ch in ')]}':
            depth -= 1
        elif ch == ',' and depth == 0:
            args.append(cur.strip())
            cur = ''
            continue
        cur += ch
    if cur.strip():
        args.append(cur.strip())
    return args


def c_string(arg, defines=None):
    """A C string literal's value, or None for NULL; a macro naming one is
    read through defines."""
    if arg == 'NULL':
        return None
    if defines is not None and re.fullmatch(r'[A-Za-z_]\w*', arg) and defines.get(arg):
        return c_string(defines[arg], defines)
    m = re.fullmatch(r'"((?:[^"\\]|\\.)*)"', arg)
    if not m:
        raise ValueError(f'not a string literal: {arg!r}')
    return m.group(1).encode().decode('unicode_escape')


def enums(config_c, evaluate):
    """Every configEnum table of config.c: its name to [(name, value)]."""
    out = {}
    for m in re.finditer(r'configEnum (\w+)\[\] = \{(.*?)\n\};', config_c, re.S):
        values = []
        for em in re.finditer(r'\{\s*"([^"]+)"\s*,\s*([^}]+?)\s*\}', m.group(2)):
            values.append((em.group(1), evaluate(em.group(2))))
        out[m.group(1)] = values
    return out


def enum_name(values, value, bitflags):
    """configEnumGetName: the name, or for bit flags the names, of value."""
    names, unmatched = [], value
    for name, v in values:
        if v == value:
            return name
        if bitflags and v and v == (unmatched & v):
            names.append(name)
            unmatched &= ~v
    if not names or unmatched:
        return 'unknown'
    return ' '.join(names)


def numeric_get(value, flags):
    """numericConfigGet: how CONFIG GET writes a number."""
    if 'numPercent' in flags and value < 0:
        return f'{-value}%'
    if 'numMemory' in flags:
        return str(value & (2**64 - 1))
    if 'numOctal' in flags:
        return format(value & (2**64 - 1), 'o')
    return str(value)


def entries(config_c):
    """Each create*Config(...) of static_configs, as (kind, [args],
    condition): the #if conditions it sits under, joined."""
    table = config_c[config_c.index('standardConfig static_configs[]'):]
    table = table[:table.index('\n};') + 3]
    out, conditions, pos = [], [], 0
    lines = table.splitlines(keepends=True)
    offsets = []
    for line in lines:
        offsets.append(pos)
        pos += len(line)
    cond_at = []
    for line in lines:
        s = line.strip()
        if s.startswith(('#ifdef', '#ifndef', '#if ')):
            conditions.append(s)
        elif s.startswith('#endif'):
            conditions.pop()
        elif s.startswith('#else'):
            conditions[-1] = 'not ' + conditions[-1]
        cond_at.append(' and '.join(conditions))
    for m in re.finditer(r'create(\w+)Config\(', table):
        depth, i = 1, m.end()
        while depth:
            depth += {'(': 1, ')': -1}.get(table[i], 0)
            i += 1
        line_no = max(k for k, off in enumerate(offsets) if off <= m.start())
        out.append((m.group(1), split_args(table[m.end():i - 1]), cond_at[line_no]))
    return out


def flag_names(text, table):
    names = []
    for f in (p.strip() for p in text.split('|')):
        if f in ('MODIFIABLE_CONFIG', 'INTEGER_CONFIG', ''):
            continue
        if f not in table:
            raise ValueError(f'unknown flag {f}')
        if table[f]:
            names.append(table[f])
    return names


def collect(redis_src):
    redis_src = Path(redis_src)
    config_c = (redis_src / 'src' / 'config.c').read_text(errors='replace')
    evaluate = Evaluator(macros(redis_src))
    enum_tables = enums(config_c, evaluate)
    specs = []
    for kind, args, condition in entries(config_c):
        name, alias, flags = c_string(args[0]), c_string(args[1]), flag_names(args[2], FLAGS)
        spec = {'name': name, 'alias': alias or '', 'flags': flags, 'condition': condition,
                'num': '', 'numflags': [], 'lower': 0, 'upper': 0, 'default': '', 'enum': [],
                'valid': '', 'apply': '', 'empty_is_null': False}
        if kind == 'Bool':
            _, _, _, _, default, valid, apply = args
            spec.update(kind='configBool', default='yes' if evaluate(default) else 'no')
        elif kind in ('String', 'SDS'):
            _, _, _, empty, _, default, valid, apply = args
            spec.update(kind='configString', default=c_string(default, evaluate.defines) or '',
                        empty_is_null=empty == 'EMPTY_STRING_IS_NULL')
        elif kind == 'Enum':
            _, _, _, table, _, default, valid, apply = args
            values = enum_tables[table]
            spec.update(kind='configEnum', enum=values,
                        default=enum_name(values, evaluate(default), 'configMultiArg' in flags))
        elif kind in NUMERIC_TYPES:
            _, _, _, lower, upper, _, default, numflags, valid, apply = args
            num, bits, signed = NUMERIC_TYPES[kind]
            nflags = flag_names(numflags, NUMERIC_FLAGS)
            value = c_cast(evaluate(default), bits, signed)
            spec.update(kind='configNumeric', num=num, numflags=nflags,
                        lower=as_long_long(evaluate(lower)), upper=as_long_long(evaluate(upper)),
                        default=numeric_get(value, nflags))
        elif kind == 'Special':
            _, _, _, _set, _get, _rewrite, apply = args
            valid = 'NULL'
            spec.update(kind='configSpecial')
        else:
            raise ValueError(f'{name}: unknown kind create{kind}Config')
        spec['valid'] = '' if valid == 'NULL' else valid
        spec['apply'] = '' if apply == 'NULL' else apply
        specs.append(spec)
    return specs


def go_string(s):
    return '"' + s.replace('\\', '\\\\').replace('"', '\\"') + '"'


def render(specs):
    lines = [
        f'// Code generated by scripts/generate-config-specs.py from Redis {REDIS_VERSION} src/config.c; DO NOT EDIT.',
        '',
        'package core',
        '',
        f'// redisConfigSpecs is Redis {REDIS_VERSION}\'s table of configuration',
        '// directives, static_configs in src/config.c, in its order: each',
        '// directive\'s name and alias, flags and type, a number\'s bounds and how it',
        '// is read, an enum\'s names, and its default as CONFIG GET writes it. A',
        '// special directive\'s default is config.go\'s. valid and apply name the',
        '// functions Redis checks and applies a new value with, and condition the',
        '// build condition the directive is declared under.',
        'var redisConfigSpecs = []configSpec{',
    ]
    for s in specs:
        fields = [f'name: {go_string(s["name"])}']
        if s['alias']:
            fields.append(f'alias: {go_string(s["alias"])}')
        fields.append(f'kind: {s["kind"]}')
        if s['flags']:
            fields.append('flags: ' + ' | '.join(s['flags']))
        if s['num']:
            fields.append(f'num: {s["num"]}')
        if s['numflags']:
            fields.append('numFlags: ' + ' | '.join(s['numflags']))
        if s['kind'] == 'configNumeric':
            fields.append(f'lower: {s["lower"]}')
            fields.append(f'upper: {s["upper"]}')
        if s['enum']:
            fields.append('enum: []configEnumValue{' + ', '.join(f'{{{go_string(n)}, {v}}}' for n, v in s['enum']) + '}')
        if s['kind'] != 'configSpecial':
            fields.append(f'def: {go_string(s["default"])}')
        if s['empty_is_null']:
            fields.append('emptyIsNull: true')
        if s['valid']:
            fields.append(f'valid: {go_string(s["valid"])}')
        if s['apply']:
            fields.append(f'apply: {go_string(s["apply"])}')
        if s['condition']:
            fields.append(f'condition: {go_string(s["condition"])}')
        lines.append('\t{' + ', '.join(fields) + '},')
    lines.append('}')
    return gofmt('\n'.join(lines) + '\n')


def gofmt(text):
    """text as gofmt lays it out, so that --check compares like with like."""
    return subprocess.run(['gofmt'], input=text, capture_output=True, text=True, check=True).stdout


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.split('\n\n')[0])
    ap.add_argument('--redis-src', required=True, help=f'an unpacked Redis {REDIS_VERSION} source tree')
    ap.add_argument('--out', default=str(OUT))
    ap.add_argument('--check', action='store_true', help='write nothing; fail if the file is not what would be written')
    args = ap.parse_args(argv)
    text = render(collect(args.redis_src))
    out = Path(args.out)
    if args.check:
        if not out.exists() or out.read_text() != text:
            print(f'{out} is not what scripts/generate-config-specs.py writes; run it', file=sys.stderr)
            return 1
        print(f'{out} is current')
        return 0
    out.write_text(text)
    print(f'wrote {out}: {text.count(chr(10) + chr(9) + "{")} directives')
    return 0


if __name__ == '__main__':
    sys.exit(main())
