import importlib.util
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))
import redis_commands as rc  # noqa: E402

spec = importlib.util.spec_from_file_location('reply_census', Path(__file__).with_name('reply-census.py'))
rcen = importlib.util.module_from_spec(spec)
spec.loader.exec_module(rcen)

OK, NIL, ONE, DOUBLE = ('+', 'OK'), ('$', None), (':', '1'), (',', '1.5')
ARRAY = ('*', [('$', 'a'), (':', '1')])
MAP = ('%', [(('$', 'k'), (':', '1'))])


class Shapes(unittest.TestCase):
    def test_shape_keeps_types_and_nesting(self):
        self.assertEqual(rcen.shape(OK), '+')
        self.assertEqual(rcen.shape(NIL), '$-1')
        self.assertEqual(rcen.shape(('_', None)), '_')
        self.assertEqual(rcen.shape(('*', None)), '*-1')
        self.assertEqual(rcen.shape(('*', [ARRAY, DOUBLE])), '*[*[$ :] ,]')
        self.assertEqual(rcen.shape(MAP), '%{$::}')
        self.assertEqual(rcen.shape(('=', 'txt:hello')), '=txt')

    def test_compatible_ignores_lengths_and_empty_aggregates(self):
        self.assertTrue(rcen.compatible(('*', [('$', 'a')]), ('*', [('$', 'b'), ('$', 'c')])))
        self.assertTrue(rcen.compatible(('*', []), ('*', [ARRAY])), 'Keel lists no modules')
        self.assertFalse(rcen.compatible(('*', [ONE]), ('*', [('$', 'a')])))
        self.assertFalse(rcen.compatible(('*', []), ('%', [])))
        self.assertTrue(rcen.compatible(MAP, ('%', [(('$', 'k'), (':', '2')), (('$', 'z'), (',', '1'))])))
        self.assertFalse(rcen.compatible(('%', [(('$', 'k'), (',', '2'))]), MAP))

    def test_compare(self):
        self.assertEqual(rcen.compare('get', NIL, NIL), ('same', ''))
        self.assertEqual(rcen.compare('get', ('_', None), NIL), ('differs', ''))
        self.assertEqual(rcen.compare('zadd', ('-', 'ERR syntax error'), ONE), ('refused', 'ERR syntax error'))
        self.assertEqual(rcen.compare('get', None, NIL), ('no reply', ''))
        self.assertEqual(rcen.compare('hello', ('*', [('$', 'keel')]), ('*', [('$', 'redis'), ('$', 'x')])),
                         ('same', 'by the kinds of thing it holds'))
        self.assertEqual(rcen.compare('memory|stats', ('%', [(('$', 'a'), (':', '1'))]),
                                      ('%', [(('$', 'a'), (',', '1'))])), ('differs', ''))


