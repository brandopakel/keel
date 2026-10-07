import importlib.util
import io
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('command_census', Path(__file__).with_name('command-census.py'))
cc = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cc)


def resp(data):
    return cc.read(io.BytesIO(data))


# COMMAND DOCS as RESP2 sends it, trimmed: a plain command, a deprecated one,
# a container with two subcommands, and a module's command.
DOCS = [
    'get', ['summary', 'Returns the string value of a key.', 'since', '1.0.0', 'group', 'string'],
    'getset', ['summary', '...', 'since', '1.0.0', 'group', 'string', 'doc_flags', ['deprecated']],
    'config', ['summary', 'A container.', 'since', '2.0.0', 'group', 'server', 'subcommands', [
        'config|set', ['since', '2.0.0', 'group', 'server'],
        'config|get', ['since', '2.0.0', 'group', 'server']]],
    'shutdown', ['since', '1.0.0', 'group', 'server'],
    'bf.add', ['since', '1.0.0', 'group', 'module', 'module', 'bf'],
]


class Protocol(unittest.TestCase):
    def test_reply_types(self):
        self.assertEqual(resp(b'+OK\r\n'), 'OK')
        self.assertEqual(resp(b':7\r\n'), '7')
        self.assertEqual(resp(b'$5\r\nhello\r\n'), 'hello')
        self.assertIsNone(resp(b'$-1\r\n'))
        self.assertIsNone(resp(b'_\r\n'))
        self.assertEqual(resp(b'*2\r\n+a\r\n:1\r\n'), ['a', '1'])
        # A RESP3 map reads as RESP2's alternating list.
        self.assertEqual(resp(b'%1\r\n+group\r\n+string\r\n'), ['group', 'string'])
        error = resp(b"-ERR unknown command 'xadd', with args beginning with: \r\n")
        self.assertIsInstance(error, cc.Error)
        self.assertIsInstance(resp(b'!9\r\nERR boom!\r\n'), cc.Error)

    def test_closed_connection(self):
        with self.assertRaises(ConnectionError):
            resp(b'+OK')

    def test_encode(self):
        self.assertEqual(cc.encode(['CONFIG', 'GET']), b'*2\r\n$6\r\nCONFIG\r\n$3\r\nGET\r\n')


class Docs(unittest.TestCase):
    def test_parse(self):
        commands = cc.parse_docs(DOCS)
        self.assertEqual([c['name'] for c in commands], ['bf.add', 'config', 'get', 'getset', 'shutdown'])
        by_name = {c['name']: c for c in commands}
        self.assertTrue(by_name['getset']['deprecated'])
        self.assertEqual(by_name['bf.add']['module'], 'bf')
        self.assertEqual([s['name'] for s in by_name['config']['subcommands']], ['config|get', 'config|set'])

    def test_probes(self):
        self.assertEqual(cc.probe_parts('config|get'), ['CONFIG', 'GET'])
        self.assertEqual(cc.probe_parts('get'), ['GET'])
        self.assertEqual(cc.probe_parts('shutdown'), ['SHUTDOWN', 'ABORT'])

    def test_classify(self):
        # Keel's refusals, as Redis words them (docs/error-replies.md).
        self.assertEqual(cc.classify(cc.Error("ERR unknown command 'xadd', with args beginning with: ")), 'missing')
        self.assertEqual(cc.classify(cc.Error("ERR unknown subcommand 'get'. Try CONFIG HELP.")), 'missing')
        self.assertEqual(cc.classify(cc.Error("ERR wrong number of arguments for 'get' command")), 'present')
        self.assertEqual(cc.classify(cc.Error('NOAUTH Authentication required.')), 'present')
        self.assertEqual(cc.classify('OK'), 'present')


class Summary(unittest.TestCase):
    ROWS = [
        {'name': 'get', 'kind': 'command', 'group': 'string', 'module': '', 'keel': 'present'},
        {'name': 'append', 'kind': 'command', 'group': 'string', 'module': '', 'keel': 'missing'},
        {'name': 'config', 'kind': 'command', 'group': 'server', 'module': '', 'keel': 'missing'},
        {'name': 'config|get', 'kind': 'subcommand', 'group': 'server', 'module': '', 'keel': 'missing'},
        {'name': 'bf.add', 'kind': 'command', 'group': 'module', 'module': 'bf', 'keel': 'present'},
    ]

    def test_counts_by_area(self):
        summary = cc.summarise(self.ROWS)
        self.assertEqual(summary['areas']['string'], {'commands': 2, 'commands_present': 1,
                                                      'subcommands': 0, 'subcommands_present': 0})
        self.assertEqual(summary['areas']['module:bf']['commands_present'], 1)
        self.assertEqual(summary['total'], {'commands': 4, 'commands_present': 2,
                                            'subcommands': 1, 'subcommands_present': 0})
        text = cc.markdown(summary, self.ROWS, '8.10.1')
        self.assertIn("Keel has **2 of Redis 8.10.1's 4 commands**", text)
        self.assertIn('| string | 1/2 | - | `append` |', text)
        self.assertIn('| server | 0/1 | 0/1 | `config` |', text)


if __name__ == '__main__':
    unittest.main()
