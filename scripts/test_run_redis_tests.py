import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('run_redis_tests', Path(__file__).with_name('run-redis-tests.py'))
rt = importlib.util.module_from_spec(spec)
spec.loader.exec_module(rt)

# What the harness prints for a unit, with its colours, as test_helper.tcl
# prints it: a passing test, a failing one with its assertion, an ignored
# one, and the end of the unit.
HARNESS = (
    'Cleanup: may take some time... OK\n'
    'Starting test server at port 21079\n'
    '[ready]: 12345\n'
    'Testing unit/type/string\n'
    '[\x1b[0;32mok\x1b[0m]: SET and GET an item (1 ms)\n'
    '[\x1b[0;32mok\x1b[0m]: SET and GET an empty item (0 ms)\n'
    '[\x1b[0;31merr\x1b[0m]: SETNX against expired volatile key in tests/unit/type/string.tcl\n'
    "Expected '0' to be equal to '1' (context: type eval line 7 cmd {assert_equal 0 [r setnx x 20]} proc ::test)\n"
    '[\x1b[0;36mignore\x1b[0m]: LCS with expiry: Tag: external:skip denied\n'
    '[\x1b[0;31merr\x1b[0m]: GETEX EX option in tests/unit/type/string.tcl\n'
    "ERR unknown command 'getex', with args beginning with: 'foo' 'EX' '10' \n"
    '    while executing\n'
    '"r getex foo EX 10"\n'
    '[1/1 \x1b[0;33mdone\x1b[0m]: unit/type/string (3 seconds)\n'
    '\n'
    '                   The End\n'
    '\n'
    'Execution time of different units:\n'
    '  3 seconds - unit/type/string\n'
    '\n'
    '\x1b[1;31m!!! WARNING\x1b[0m The following tests failed:\n'
    '\n'
    '*** [err]: SETNX against expired volatile key in tests/unit/type/string.tcl\n'
    "Expected '0' to be equal to '1' (context: ...)\n"
    '*** [err]: GETEX EX option in tests/unit/type/string.tcl\n'
    "ERR unknown command 'getex'\n"
)

EXCEPTION = (
    '[\x1b[0;32mok\x1b[0m]: PING (0 ms)\n'
    '[\x1b[0;31mexception\x1b[0m]: Executing test client: I/O error reading reply.\n'
    'I/O error reading reply\n'
    '    while executing\n'
    '"$r read"\n'
)


class Parsing(unittest.TestCase):
    def test_counts_and_details(self):
        parsed = rt.parse(HARNESS)
        self.assertEqual([p['name'] for p in parsed['passed']], ['SET and GET an item', 'SET and GET an empty item'])
        self.assertEqual([f['name'] for f in parsed['failed']], ['SETNX against expired volatile key', 'GETEX EX option'])
        self.assertEqual(parsed['failed'][0]['file'], 'tests/unit/type/string.tcl')
        self.assertEqual(parsed['failed'][0]['detail'],
                         ["Expected '0' to be equal to '1' (context: type eval line 7 cmd {assert_equal 0 [r setnx x 20]} proc ::test)"])
        self.assertEqual(parsed['failed'][1]['detail'][0], "ERR unknown command 'getex', with args beginning with: 'foo' 'EX' '10'")
        self.assertEqual(parsed['ignored'], ['LCS with expiry: Tag: external:skip denied'])
        self.assertEqual((parsed['exception'], parsed['harness_timeout'], parsed['done']), ('', False, True))

    def test_exception_and_timeout(self):
        parsed = rt.parse(EXCEPTION)
        self.assertEqual(len(parsed['passed']), 1)
        self.assertTrue(parsed['exception'].startswith('Executing test client: I/O error reading reply.\nI/O error'))
        self.assertFalse(parsed['done'])
        parsed = rt.parse('[ok]: a (1 ms)\n[\x1b[0;31mTIMEOUT\x1b[0m]: clients state report follows.\n')
        self.assertTrue(parsed['harness_timeout'])
        unconfigurable = rt.parse("[\x1b[0;31mexception\x1b[0m]: Executing test client: ERR CONFIG SET failed "
                                  "(possibly related to argument 'appendonly') - can't set immutable config.\n")
        self.assertTrue(rt.CONFIG_SET.search(unconfigurable['exception']))

    def test_declared_tests(self):
        with tempfile.TemporaryDirectory() as tmp:
            unit = Path(tmp) / 'tests/unit/type'
            unit.mkdir(parents=True)
            (unit / 'string.tcl').write_text('start_server {tags {"string"}} {\n    test {SET and GET an item} {\n'
                                             '        r set x foobar\n    } {foobar}\n    test "GETEX EX" {\n    }\n'
                                             '    if {$::verbose} { test {x} {} }\n}\n')
            self.assertEqual(rt.count_tests(tmp, 'unit/type/string'), 2, 'tests at the start of a line')
            self.assertEqual(rt.count_tests(tmp, 'unit/nosuch'), 0)


SERVER_TCL = '''proc run_external_server_test {code overrides} {
    set srv {}
    }

    r flushall
    r function flush
    r script flush
    r config resetstat

    # Resolve client dynamically
}
'''