class Schema(unittest.TestCase):
    def test_json_value(self):
        self.assertEqual(rcen.to_json(('*', [(':', '1'), (',', '1.5'), ('_', None), ('#', True), ('$', 'a')])),
                         [1, 1.5, None, True, 'a'])
        self.assertEqual(rcen.to_json(('%', [(('$', 'k'), (':', '1'))])), {'k': 1})
        self.assertEqual(rcen.to_json(('=', 'txt:hello')), 'hello')
        self.assertEqual(rcen.to_json(('~', [('$', 'a')])), ['a'])

    def test_types_and_constants(self):
        self.assertEqual(rcen.check('OK', {'const': 'OK'}), [])
        self.assertEqual(rcen.check(None, {'type': 'null'}), [])
        self.assertEqual(rcen.check(1, {'type': 'number'}), [])
        self.assertEqual(rcen.check(True, {'type': 'integer'}), ['/: a boolean is not integer'])
        self.assertEqual(rcen.check(1.5, {'type': 'integer'}), ['/: a number is not integer'])
        self.assertEqual(rcen.check(-1, {'type': 'integer', 'minimum': 0}), ['/: -1 is below 0'])
        self.assertEqual(rcen.check('x', {'type': ['string', 'null']}), [])
        self.assertEqual(rcen.check('ab', {'type': 'string', 'pattern': '^[0-9]+$'}),
                         ["/: 'ab' does not match '^[0-9]+$'"])

    def test_arrays(self):
        pairs = {'type': 'array', 'items': {'type': 'array', 'minItems': 2, 'maxItems': 2,
                                            'items': [{'type': 'string'}, {'type': 'number'}]}}
        self.assertEqual(rcen.check([['a', 1.5], ['b', 2]], pairs), [])
        self.assertEqual(rcen.check([['a']], pairs), ['/0: 1 items, fewer than 2'])
        self.assertEqual(rcen.check([[1, 'a']], pairs), ['/0/0: an integer is not string', '/0/1: a string is not number'])
        self.assertEqual(rcen.check(['a', 'a'], {'type': 'array', 'uniqueItems': True}), ['/: items are not unique'])
        self.assertEqual(rcen.check(['a', 'b'], {'type': 'array', 'items': [{'type': 'string'}], 'additionalItems': False}),
                         ['/1: more items than the schema lists'])

    def test_objects(self):
        stats = {'type': 'object', 'additionalProperties': False,
                 'properties': {'peak.allocated': {'type': 'integer'}},
                 'patternProperties': {'^db\\.\\d+$': {'type': 'object'}}}
        self.assertEqual(rcen.check({'peak.allocated': 1, 'db.0': {}}, stats), [])
        self.assertEqual(rcen.check({'keyspace.bytes': 1}, stats), ['/keyspace.bytes: not a listed field'])
        self.assertEqual(rcen.check({'peak.allocated': 'x'}, stats), ['/peak.allocated: a string is not integer'])
        self.assertEqual(rcen.check({'f': 1}, {'type': 'object', 'additionalProperties': {'type': 'string'}}),
                         ['/f: an integer is not string'])
        self.assertEqual(rcen.check({}, {'type': 'object', 'required': ['id']}), ["/: 'id' is missing"])

    def test_alternatives(self):
        zadd = {'anyOf': [{'type': 'null'}, {'type': 'integer'}, {'type': 'number'}]}
        self.assertEqual(rcen.check(None, zadd), [])
        self.assertEqual(rcen.check('x', zadd), ['/: a string matches none of the alternatives'])
        one = {'oneOf': [{'const': 'OK'}, {'type': 'null'}]}
        self.assertEqual(rcen.check('OK', one), [])
        self.assertEqual(rcen.check(1, one), ['/: an integer matches 0 alternatives, not one'])
        self.assertEqual(rcen.check(1, {'oneOf': [{'type': 'integer'}, {'type': 'number'}]}),
                         ['/: an integer matches 2 alternatives, not one'])
        self.assertEqual(rcen.check(1, {'not': {'type': 'integer'}}), ['/: matches what the schema excludes'])
        self.assertEqual(rcen.check(1, {'allOf': [{'type': 'integer'}, {'minimum': 5}]}), ['/: 1 is below 5'])


class Cases(unittest.TestCase):
    def test_base_form_with_missing_wrong_type_and_extra(self):
        spec = {'name': 'lrange', 'group': 'list', 'since': '1.0.0', 'is_container': False,
                'key_specs': [{'flags': ['RO', 'ACCESS']}],
                'arguments': [{'name': 'key', 'type': 'key', 'key_spec_index': 0},
                              {'name': 'start', 'type': 'integer'}, {'name': 'stop', 'type': 'integer'}]}
        cases = rcen.cases_of(spec, 'pw')
        self.assertEqual([(c['case'], ' '.join(c['argv'])) for c in cases],
                         [('form', 'LRANGE list 0 -1'), ('missing key', 'LRANGE nokey 0 -1'),
                          ('wrong type', 'LRANGE str 0 -1'), ('extra argument', 'LRANGE list 0 -1 BOGUS')])

    def test_string_commands_get_a_list_for_the_wrong_type(self):
        spec = {'name': 'get', 'group': 'string', 'since': '1.0.0', 'is_container': False,
                'key_specs': [{'flags': ['RO', 'ACCESS']}],
                'arguments': [{'name': 'key', 'type': 'key', 'key_spec_index': 0}]}
        self.assertEqual(' '.join(rcen.cases_of(spec, 'pw')[2]['argv']), 'GET list')

    def test_no_keys_no_key_cases(self):
        spec = {'name': 'ping', 'group': 'connection', 'since': '1.0.0', 'is_container': False, 'key_specs': [],
                'arguments': [{'name': 'message', 'type': 'string', 'optional': True}]}
        self.assertEqual([c['case'] for c in rcen.cases_of(spec, 'pw')], ['form', 'form', 'extra argument'])


