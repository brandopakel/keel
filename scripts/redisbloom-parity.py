#!/usr/bin/env python3
"""Compare Keel's BF.* and CF.* replies with RedisBloom's, byte for byte.

Keel's Bloom and cuckoo filter commands answer as RedisBloom does: the same
reply types, the same error text, and the same edge cases. This runs the same
commands against Keel and against Redis with RedisBloom loaded, over RESP2 and
over RESP3, and compares every reply exactly: the bytes on the wire, errors
included, inside arrays and EXEC replies too.

Three parts, each run in both protocols:

- a fixed corpus of edge cases: missing keys, keys of another type, every
  parameter RedisBloom parses (error rate, capacity, EXPANSION, NONSCALING,
  BUCKETSIZE, MAXITERATIONS) at and past its limits and malformed, BF.INFO's
  single fields, full filters, and transactions;
- a seeded random run of BF and CF commands on a few filters, with MULTI
  blocks among them;
- a snapshot of every filter, read back through BF.MEXISTS, CF.MEXISTS,
  CF.COUNT and the INFO commands.

Then Keel alone: the KEEL.DUMP image of every filter is the same over both
protocols, and survives two crash restarts and a rewrite unchanged.

What is not compared byte for byte, and why:

- figures that describe each implementation's own memory layout: Size in
  BF.INFO and CF.INFO, and CF.INFO's Number of buckets. Their types are
  compared, and every other field exactly. Keel's cuckoo filters have one
  geometry, BUCKETSIZE 4, MAXITERATIONS 500 and EXPANSION 0, so the filters
  compared field by field are reserved with it on both servers. A filter of
  RedisBloom's default geometry has those three fields masked too, and with
  them Number of filters and Number of items deleted: RedisBloom grows such a
  filter where Keel's refuses an item, and compacts it later, which resets its
  count of deletions;
- documented differences, which report.json lists: a cuckoo geometry
  Keel does not have is refused rather than accepted, and filling a filter
  takes a different number of items, since the two hash differently. Keel's
  answer to each is checked exactly; RedisBloom's is recorded beside it.

False positives differ between the two (they hash differently), so filters
are sized so that none is likely in a run, and a seeded run is the same run on
every attempt.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import random
import socket
import subprocess
import time

from validation_lib import Server, rewrite, sha256

# Keel's one cuckoo geometry, as CF.RESERVE options.
NATIVE = ['BUCKETSIZE', 4, 'MAXITERATIONS', 500, 'EXPANSION', 0]
ITEMS = [b'a', b'b', b'c', b'item', b'', b'\x00\xff\r\n', b'x' * 64, b'007', b'-0', b'Item'] + \
        [f'i{n}'.encode() for n in range(14)]


def encode(parts):
    parts = [p if isinstance(p, bytes) else str(p).encode() for p in parts]
    return b'*%d\r\n' % len(parts) + b''.join(b'$%d\r\n' % len(p) + p + b'\r\n' for p in parts)


def read_reply(stream, depth=0):
    """One reply: the exact bytes it arrived as, and a typed tree of it."""
    if depth > 16:
        raise ValueError('RESP nesting limit')
    line = stream.readline(1 << 20)
    if not line.endswith(b'\r\n'):
        raise ValueError(f'invalid RESP header {line!r}')
    kind, body = line[:1], line[1:-2]
    simple = {b'+': 'simple', b'-': 'error', b',': 'double', b'#': 'bool', b'(': 'bignum'}
    if kind in simple:
        return line, (simple[kind], body)
    if kind == b':':
        int(body)
        return line, ('int', body)
    if kind == b'_':
        return line, ('null',)
    n = int(body)
    if kind in (b'$', b'=', b'!'):
        if n == -1:
            return line, ('null-bulk',)
        data = stream.read(n + 2)
        if len(data) != n + 2 or data[-2:] != b'\r\n':
            raise ValueError('invalid bulk reply')
        return line + data, ({b'$': 'bulk', b'=': 'verbatim', b'!': 'error'}[kind], data[:-2])
    aggregates = {b'*': 'array', b'~': 'set', b'>': 'push', b'%': 'map'}
    if kind in aggregates:
        if n == -1:
            return line, ('null-array',)
        raw, items = [line], []
        for _ in range(2 * n if kind == b'%' else n):
            r, item = read_reply(stream, depth + 1)
            raw.append(r)
            items.append(item)
        return b''.join(raw), (aggregates[kind], items)
    raise ValueError(f'unknown RESP kind {kind!r}')


class Conn:
    def __init__(self, port, password, protocol):
        self.socket = socket.create_connection(('127.0.0.1', port), timeout=30)
        self.stream = self.socket.makefile('rb')
        if protocol == 3:
            _, reply = self.call('HELLO', 3, 'AUTH', 'default', password)
            assert reply[0] == 'map', reply
        else:
            _, reply = self.call('AUTH', password)
            assert reply == ('simple', b'OK'), reply

    def send(self, *parts):
        self.socket.sendall(encode(parts))

    def call(self, *parts):
        self.send(*parts)
        return read_reply(self.stream)

    def close(self):
        self.stream.close()
        self.socket.close()


def is_arity(tree):
    return tree[0] == 'error' and tree[1].startswith(b'ERR wrong number of arguments')


def normalize(tree, mask=frozenset(), mask_values=False):
    """The tree compared, with the masked fields replaced by their types."""
    kind = tree[0]
    if mask_values and kind in ('int', 'double', 'bulk'):
        return ('masked', kind)
    if kind in ('array', 'map', 'set', 'push'):
        items = [normalize(item) for item in tree[1]]
        if kind in ('array', 'map') and mask:
            for i in range(0, len(items) - 1):
                if items[i][0] in ('simple', 'bulk') and items[i][1].decode(errors='replace') in mask:
                    items[i + 1] = ('masked', items[i + 1][0])
        if mask_values:
            items = [('masked', item[0]) if item[0] in ('int', 'double') else item for item in items]
        return (kind, items)
    return tree


# Fields of BF.INFO and CF.INFO that describe each implementation's own memory.
LAYOUT = frozenset({'Size', 'Number of buckets'})
# And those of a cuckoo filter of RedisBloom's default geometry, which grows
# where Keel's refuses, and compacts what it grew, resetting its count of
# deletions when it does.
GROWTH = frozenset({'Number of filters', 'Number of items deleted'})
DEFAULT_GEOMETRY = LAYOUT | GROWTH | {'Bucket size', 'Max iterations', 'Expansion rate'}
GEOMETRY_FIELDS = {'BUCKETSIZE': 'Bucket size', 'MAXITERATIONS': 'Max iterations', 'EXPANSION': 'Expansion rate'}
KEEL_GEOMETRY = b'-ERR this server\'s cuckoo filters have BUCKETSIZE 4, MAXITERATIONS 500 and EXPANSION 0 only\r\n'


def geometry_mask(options):
    """CF.INFO's mask for a filter reserved with these options: a geometry
    field RedisBloom took its default for is Keel's own value."""
    given = {str(option).upper() for option in options}
    mask = LAYOUT | {field for option, field in GEOMETRY_FIELDS.items() if option not in given}
    return mask | GROWTH if 'Expansion rate' in mask else mask


