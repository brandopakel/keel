import importlib.util
import io
from pathlib import Path
import sys
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))
import redis_commands as rc  # noqa: E402

spec = importlib.util.spec_from_file_location('argument_census', Path(__file__).with_name('argument-census.py'))
ac = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ac)


def spec_of(name, arguments, group='string', key_specs=None, since='1.0.0'):
    return {'name': name, 'group': group, 'since': since, 'arity': -1, 'arguments': arguments,
            'key_specs': key_specs if key_specs is not None else [{'flags': ['RW', 'ACCESS']}],
            'is_container': False}


def forms(spec):
    return [(f['label'], f['since'], ' '.join(f['argv'])) for f in rc.forms_of(spec, 'pw')]


# SET as src/commands/set.json documents it, trimmed to what the forms need.
SET = [
    {'name': 'key', 'type': 'key', 'key_spec_index': 0},
    {'name': 'value', 'type': 'string'},
    {'name': 'condition', 'type': 'oneof', 'optional': True, 'since': '2.6.12', 'arguments': [
        {'name': 'nx', 'type': 'pure-token', 'token': 'NX'},
        {'name': 'xx', 'type': 'pure-token', 'token': 'XX'},
        {'name': 'ifeq-value', 'type': 'string', 'token': 'IFEQ', 'since': '8.4.0'}]},
    {'name': 'get', 'token': 'GET', 'type': 'pure-token', 'optional': True, 'since': '6.2.0'},
    {'name': 'expiration', 'type': 'oneof', 'optional': True, 'arguments': [
        {'name': 'seconds', 'type': 'integer', 'token': 'EX', 'since': '2.6.12'},
        {'name': 'keepttl', 'type': 'pure-token', 'token': 'KEEPTTL', 'since': '6.0.0'}]},
]