C_FILE = '''#include "server.h"

static void helper(client *c, int n) {
    if (n) {
        addReplyLongLong(c, n);
        return;
    }
    addReplyNull(c);
}

void fooCommand(client *c) {
    helper(c, 1);
}

void barCommand(client *c) {
    addReply(c, shared.ok);
}
'''


class SourceIndex(unittest.TestCase):
    def test_functions_and_reply_sites(self):
        with tempfile.TemporaryDirectory() as tmp:
            (Path(tmp) / 't_foo.c').write_text(C_FILE)
            source = rcen.Source(tmp)
            self.assertEqual(source.functions['fooCommand'], ('t_foo.c', 11, 13))
            self.assertEqual(source.functions['helper'], ('t_foo.c', 3, 9))
            where = source.where({'function': 'fooCommand'})
            self.assertEqual(where['at'], 't_foo.c:11')
            self.assertEqual(where['sites'], ['t_foo.c:5: addReplyLongLong(c, n);', 't_foo.c:8: addReplyNull(c);'])
            self.assertEqual(source.where({'function': 'barCommand'})['sites'], ['t_foo.c:16: addReply(c, shared.ok);'])
            self.assertEqual(source.where({'function': 'nosuch'}), {'function': 'nosuch', 'at': '', 'sites': []})


class Summary(unittest.TestCase):
    def test_counts_and_markdown(self):
        rows = [
            {'command': 'get', 'area': 'string', 'case': 'form', 'label': '', 'since': '1.0.0', 'argv': ['GET', 'k'],
             'protocol': 2, 'result': 'same', 'detail': '', 'keel': '$abc', 'redis': '$abc', 'keel_shape': '$',
             'redis_shape': '$', 'keel_schema': '', 'redis_schema': ''},
            {'command': 'get', 'area': 'string', 'case': 'form', 'label': '', 'since': '1.0.0', 'argv': ['GET', 'k'],
             'protocol': 3, 'result': 'differs', 'detail': '', 'keel': '$-1', 'redis': '_', 'keel_shape': '$-1',
             'redis_shape': '_', 'keel_schema': 'violation: /: a string is not null', 'redis_schema': 'ok'},
            {'command': 'zadd', 'area': 'sorted_set', 'case': 'form', 'label': 'GT', 'since': '6.2.0',
             'argv': ['ZADD', 'z', 'GT', '1', 'a'], 'protocol': 3, 'result': 'refused', 'detail': 'ERR syntax error',
             'keel': '-ERR syntax error', 'redis': ':0', 'keel_shape': '-', 'redis_shape': ':', 'keel_schema': 'error',
             'redis_schema': 'ok'},
        ]
        summary = rcen.summarise(rows)
        self.assertEqual(summary['total'], {'commands': 2, 'cases': 3, 'same': 1, 'differs': 1, 'refused': 1,
                                            'no_reply': 0, 'resp3_cases': 2, 'resp3_same': 0,
                                            'keel_schema_violations': 1, 'redis_schema_violations': 0})
        commands = {'get': {'area': 'string', 'function': 'getCommand', 'at': 't_string.c:1',
                            'sites': ['t_string.c:5: addReplyNull(c);'], 'schema': True}}
        text = rcen.markdown(summary, rows, commands, '8.10.1')
        self.assertIn("shape in 1 of 3 cases**", text)
        self.assertIn('| string | 1 | 2 | 1 | 1 | 0 | 1 | `get` |', text)
        self.assertIn('| `get` |  | 3 | `GET k` | `$-1` | `_` | t_string.c:1 (t_string.c:5) |', text)
        self.assertIn('- `GET k`: `$-1`: /: a string is not null', text)
        self.assertNotIn('ZADD z GT', text.split('| Command |')[1].split('Keel replies')[0])


if __name__ == '__main__':
    unittest.main()