class Adaptation(unittest.TestCase):
    def test_setup_is_adapted_once_and_only_when_exact(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'tests/support/server.tcl'
            path.parent.mkdir(parents=True)
            path.write_text(SERVER_TCL)
            self.assertTrue(rt.adapt_harness(tmp).startswith('adapted'))
            text = path.read_text()
            self.assertIn('    if {[catch {r flushall}]} { r flushdb }\n    catch {r function flush}\n', text)
            self.assertNotIn('    r flushall\n', text)
            self.assertIn('# Resolve client dynamically', text, 'nothing else changes')
            self.assertEqual(rt.adapt_harness(tmp), 'already adapted')
            path.write_text(SERVER_TCL.replace('    r script flush\n', ''))
            with self.assertRaises(SystemExit):
                rt.adapt_harness(tmp)


class Groups(unittest.TestCase):
    def test_every_unit_once(self):
        units = [u for group in rt.GROUPS.values() for u in group]
        self.assertEqual(len(units), len(set(units)))
        self.assertFalse(set(units) & set(rt.SKIPPED))
        self.assertIn('unit/type/string', units)
        self.assertIn('unit/shutdown', rt.SKIPPED)


def unit_result(unit, server, passed, failed, outcome='ok', not_run=0, seconds=3.5):
    return {'unit': unit, 'server': server, 'outcome': outcome, 'seconds': seconds, 'declared': passed + failed + not_run,
            'passed': [{'name': f'p{i}', 'ms': 1} for i in range(passed)],
            'failed': [{'name': f'f{i}', 'file': 'tests/x.tcl', 'detail': ['Expected 1 to equal 0']} for i in range(failed)],
            'ignored': [], 'exception': '', 'harness_timeout': False, 'done': outcome == 'ok', 'not_run': not_run}


class Report(unittest.TestCase):
    def test_joins_both_servers(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for server, group, units in [
                ('keel', 1, [unit_result('unit/type/string', 'keel', 100, 40, not_run=4),
                             unit_result('unit/scan', 'keel', 0, 0, outcome='crash')]),
                ('redis', 1, [unit_result('unit/type/string', 'redis', 144, 0), unit_result('unit/scan', 'redis', 10, 0)]),
                ('keel', 2, [unit_result('unit/expire', 'keel', 30, 20, outcome='timeout')]),
            ]:
                directory = root / 'groups' / f'redis-tests-{server}-{group}' / server / f'group-{group}'
                directory.mkdir(parents=True)
                (directory / 'group.json').write_text(json.dumps({'server': server, 'group': group, 'units': units}))
            rt.main(['report', '--out', str(root / 'report'), '--results', str(root / 'groups')])
            result = json.loads((root / 'report/redis-tests.json').read_text())
            self.assertEqual(result['totals']['keel'], {'units': 3, 'passed': 130, 'failed': 60, 'ignored': 0,
                                                        'not_run': 4, 'units_crashed': 1, 'units_timed_out': 1,
                                                        'units_exception': 0, 'units_unconfigurable': 0})
            self.assertEqual(result['totals']['redis']['passed'], 154)
            self.assertEqual([u['unit'] for u in result['units']], ['unit/expire', 'unit/scan', 'unit/type/string'])
            self.assertIsNone(result['units'][0]['redis'], 'a unit one side has no result for')
            self.assertEqual(result['units'][2]['keel']['failing'][:2], ['f0', 'f1'])
            self.assertEqual(result['units'][2]['keel']['details']['f0'], 'Expected 1 to equal 0')
            csv_text = (root / 'report/redis-tests.csv').read_text().splitlines()
            self.assertEqual(csv_text[0].split(',')[:3], ['unit', 'redis_passed', 'redis_failed'])
            self.assertTrue(csv_text[3].startswith('unit/type/string,144,0,0,ok,100,40,0,4,ok,3.5,'))
            self.assertEqual(result['harness_adaptation'], ['unrecorded'])
            text = (root / 'report/redis-tests.md').read_text()
            self.assertIn('Keel passes **130** of the **154** Redis passes in external mode (84.4%)', text)
            self.assertIn('| unit/scan | 10 | 0 | 0 | 0 | 0 | crash | 3.5 |', text)
            self.assertIn('unit/shutdown (stops and restarts the server)', text)
            # Three of the fourteen groups reported, none of them marked
            # complete: the run is incomplete, and says which groups.
            self.assertFalse(result['complete'])
            self.assertEqual(len(result['incomplete_groups']), 2 * len(rt.GROUPS))
            self.assertIn('keel group 1', result['incomplete_groups'])
            self.assertIn('**Incomplete run:**', text)

    def write_groups(self, root, skip=(), unfinished=()):
        for server in ('keel', 'redis'):
            for group in rt.GROUPS:
                if (server, group) in skip:
                    continue
                directory = root / 'groups' / f'redis-tests-{server}-{group}' / server / f'group-{group}'
                directory.mkdir(parents=True)
                units = [unit_result(f'unit/g{group}', server, 5, 0)]
                (directory / 'group.json').write_text(json.dumps(
                    {'server': server, 'group': group, 'units': units, 'complete': (server, group) not in unfinished,
                     'harness_adaptation': 'adapted'}))

    def test_complete_only_when_every_group_reported_and_finished(self):
        cases = [((), (), True, []),
                 ((('redis', 4),), (), False, ['redis group 4']),
                 ((), (('keel', 6),), False, ['keel group 6'])]
        for skip, unfinished, complete, missing in cases:
            with tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                self.write_groups(root, skip, unfinished)
                rt.main(['report', '--out', str(root / 'report'), '--results', str(root / 'groups')])
                result = json.loads((root / 'report/redis-tests.json').read_text())
                self.assertEqual((result['complete'], result['incomplete_groups']), (complete, missing), (skip, unfinished))
                text = (root / 'report/redis-tests.md').read_text()
                self.assertEqual('**Incomplete run:**' in text, not complete)


if __name__ == '__main__':
    unittest.main()