def step(*command, mode='exact', mask=frozenset(), mask_values=False, note=None, keel=None):
    return {'command': list(command), 'mode': mode, 'mask': mask, 'mask_values': mask_values,
            'note': note, 'keel': keel}


class Keys:
    """Fresh key names, the same on both servers."""
    def __init__(self):
        self.n = 0

    def __call__(self, prefix):
        self.n += 1
        return f'{prefix}:{self.n}'


def corpus():
    k = Keys()
    s = []
    add = lambda *command, **options: s.append(step(*command, **options))
    arity = lambda *command: s.append(step(*command, mode='arity'))

    # Keys of every other type, and one filter of each kind, to aim at.
    add('SET', 'str', 'v')
    add('HSET', 'hash', 'f', 'v')
    add('RPUSH', 'list', 'a')
    add('BF.RESERVE', 'bf:held', 0.000001, 1000)
    add('CF.RESERVE', 'cf:held', 100000, *NATIVE)

    # BF.RESERVE: the error rate, as Redis parses a double, and its range.
    for rate in ['abc', '', ' 0.1', '0.1 ', '0', '-0', '-0.1', '1', '1.0', '1.5', 'nan', 'NaN', '-nan',
                 'inf', '+inf', '-inf', 'infinity', 'Infinity', '1e-400', '1e400', '0x1p-3', '0X1P-3',
                 '+0.1', '.5', '5.', '0.', '1e-2', '1E-2', '1e+0', '0.5', '0.25', '0.2500001',
                 '0.9999999', '4.9e-324', '2.2250738585072014e-308', '0.01abc', '0,01', '0.0.1',
                 '1e', 'e1', '--0.1', '0.01\x00', '\t0.1', '0.1\n', '00.1', '0.000000000001',
                 '0x10', '0x.8', '0x1.8', '0x1p', '0x', '0x_1p0', '1_0', '0x1p-1100', '0x0p0', '-0x1p-3',
                 '1e-320', '0.1e1', '1.e-1', 'INF', 'nan(1)', '0x1P+1', '0x8p-4']:
        add('BF.RESERVE', k('bf:rate'), rate, 100)
    # The capacity, as Redis parses an integer, and its range. A capacity at
    # the top of the range is not reserved: RedisBloom would allocate it.
    for capacity in ['abc', '', ' 5', '5 ', '0', '-0', '-1', '007', '0x10', '+5', '1.0', '1e3', '1',
                     '1073741825', '9223372036854775807', '9223372036854775808',
                     '-9223372036854775808', '-9223372036854775809', '18446744073709551616']:
        add('BF.RESERVE', k('bf:capacity'), 0.01, capacity)
    # EXPANSION and NONSCALING, found anywhere among the arguments.
    for options in [['EXPANSION'], ['EXPANSION', 'abc'], ['EXPANSION', ''], ['EXPANSION', '0'],
                    ['EXPANSION', '-1'], ['EXPANSION', '1'], ['EXPANSION', '2'], ['EXPANSION', '32768'],
                    ['EXPANSION', '32769'], ['EXPANSION', '007'], ['EXPANSION', '+2'], ['EXPANSION', '2.0'],
                    ['expansion', '3'], ['ExPaNsIoN', '3'], ['NONSCALING'], ['nonscaling'],
                    ['NONSCALING', 'EXPANSION', '2'], ['EXPANSION', '2', 'NONSCALING'],
                    ['NONSCALING', 'EXPANSION', '0'], ['EXPANSION', '0', 'NONSCALING'],
                    ['NONSCALING', 'EXPANSION', 'abc'], ['NONSCALING', 'EXPANSION', '-1'],
                    ['NONSCALING', 'EXPANSION', '40000'], ['EXPANSION', 'NONSCALING'],
                    ['NONSCALING', 'EXPANSION'], ['NONSCALING', 'NONSCALING'], ['FOO'], ['FOO', 'BAR'],
                    ['FOO', 'BAR', 'BAZ'], ['EXPANSION', '3', 'FOO'], ['NONSCALINGX'], ['EXPANSIONS', '2']]:
        key = k('bf:options')
        add('BF.RESERVE', key, 0.000001, 100, *options)
        add('BF.INFO', key, mask=LAYOUT)
    for options in [['EXPANSION', '2', 'EXPANSION', '3'], ['NONSCALING', 'FOO', 'BAR', 'BAZ']]:
        arity('BF.RESERVE', k('bf:options'), 0.01, 100, *options)
    # The options are looked for among all the arguments, the key included.
    for key in ['expansion', 'nonscaling', 'EXPANSION']:
        add('BF.RESERVE', key, 0.000001, 100)
        add('BF.INFO', key, mask=LAYOUT)
    add('BF.RESERVE', k('bf:quirk'), 0.000001, 100, 'EXPANSION', 'EXPANSION')
    # Which check comes first: parameters, then the key.
    add('BF.RESERVE', 'bf:held', 0.01, 100)
    add('BF.RESERVE', 'str', 0.01, 100)
    add('BF.RESERVE', 'cf:held', 0.01, 100)
    add('BF.RESERVE', 'hash', 0.01, 100, 'NONSCALING')
    add('BF.RESERVE', 'str', 'abc', 100)
    add('BF.RESERVE', 'bf:held', 0.01, 0)
    add('BF.RESERVE', 'bf:held', 2, 'abc')
    add('BF.RESERVE', 'bf:held', 0.01, 100, 'EXPANSION')
    add('BF.RESERVE', 'cf:held', 0.01, 100, 'NONSCALING', 'EXPANSION', 2)
    for args in [[], ['k'], ['k', 0.01]]:
        arity('BF.RESERVE', *args)

    # BF.ADD and BF.MADD: created on demand, refused for another type.
    add('BF.ADD', 'bf:auto', 'a')
    add('BF.ADD', 'bf:auto', 'a')
    add('BF.ADD', 'bf:auto', b'')
    for key in ['str', 'hash', 'list', 'cf:held']:
        add('BF.ADD', key, 'a')
        add('BF.MADD', key, 'a', 'b')
    add('BF.MADD', 'bf:auto2', 'a', 'b', 'a')
    add('BF.MADD', 'bf:auto2', 'c')
    # A filter that cannot grow refuses the item that would make it, and
    # BF.MADD stops there; an item it already holds still answers 0.
    # The filters hold fifty, so that a false positive - the two hash
    # differently - is unlikely before they fill.
    fill = [f'f{n}' for n in range(49)]
    for options in [['NONSCALING'], ['EXPANSION', 0]]:
        key = k('bf:full')
        add('BF.RESERVE', key, 0.000001, 50, *options)
        add('BF.MADD', key, *fill)
        add('BF.ADD', key, 'a')
        add('BF.ADD', key, 'b')
        add('BF.ADD', key, 'a')
        add('BF.MADD', key, 'a', 'c', 'b', 'd')
        add('BF.MADD', key, 'f1', 'a')
        add('BF.INFO', key, mask=LAYOUT)
        add('BF.INFO', key, 'EXPANSION')
        add('BF.INFO', key, 'items')
        key = k('bf:full')
        add('BF.RESERVE', key, 0.000001, 50, *options)
        add('BF.MADD', key, *fill, 'a', 'b', 'c', 'f0')
    # A filter that grows.
    add('BF.RESERVE', 'bf:grow', 0.000001, 4, 'EXPANSION', 3)
    add('BF.MADD', 'bf:grow', *[f'g{n}' for n in range(20)])
    add('BF.INFO', 'bf:grow', mask=LAYOUT)
    for args in [['k'], ['k', 'a', 'b']]:
        arity('BF.ADD', *args)
    arity('BF.MADD', 'k')

    # BF.EXISTS and BF.MEXISTS answer no for a key of another type.
    for key in ['bf:auto', 'bf:missing', 'str', 'hash', 'cf:held']:
        add('BF.EXISTS', key, 'a')
        add('BF.EXISTS', key, 'zz')
        add('BF.MEXISTS', key, 'a', 'zz', b'')
    for args in [['k'], ['k', 'a', 'b']]:
        arity('BF.EXISTS', *args)
    arity('BF.MEXISTS', 'k')

    # BF.INFO, whole and by field.
    add('BF.INFO', 'bf:auto', mask=LAYOUT)
    for key in ['bf:missing', 'str', 'cf:held', 'list']:
        add('BF.INFO', key)
        add('BF.INFO', key, 'capacity')
        add('BF.INFO', key, 'nosuchfield')
    for field in ['CAPACITY', 'capacity', 'Capacity', 'FILTERS', 'filters', 'ITEMS', 'items', 'EXPANSION', 'expansion']:
        add('BF.INFO', 'bf:auto', field)
    add('BF.INFO', 'bf:auto', 'SIZE', mask_values=True)
    add('BF.INFO', 'bf:auto', 'size', mask_values=True)
    for field in ['nosuchfield', '', 'capacityx', 'cap', 'Number of filters']:
        add('BF.INFO', 'bf:auto', field)
    arity('BF.INFO')
    arity('BF.INFO', 'bf:auto', 'capacity', 'size')

    # CF.RESERVE: the capacity, and each option RedisBloom parses.
    for capacity in ['abc', '', ' 4', '4 ', '0', '1', '2', '3', '4', '5', '-1', '-0', '007', '+5', '1.0',
                     '1073741825', '9223372036854775807', '9223372036854775808']:
        add('CF.RESERVE', k('cf:capacity'), capacity)
    for options in [['BUCKETSIZE', 'abc'], ['BUCKETSIZE', ''], ['BUCKETSIZE', '0'], ['BUCKETSIZE', '-1'],
                    ['BUCKETSIZE', '256'], ['BUCKETSIZE', '4'], ['bucketsize', '4'], ['BUCKETSIZE', '007'],
                    ['BUCKETSIZE', '+4'], ['MAXITERATIONS', 'abc'], ['MAXITERATIONS', '0'],
                    ['MAXITERATIONS', '-1'], ['MAXITERATIONS', '65536'], ['MAXITERATIONS', '500'],
                    ['maxiterations', '500'], ['EXPANSION', 'abc'], ['EXPANSION', '-1'], ['EXPANSION', '32769'],
                    ['EXPANSION', '0'], ['expansion', '0'], NATIVE, NATIVE[4:] + NATIVE[:4],
                    ['MAXITERATIONS', '0', 'BUCKETSIZE', '0'], ['BUCKETSIZE', '0', 'EXPANSION', '-1'],
                    ['EXPANSION', '-1', 'MAXITERATIONS', 'x'], ['FOO', 'BAR'], ['FOO', '1', 'BAR', '2'],
                    ['BUCKETSIZE', '4', 'BUCKETSIZE', '2'], ['BUCKETSIZE', '4', 'FOO', 'BAR'],
                    ['MAXITERATIONS', '500', 'MAXITERATIONS', '20', 'EXPANSION', '0']]:
        key = k('cf:options')
        add('CF.RESERVE', key, 1000, *options)
        add('CF.INFO', key, mask=geometry_mask(options))
    for options in [['BUCKETSIZE'], ['BUCKETSIZE', '4', 'MAXITERATIONS'], ['FOO']]:
        arity('CF.RESERVE', k('cf:options'), 1000, *options)
    # The capacity against the bucket size, and which check comes first.
    for capacity, options in [(7, NATIVE), (8, NATIVE), ('abc', ['BUCKETSIZE', 0]), (0, ['BUCKETSIZE', 0]),
                              (0, ['MAXITERATIONS', 0]), (3, ['EXPANSION', 'x']), (1073741825, NATIVE),
                              (1, ['BUCKETSIZE', 'x', 'MAXITERATIONS', 0]), (7, ['BUCKETSIZE', 4, 'EXPANSION', 40000])]:
        add('CF.RESERVE', k('cf:order'), capacity, *options)
    add('CF.RESERVE', 'bucketsize', 1000)
    add('CF.RESERVE', 'cf:held', 1000)
    add('CF.RESERVE', 'cf:held', 1000, *NATIVE)
    add('CF.RESERVE', 'str', 1000)
    add('CF.RESERVE', 'bf:held', 1000, *NATIVE)
    add('CF.RESERVE', 'str', 'abc')
    add('CF.RESERVE', 'cf:held', 0)
    add('CF.RESERVE', 'cf:held', 1000, 'BUCKETSIZE', 0)
    for args in [[], ['k']]:
        arity('CF.RESERVE', *args)
    # A geometry Keel's filters do not have: Keel refuses it, where RedisBloom
    # builds it.
    for options in [['BUCKETSIZE', 2], ['MAXITERATIONS', 20], ['EXPANSION', 1], ['EXPANSION', 4],
                    ['BUCKETSIZE', 4, 'MAXITERATIONS', 500, 'EXPANSION', 2], ['bucketsize', 255]]:
        add('CF.RESERVE', k('cf:geometry'), 1000, *options, mode='keel', keel=KEEL_GEOMETRY,
            note='a geometry other than BUCKETSIZE 4, MAXITERATIONS 500 and EXPANSION 0 is refused')
    for options in [['BUCKETSIZE', 4, 'MAXITERATIONS', 500], ['EXPANSION', 0], ['MAXITERATIONS', 500, 'FOO', 'BAR']]:
        key = k('cf:partial')
        add('CF.RESERVE', key, 1000, *options)
        add('CF.INFO', key, mask=geometry_mask(options))

    # CF.ADD and CF.ADDNX.
    add('CF.ADD', 'cf:auto', 'a')
    add('CF.ADD', 'cf:auto', 'a')
    add('CF.ADDNX', 'cf:auto', 'a')
    add('CF.ADDNX', 'cf:auto', 'b')
    add('CF.ADDNX', 'cf:auto3', 'x')
    add('CF.ADD', 'cf:held', b'')
    for key in ['str', 'hash', 'list', 'bf:held']:
        add('CF.ADD', key, 'a')
        add('CF.ADDNX', key, 'a')
    # A full filter refuses the item; the two fill at different points.
    for command in ['CF.ADD', 'CF.ADDNX']:
        key = k('cf:tiny')
        add('CF.RESERVE', key, 8, *NATIVE)
        s.append(step(command, key, mode='fill', note='filled until refused; the item count differs'))
        add('CF.ADDNX', key, 'fill:0')
        add('CF.EXISTS', key, 'fill:0')
        add('CF.INFO', key, mask=LAYOUT | {'Number of items inserted'})
    for command, args in [('CF.ADD', ['k']), ('CF.ADD', ['k', 'a', 'b']), ('CF.ADDNX', ['k']), ('CF.ADDNX', ['k', 'a', 'b'])]:
        arity(command, *args)

    # CF.EXISTS, CF.MEXISTS and CF.COUNT answer no, and 0, for a key of
    # another type; CF.DEL answers "Not found".
    for key in ['cf:auto', 'cf:missing', 'str', 'hash', 'bf:held']:
        add('CF.EXISTS', key, 'a')
        add('CF.EXISTS', key, 'zz')
        add('CF.MEXISTS', key, 'a', 'zz', b'')
        add('CF.COUNT', key, 'a')
        add('CF.COUNT', key, 'zz')
    for key in ['cf:missing', 'str', 'hash', 'bf:held']:
        add('CF.DEL', key, 'a')
    add('CF.DEL', 'cf:auto', 'a')
    add('CF.DEL', 'cf:auto', 'a')
    add('CF.DEL', 'cf:auto', 'a')
    add('CF.COUNT', 'cf:auto', 'a')
    for command in ['CF.EXISTS', 'CF.COUNT', 'CF.DEL']:
        arity(command, 'k')
        arity(command, 'k', 'a', 'b')
    arity('CF.MEXISTS', 'k')

    # CF.INFO.
    add('CF.ADD', 'cf:held', 'a')
    add('CF.ADD', 'cf:held', 'b')
    add('CF.DEL', 'cf:held', 'a')
    add('CF.INFO', 'cf:held', mask=LAYOUT)
    add('CF.INFO', 'cf:auto', mask=DEFAULT_GEOMETRY)
    for key in ['cf:missing', 'str', 'bf:held', 'hash']:
        add('CF.INFO', key)
    arity('CF.INFO')
    arity('CF.INFO', 'cf:held', 'x')

    # A command with the wrong number of arguments is refused for that before
    # its key is looked at.
    for command in [['BF.RESERVE', 'str', 0.01], ['BF.RESERVE', 'cf:held', 0.01, 100, 'a', 'b', 'c', 'd'],
                    ['BF.ADD', 'str'], ['BF.MADD', 'cf:held'], ['BF.EXISTS', 'str'], ['BF.INFO', 'str', 'a', 'b'],
                    ['CF.RESERVE', 'str'], ['CF.RESERVE', 'bf:held', 1000, 'BUCKETSIZE'], ['CF.ADD', 'str'],
                    ['CF.ADDNX', 'bf:held'], ['CF.DEL', 'str'], ['CF.COUNT', 'str', 'a', 'b'], ['CF.INFO', 'str', 'x']]:
        arity(*command)

    # Transactions: errors at EXEC stand in their places, and a refusal while
    # queueing aborts the block.
    tx = lambda *commands: s.append(step('MULTI', mode='tx', note=[c if isinstance(c, dict) else step(*c) for c in commands]))
    tx(
        ['BF.ADD', 'bf:tx', 'a'], ['BF.ADD', 'str', 'a'], ['BF.EXISTS', 'str', 'a'], ['CF.DEL', 'cf:missing', 'a'],
        ['BF.INFO', 'bf:missing'], ['BF.INFO', 'bf:tx', 'items'], ['BF.INFO', 'bf:tx', 'bogus'],
        ['CF.MEXISTS', 'cf:held', 'b', 'zz'], ['CF.RESERVE', 'cf:held', 10], ['CF.RESERVE', 'cf:tx', 'abc'],
        ['BF.RESERVE', 'bf:tx2', 0.000001, 10, 'NONSCALING', 'EXPANSION', 2], ['BF.MADD', 'bf:tx', 'a', 'b'],
        ['CF.COUNT', 'cf:held', 'b'], ['BF.INFO', 'bf:tx', 'capacity', 'extra'], step('BF.INFO', 'bf:tx', mask=LAYOUT),
        step('CF.INFO', 'cf:held', mask=LAYOUT), ['BF.ADD', 'str', 'x'], ['CF.ADD', 'cf:tx', 'x'], ['CF.DEL', 'cf:tx', 'x'], ['EXEC'])
    tx(['BF.ADD', 'bf:tx', 'c'], ['CF.RESERVE', 'k'], ['EXEC'])
    tx(['CF.ADD', 'cf:tx', 'c'], ['BF.INFO'], ['EXEC'])
    tx(['CF.RESERVE', 'cf:tx3', 1000, 'BUCKETSIZE', 0], ['CF.INFO', 'cf:tx3'], ['EXEC'])
    tx(['BF.ADD', 'str'], ['CF.INFO', 'str', 'x'], ['EXEC'])
    tx(['BF.RESERVE', 'str', 0.01, 100, 'a', 'b', 'c', 'd'], ['EXEC'])
    add('BF.EXISTS', 'bf:tx', 'c')
    add('CF.EXISTS', 'cf:tx', 'c')
    return s