class Forms(unittest.TestCase):
    def test_base_then_one_option_at_a_time(self):
        self.assertEqual(forms(spec_of('set', SET)), [
            ('', '1.0.0', 'SET str v'),
            ('NX', '2.6.12', 'SET str v NX'),
            ('XX', '2.6.12', 'SET str v XX'),
            ('IFEQ', '8.4.0', 'SET str v IFEQ s'),
            ('GET', '6.2.0', 'SET str v GET'),
            ('EX', '2.6.12', 'SET str v EX 100'),
            ('KEEPTTL', '6.0.0', 'SET str v KEEPTTL'),
        ])

    def test_repeated_block_twice_with_destination_keys(self):
        mset = [{'name': 'data', 'type': 'block', 'multiple': True, 'arguments': [
            {'name': 'key', 'type': 'key', 'key_spec_index': 0}, {'name': 'value', 'type': 'string'}]}]
        self.assertEqual(forms(spec_of('mset', mset, key_specs=[{'flags': ['OW', 'UPDATE']}])),
                         [('', '1.0.0', 'MSET dest v'), ('data*2', '1.0.0', 'MSET dest v dest2 w')])

    def test_keys_of_a_type_are_numbered_across_the_form(self):
        lcs = [{'name': 'key1', 'type': 'key', 'key_spec_index': 0}, {'name': 'key2', 'type': 'key', 'key_spec_index': 0}]
        self.assertEqual(forms(spec_of('lcs', lcs))[0], ('', '1.0.0', 'LCS str str2'))
        smove = [{'name': 'source', 'type': 'key', 'key_spec_index': 0},
                 {'name': 'destination', 'type': 'key', 'key_spec_index': 1}, {'name': 'member', 'type': 'string'}]
        self.assertEqual(forms(spec_of('smove', smove, 'set', [{'flags': ['RW', 'ACCESS', 'DELETE']},
                                                               {'flags': ['RW', 'INSERT']}]))[0],
                         ('', '1.0.0', 'SMOVE set set2 a'))

    def test_counts_follow_their_repeated_argument(self):
        hexpire = [{'name': 'key', 'type': 'key', 'key_spec_index': 0}, {'name': 'seconds', 'type': 'integer'},
                   {'name': 'fields', 'token': 'FIELDS', 'type': 'block', 'arguments': [
                       {'name': 'numfields', 'type': 'integer'}, {'name': 'field', 'type': 'string', 'multiple': True}]}]
        self.assertEqual(forms(spec_of('hexpire', hexpire, 'hash')), [
            ('', '1.0.0', 'HEXPIRE hash 100 FIELDS 1 f'), ('field*2', '1.0.0', 'HEXPIRE hash 100 FIELDS 2 f n')])
        eval_ = [{'name': 'script', 'type': 'string'}, {'name': 'numkeys', 'type': 'integer'},
                 {'name': 'key', 'type': 'key', 'key_spec_index': 0, 'optional': True, 'multiple': True},
                 {'name': 'arg', 'type': 'string', 'optional': True, 'multiple': True}]
        self.assertEqual([f[2] for f in forms(spec_of('eval', eval_, 'scripting'))],
                         ['EVAL return 1 0', 'EVAL return 1 1 str', 'EVAL return 1 2 str str2',
                          'EVAL return 1 0 x', 'EVAL return 1 0 x y'])

    def test_token_once_before_repetitions_or_before_each(self):
        weights = [{'name': 'numkeys', 'type': 'integer'},
                   {'name': 'key', 'type': 'key', 'key_spec_index': 0, 'multiple': True},
                   {'token': 'WEIGHTS', 'name': 'weight', 'type': 'integer', 'optional': True, 'multiple': True}]
        self.assertEqual([f[2] for f in forms(spec_of('zunion', weights, 'sorted_set'))],
                         ['ZUNION 1 zset', 'ZUNION 2 zset zset2', 'ZUNION 1 zset WEIGHTS 1',
                          'ZUNION 2 zset zset2 WEIGHTS 1 2'])
        get = [{'name': 'key', 'type': 'key', 'key_spec_index': 0},
               {'token': 'GET', 'name': 'get-pattern', 'type': 'pattern', 'optional': True, 'multiple': True,
                'multiple_token': True}]
        self.assertEqual([f[2] for f in forms(spec_of('sort', get, 'generic'))],
                         ['SORT list', 'SORT list GET #', 'SORT list GET # GET #'])

    def test_nested_options_are_forms_of_their_own(self):
        count = [{'name': 'key', 'type': 'key', 'key_spec_index': 0},
                 {'name': 'count-block', 'type': 'block', 'optional': True, 'arguments': [
                     {'token': 'COUNT', 'name': 'count', 'type': 'integer'},
                     {'name': 'any', 'token': 'ANY', 'type': 'pure-token', 'optional': True}]}]
        self.assertEqual(forms(spec_of('geosearch', count, 'geo'))[1:],
                         [('COUNT', '1.0.0', 'GEOSEARCH geo COUNT 1'), ('COUNT ANY', '1.0.0', 'GEOSEARCH geo COUNT 1 ANY')])

    def test_zrange_bounds_follow_its_options(self):
        zrange = [{'name': 'key', 'type': 'key', 'key_spec_index': 0}, {'name': 'start', 'type': 'string'},
                  {'name': 'stop', 'type': 'string'},
                  {'name': 'sortby', 'type': 'oneof', 'optional': True, 'arguments': [
                      {'name': 'byscore', 'type': 'pure-token', 'token': 'BYSCORE'},
                      {'name': 'bylex', 'type': 'pure-token', 'token': 'BYLEX'}]},
                  {'name': 'rev', 'token': 'REV', 'type': 'pure-token', 'optional': True},
                  {'token': 'LIMIT', 'name': 'limit', 'type': 'block', 'optional': True, 'arguments': [
                      {'name': 'offset', 'type': 'integer'}, {'name': 'count', 'type': 'integer'}]}]
        self.assertEqual([f[2] for f in forms(spec_of('zrange', zrange, 'sorted_set'))],
                         ['ZRANGE zset 0 -1', 'ZRANGE zset -inf +inf BYSCORE', 'ZRANGE zset - + BYLEX',
                          'ZRANGE zset 0 -1 REV', 'ZRANGE zset -inf +inf BYSCORE LIMIT 0 1'])

    def test_password_and_versions(self):
        auth = [{'name': 'username', 'type': 'string', 'optional': True, 'since': '6.0.0'},
                {'name': 'password', 'type': 'string'}]
        self.assertEqual(forms(spec_of('auth', auth, 'connection')),
                         [('', '1.0.0', 'AUTH pw'), ('username', '6.0.0', 'AUTH default pw')])
        self.assertEqual(rc.version('8.10.1'), (8, 10, 1))
        self.assertEqual(rc.later('6.2.0', '8.4.0'), '8.4.0')
        self.assertEqual(rc.later('', '1.0.0'), '1.0.0')

    def test_fixture_uses_only_commands_both_servers_have(self):
        commands = rc.fixture_commands({'set', 'rpush', 'hset', 'sadd', 'zadd', 'geoadd', 'pfadd'})
        self.assertNotIn('XADD', {c[0] for c in commands})
        self.assertIn(['SET', 'str', 'abc'], commands)