def snapshot_commands(keys, kinds, native):
    """Each filter, read back: membership and counts of its items, and INFO."""
    out = []
    for key in sorted(keys):
        if kinds[key] == 'bloom':
            out.append(step('BF.MEXISTS', key, *keys[key]))
            out.append(step('BF.INFO', key, mask=LAYOUT))
        else:
            mask = LAYOUT if key in native else DEFAULT_GEOMETRY
            out.append(step('CF.MEXISTS', key, *keys[key]))
            out.append(step('CF.COUNT', key, keys[key][0]))
            out.append(step('CF.INFO', key, mask=mask))
    return out


class Random:
    """Seeded BF and CF commands on a few filters.

    Reserved filters are large and tight, so neither server is likely to see
    a false positive; filters created on demand take RedisBloom's default size,
    so each is only ever given, and asked about, four items. A cuckoo filter
    holds at most three copies of an item here: RedisBloom's default buckets
    hold two fingerprints and it grows a filter where Keel's would refuse,
    which is a difference in when a filter fills, not in a reply.
    """
    def __init__(self, seed):
        self.rng = random.Random(seed)
        self.copies = {}
        self.cuckoo = {f'cf:r{i}': ITEMS for i in range(3)}
        self.cuckoo.update({f'cf:a{i}': ITEMS[4 * i:4 * i + 4] for i in range(3)})
        self.bloom = {f'bf:r{i}': ITEMS for i in range(3)}
        self.bloom.update({f'bf:a{i}': ITEMS[4 * i:4 * i + 4] for i in range(3)})

    def setup(self):
        out = [step('BF.RESERVE', key, 0.0000001, 100000) for key in self.bloom if key.startswith('bf:r')]
        out += [step('CF.RESERVE', key, 100000, *NATIVE) for key in self.cuckoo if key.startswith('cf:r')]
        return out

    def command(self):
        rng = self.rng
        # A missing key is only read, so it stays missing.
        if rng.random() < .5:
            key = rng.choice(list(self.bloom) + ['bf:missing', 'str', 'cf:r0'])
            items = self.bloom.get(key, ITEMS)
            item, other = rng.choice(items), rng.choice(items)
            choices = [step('BF.EXISTS', key, item), step('BF.MEXISTS', key, item, other),
                       step('BF.INFO', key, mask=LAYOUT), step('BF.INFO', key, rng.choice(['ITEMS', 'capacity', 'Filters', 'expansion']))]
            if key != 'bf:missing':
                choices += [step('BF.ADD', key, item), step('BF.MADD', key, item, other), step('BF.RESERVE', key, 0.01, 100)]
            return rng.choice(choices)
        key = rng.choice(list(self.cuckoo) + ['cf:missing', 'str', 'bf:r0'])
        items = self.cuckoo.get(key, ITEMS)
        item, other = rng.choice(items), rng.choice(items)
        count = self.copies.get((key, item), 0)
        mask = LAYOUT if key.startswith('cf:r') else DEFAULT_GEOMETRY
        choices = [step('CF.EXISTS', key, item), step('CF.MEXISTS', key, item, other),
                   step('CF.COUNT', key, item), step('CF.DEL', key, item), step('CF.INFO', key, mask=mask)]
        if key != 'cf:missing':
            choices += [step('CF.ADDNX', key, item), step('CF.RESERVE', key, 1000)]
            if count < 3:
                choices.append(step('CF.ADD', key, item))
        chosen = rng.choice(choices)
        self.track(chosen['command'])
        return chosen

    def track(self, command):
        name, key = command[0], command[1]
        if key not in self.cuckoo:
            return
        item = command[2] if len(command) > 2 else None
        count = self.copies.get((key, item), 0)
        if name == 'CF.ADD' or (name == 'CF.ADDNX' and count == 0):
            self.copies[(key, item)] = count + 1
        elif name == 'CF.DEL' and count:
            self.copies[(key, item)] = count - 1

    def transaction(self):
        commands = [self.command() for _ in range(self.rng.randrange(1, 5))]
        if self.rng.random() < .2:
            commands.insert(self.rng.randrange(len(commands) + 1),
                            step(*self.rng.choice([['BF.ADD', 'k'], ['CF.INFO'], ['CF.RESERVE', 'k']])))
        return step('MULTI', mode='tx', note=commands + [step('EXEC')])


class Runner:
    def __init__(self, keel, redis, protocol, trace):
        self.keel, self.redis, self.protocol, self.trace = keel, redis, protocol, trace
        self.checks, self.mismatches, self.differences = 0, [], []

    def record(self, entry):
        self.trace.write(json.dumps(entry, default=lambda b: b.decode('latin-1')) + '\n')

    def compare(self, item, got, want):
        command = item['command']
        self.checks += 1
        entry = {'protocol': self.protocol, 'command': [str(c) if not isinstance(c, bytes) else c.decode('latin-1') for c in command],
                 'mode': item['mode'], 'keel': got[0].decode('latin-1'), 'redisbloom': want[0].decode('latin-1')}
        if item['mode'] == 'arity':
            # Both refuse for the count, in the same words: RedisBloom names
            # the command in lower case, as Redis does.
            ok = is_arity(got[1]) and is_arity(want[1]) and got[0] == want[0]
        elif item['mode'] == 'keel':
            ok = got[0] == item['keel']
            entry['note'] = item['note']
            self.differences.append(entry)
        else:
            # A reply's tree keeps every byte of it - types, lengths, integers
            # as sent - so equal trees are equal replies, apart from what
            # normalize masks.
            ok = normalize(got[1], item['mask'], item['mask_values']) == normalize(want[1], item['mask'], item['mask_values'])
        entry['ok'] = ok
        self.record(entry)
        if not ok:
            self.mismatches.append(entry)

    def run(self, item):
        if item['mode'] == 'tx':
            return self.transaction(item)
        if item['mode'] == 'fill':
            return self.fill(item)
        got = self.keel.call(*item['command'])
        want = self.redis.call(*item['command'])
        self.compare(item, got, want)
        if item['mode'] == 'keel':
            # RedisBloom built what Keel refused; neither keeps it, so the
            # two keyspaces stay alike.
            for conn in (self.keel, self.redis):
                conn.call('DEL', item['command'][1])

    def transaction(self, item):
        """A MULTI block: each reply while queueing, and EXEC's, whose
        elements are compared as their commands' replies would be alone."""
        queued = []
        for entry in [step('MULTI')] + item['note']:
            got, want = self.keel.call(*entry['command']), self.redis.call(*entry['command'])
            if entry['command'] == ['EXEC'] and got[1][0] == want[1][0] == 'array' and len(got[1][1]) == len(want[1][1]) == len(queued):
                self.checks += 1
                a = [normalize(x, q['mask'], q['mask_values']) for x, q in zip(got[1][1], queued)]
                b = [normalize(x, q['mask'], q['mask_values']) for x, q in zip(want[1][1], queued)]
                record = {'protocol': self.protocol, 'command': ['EXEC'], 'mode': 'tx', 'ok': a == b,
                          'keel': got[0].decode('latin-1'), 'redisbloom': want[0].decode('latin-1')}
                self.record(record)
                if a != b:
                    self.mismatches.append(record)
                continue
            self.compare(entry, got, want)
            if want[1] == ('simple', b'QUEUED'):
                queued.append(entry)

    def fill(self, item):
        """Adds items until each server refuses one; compares the refusals."""
        refusals = []
        for conn in (self.keel, self.redis):
            for n in range(200):
                raw, tree = conn.call(*item['command'], f'fill:{n}')
                if tree[0] == 'error':
                    refusals.append((raw, tree, n))
                    break
            else:
                refusals.append((b'(never refused)', ('none',), 200))
        (got_raw, got_tree, got_n), (want_raw, want_tree, want_n) = refusals
        self.compare(step(*item['command'], '<fill:n>'), (got_raw, got_tree), (want_raw, want_tree))
        self.differences.append({'protocol': self.protocol, 'command': item['command'] + ['fill:n'],
                                 'note': item['note'], 'keel_items_before_refusal': got_n,
                                 'redisbloom_items_before_refusal': want_n})