class Replies(unittest.TestCase):
    def read(self, data):
        return rc.read_reply(io.BytesIO(data))

    def test_reads_both_protocols(self):
        self.assertEqual(self.read(b'+OK\r\n'), ('+', 'OK'))
        self.assertEqual(self.read(b'$-1\r\n'), ('$', None))
        self.assertEqual(self.read(b'*2\r\n:1\r\n$1\r\na\r\n'), ('*', [(':', '1'), ('$', 'a')]))
        self.assertEqual(self.read(b'%1\r\n+k\r\n,1.5\r\n'), ('%', [(('+', 'k'), (',', '1.5'))]))
        self.assertEqual(self.read(b'_\r\n'), ('_', None))
        self.assertEqual(self.read(b'#t\r\n'), ('#', True))
        with self.assertRaises(ConnectionError):
            self.read(b'+OK')

    def test_text(self):
        self.assertEqual(rc.text(('*', [(':', '1'), ('$', 'a'), ('$', None)])), '*[:1 $a $-1]')
        self.assertEqual(rc.text(('%', [(('+', 'k'), (',', '1.5'))])), '%{+k: ,1.5}')
        self.assertEqual(rc.text(('-', 'ERR x')), '-ERR x')
        self.assertEqual(rc.text(('$', 'a' * 200), 20), '$' + 'a' * 16 + '...')

    def test_classes(self):
        ok, nil, one = ('+', 'OK'), ('$', None), (':', '1')
        self.assertEqual(ac.classify('set', ok, ok), ('accepted', ''))
        self.assertEqual(ac.classify('set', ('-', 'ERR syntax error'), ok), ('refused', 'not parsed'))
        self.assertEqual(ac.classify('config|set', ('-', "ERR CONFIG SET failed (possibly related to argument 'x')"
                                                        " - can't set immutable config"), ok),
                         ('refused', 'parsed, then refused'))
        self.assertEqual(ac.classify('set', nil, ok), ('wrong reply', ''))
        self.assertEqual(ac.classify('set', ('-', 'ERR a'), ('-', 'ERR b')), ('wrong reply', 'a different error'))
        self.assertEqual(ac.classify('set', ok, ('-', 'ERR b')), ('control', 'Redis refused the form'))
        self.assertEqual(ac.classify('set', ('-', 'ERR b'), ('-', 'ERR b')),
                         ('control', 'Redis refused the form, and so did Keel'))
        self.assertEqual(ac.classify('config|rewrite', ('-', 'ERR no file'), ('-', 'ERR no file'))[0], 'accepted')
        self.assertEqual(ac.classify('set', None, ok), ('no reply', ''))
        # Server-specific contents: the same kind of reply is enough.
        self.assertEqual(ac.classify('client|id', one, (':', '7')), ('accepted', 'same kind of reply'))
        self.assertEqual(ac.classify('hello', ('*', []), ('%', [])), ('wrong reply', 'a different kind of reply'))
        # Unordered replies are sorted, as pairs for HGETALL.
        self.assertEqual(ac.classify('smembers', ('*', [('$', 'b'), ('$', 'a')]), ('*', [('$', 'a'), ('$', 'b')])),
                         ('accepted', ''))
        self.assertEqual(ac.classify('hgetall', ('*', [('$', 'n'), ('$', '5'), ('$', 'f'), ('$', 'v')]),
                                     ('*', [('$', 'f'), ('$', 'v'), ('$', 'n'), ('$', '5')])), ('accepted', ''))
        self.assertEqual(ac.classify('lrange', ('*', [('$', 'b'), ('$', 'a')]), ('*', [('$', 'a'), ('$', 'b')])),
                         ('wrong reply', ''))


class Summary(unittest.TestCase):
    ROWS = [
        {'command': 'set', 'area': 'string', 'label': '', 'since': '1.0.0', 'argv': ['SET', 'k', 'v'],
         'class': 'accepted', 'detail': '', 'keel': '+OK', 'redis': '+OK'},
        {'command': 'set', 'area': 'string', 'label': 'IFEQ', 'since': '8.4.0', 'argv': ['SET', 'k', 'v', 'IFEQ', 's'],
         'class': 'refused', 'detail': 'not parsed', 'keel': '-ERR syntax error', 'redis': '$-1'},
        {'command': 'zadd', 'area': 'sorted_set', 'label': 'GT', 'since': '6.2.0', 'argv': ['ZADD', 'z', 'GT', '1', 'a'],
         'class': 'wrong reply', 'detail': '', 'keel': ':1', 'redis': ':0'},
        {'command': 'shutdown', 'area': 'server', 'label': '', 'since': '1.0.0', 'argv': [],
         'class': 'not probed', 'detail': 'stops the server', 'keel': '', 'redis': ''},
    ]

    def test_counts_and_markdown(self):
        summary = ac.summarise(self.ROWS)
        self.assertEqual(summary['total'], {'commands': 2, 'forms': 3, 'accepted': 1, 'refused': 1, 'wrong_reply': 1,
                                            'no_reply': 0, 'control': 0, 'redis8_forms': 1, 'redis8_accepted': 0})
        self.assertEqual(sorted(summary['areas']), ['sorted_set', 'string'])
        text = ac.markdown(summary, self.ROWS, '8.10.1')
        self.assertIn('**1 of the 3 documented argument forms** of the 2 commands', text)
        self.assertIn('| string | 1 | 2 | 1 | 1 | 0 | `set`: IFEQ |', text)
        self.assertIn('| `zadd` | GT | 6.2.0 | `ZADD z GT 1 a` | wrong reply | `:1` | `:0` |', text)
        self.assertNotIn('shutdown', text.split('| Command |')[1])


if __name__ == '__main__':
    unittest.main()