def start_redis(redis, module, password, root):
    with socket.socket() as reservation:
        reservation.bind(('127.0.0.1', 0))
        port = reservation.getsockname()[1]
    log = (root / 'redis.log').open('w')
    env = {key: value for key, value in os.environ.items() if key in ('PATH', 'HOME', 'TMPDIR')}
    process = subprocess.Popen([str(redis.absolute()), '-'], stdin=subprocess.PIPE, stdout=log, stderr=log, env=env)
    process.stdin.write((f'bind 127.0.0.1\nport {port}\nsave ""\nappendonly no\nrequirepass {password}\n'
                         f'loadmodule {module.absolute()}\n').encode())
    process.stdin.close()
    deadline = time.monotonic() + 10
    while True:
        if process.poll() is not None:
            raise RuntimeError(f'Redis startup failed; see {root}/redis.log')
        try:
            Conn(port, password, 2).close()
            return process, port, log
        except OSError:
            if time.monotonic() > deadline:
                raise
            time.sleep(.02)


def dumps(conn, keys):
    out = {}
    for key in sorted(keys):
        raw, tree = conn.call('KEEL.DUMP', key)
        assert tree[0] == 'bulk', (key, raw[:80])
        out[key] = hashlib.sha256(tree[1]).hexdigest()
    return out


# TYPE's names for the two filter types: Keel's, and RedisBloom's.
FILTER_TYPES = {b'bloom': 'bloom', b'MBbloom--': 'bloom', b'cuckoo': 'cuckoo', b'MBbloomCF': 'cuckoo'}


def filter_keys(conn):
    """Every filter on a server, and which kind it is."""
    _, tree = conn.call('KEYS', '*')
    keys = {}
    for item in tree[1]:
        name = item[1].decode('latin-1')
        _, kind = conn.call('TYPE', name)
        if kind[1] in FILTER_TYPES:
            keys[name] = FILTER_TYPES[kind[1]]
    return keys


def run(args):
    os.umask(0o077)
    root = args.out.resolve()
    root.mkdir(parents=True, exist_ok=False)
    report = {'status': 'running', 'seed': args.seed, 'steps_per_protocol': args.steps,
              'binary_sha256': sha256(args.bin), 'redis_sha256': sha256(args.redis),
              'redis_module_sha256': sha256(args.redis_module), 'harness_sha256': sha256(__file__),
              'protocols': {}, 'mismatches': [], 'documented_differences': []}
    server = Server(args.bin, root / 'keel', policy='always')
    server.env = {key: value for key, value in server.env.items() if key in ('PATH', 'HOME', 'TMPDIR', 'KEEL_VALIDATION_PASSWORD')}
    redis = log = None
    trace = (root / 'replies.jsonl').open('w')
    conns = []
    try:
        server.start()
        redis, port, log = start_redis(args.redis, args.redis_module, server.password, root)
        probe = Conn(port, server.password, 2)
        _, modules = probe.call('MODULE', 'LIST')
        report['redis_modules'] = [{item[1][i][1].decode(): item[1][i + 1][1].decode() for i in range(0, 4, 2)}
                                   for item in modules[1]]
        _, version = probe.call('INFO', 'server')
        report['redis_version'] = next(line.split(':', 1)[1] for line in version[1].decode().splitlines() if line.startswith('redis_version:'))
        probe.close()
        final_keys = None
        for protocol in (2, 3):
            keel, ref = Conn(server.port, server.password, protocol), Conn(port, server.password, protocol)
            conns += [keel, ref]
            for conn in (keel, ref):
                assert conn.call('FLUSHDB')[1] == ('simple', b'OK')
            runner = Runner(keel, ref, protocol, trace)
            for item in corpus():
                runner.run(item)
            corpus_checks = runner.checks
            generator = Random(args.seed + protocol)
            for item in generator.setup():
                runner.run(item)
            for _ in range(args.steps):
                runner.run(generator.transaction() if generator.rng.random() < .1 else generator.command())
            filters, reference_filters = filter_keys(keel), filter_keys(ref)
            if filters != reference_filters:
                runner.mismatches.append({'protocol': protocol, 'check': 'the same filters, of the same kinds, exist on both',
                                          'keel_only': sorted(set(filters.items()) - set(reference_filters.items())),
                                          'redisbloom_only': sorted(set(reference_filters.items()) - set(filters.items()))})
                filters = {key: kind for key, kind in filters.items() if reference_filters.get(key) == kind}
            # A filled cuckoo filter is not read back: RedisBloom's 8-bit
            # fingerprints make a false positive likely in one that full.
            snapshot = {key: generator.cuckoo.get(key, generator.bloom.get(key, ITEMS))
                        for key in filters if not key.startswith('cf:tiny')}
            native = {'cf:held', *[key for key in generator.cuckoo if key.startswith('cf:r')]}
            for item in snapshot_commands(snapshot, filters, native):
                runner.run(item)
            report['protocols'][str(protocol)] = {'filters': len(filters)}
            report['protocols'][str(protocol)].update(corpus_checks=corpus_checks, checks=runner.checks,
                                                      mismatches=len(runner.mismatches))
            report['mismatches'] += runner.mismatches
            report['documented_differences'] += runner.differences
            final_keys = filter_keys(keel)
        # Keel alone: a dump image does not depend on the protocol, and the
        # filters replay from the log, and from a rewrite, unchanged.
        images = dumps(conns[0], final_keys)
        assert images == dumps(conns[2], final_keys), 'KEEL.DUMP is the same over RESP2 and RESP3'
        for conn in conns:
            conn.close()
        conns = []
        persistence = {'filters': len(images), 'dump_set_sha256': hashlib.sha256(json.dumps(images, sort_keys=True).encode()).hexdigest()}
        for stage in ('crash restart', 'second crash restart', 'rewrite and restart'):
            if stage.startswith('rewrite'):
                rewrite(server.client)
                server.stop()
            else:
                server.stop(crash=True)
            server.start()
            conn = Conn(server.port, server.password, 3)
            assert dumps(conn, final_keys) == images, f'filters differ after {stage}'
            conn.close()
        persistence['restarts'] = 3
        report['persistence'] = persistence
        report['status'] = 'passed' if not report['mismatches'] else 'failed'
    except BaseException as exc:
        report.update(status='failed', failure=repr(exc))
        raise
    finally:
        for conn in conns:
            conn.close()
        server.stop(check=False)
        if redis is not None and redis.poll() is None:
            redis.terminate()
            try:
                redis.wait(timeout=5)
            except subprocess.TimeoutExpired:
                redis.kill()
                redis.wait()
        if log:
            log.close()
        trace.close()
        report['replies_sha256'] = sha256(root / 'replies.jsonl')
        (root / 'report.json').write_text(json.dumps(report, indent=2) + '\n')
    summary = {key: value for key, value in report.items() if key not in ('mismatches', 'documented_differences')}
    summary['mismatch_count'] = len(report['mismatches'])
    summary['first_mismatches'] = report['mismatches'][:20]
    summary['documented_difference_count'] = len(report['documented_differences'])
    print(json.dumps(summary, indent=2))
    if report['mismatches']:
        raise SystemExit(1)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--bin', type=Path, required=True, help='Keel binary')
    parser.add_argument('--redis', type=Path, required=True, help='redis-server, 8.10.1')
    parser.add_argument('--redis-module', type=Path, required=True, help="that release's redisbloom.so")
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--seed', type=int, default=20261003)
    parser.add_argument('--steps', type=int, default=4000, help='random commands per protocol')
    args = parser.parse_args()
    if not 0 <= args.steps <= 1000000:
        parser.error('steps must be 0..1000000')
    run(args)
