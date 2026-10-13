import importlib.util
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('capture_command_goldens', Path(__file__).with_name('capture-command-goldens.py'))
ccg = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ccg)

SOURCE = '''package core

const (
	commandGoldenRevision = "abc1234"
	commandGoldenDir      = "testdata/commands-" + commandGoldenRevision
)
'''

FIXTURE = '''case strings/set
> SET "k" "v"
2 "+OK\\r\\n"
3 "+OK\\r\\n"
~ "SET" "k" "v"
> GET "k"
2 "$1\\r\\nv\\r\\n"
3 "$1\\r\\nv\\r\\n"

case keys/expire
! sleep 5ms
~ "DEL" "k"
> TTL "k" @ttl-s
2 ":-2\\r\\n"
3 ":-2\\r\\n"
'''


class Constants(unittest.TestCase):
    def test_reads_a_constant_built_from_another(self):
        with tempfile.TemporaryDirectory() as tmp:
            source = Path(tmp) / 'x_test.go'
            source.write_text(SOURCE)
            self.assertEqual(ccg.read_constant(source, 'commandGoldenRevision'), 'abc1234')
            self.assertEqual(ccg.read_constant(source, 'commandGoldenDir'), 'testdata/commands-abc1234')
            with self.assertRaises(ValueError):
                ccg.read_constant(source, 'nosuch')

    def test_targets_name_the_checked_in_fixtures(self):
        targets = ccg.fixture_targets()
        self.assertEqual([t['captured'] for t in targets], ['core', 'server'])
        for target in targets:
            self.assertRegex(target['revision'], r'^[0-9a-f]{7}$')
            self.assertTrue(target['fixture'].name.endswith(target['revision']), target['fixture'])
            self.assertTrue((target['fixture'] / 'manifest.json').exists(), target['fixture'])


class Fixtures(unittest.TestCase):
    def test_summary_counts_cases_commands_and_records(self):
        with tempfile.TemporaryDirectory() as tmp:
            (Path(tmp) / 'a.txt').write_text(FIXTURE)
            (Path(tmp) / 'manifest.json').write_text('{}')
            self.assertEqual(ccg.summarize(Path(tmp)), dict(cases=2, commands=3, records=2))


class Revisions(unittest.TestCase):
    def test_resolves_a_revision(self):
        self.assertIsNone(ccg.resolve('0000000'))
        self.assertRegex(ccg.resolve('HEAD'), r'^[0-9a-f]{40}$')

    def test_changed_source_against_head_is_the_working_tree(self):
        for path in ccg.changed_source('HEAD'):
            self.assertNotRegex(path, ccg.HARMLESS)


class Harmless(unittest.TestCase):
    def test_what_may_differ_from_head(self):
        for path in ('internal/core/x_test.go', 'internal/core/testdata/commands-abc1234/strings.txt', 'docs/plan.md',
                     'scripts/capture-command-goldens.py', 'internal/golden/golden.go', 'README.md'):
            self.assertRegex(path, ccg.HARMLESS, path)
        for path in ('internal/core/eval.go', 'cmd/keel/main.go', 'go.mod', 'internal/server/server.go'):
            self.assertNotRegex(path, ccg.HARMLESS, path)


if __name__ == '__main__':
    unittest.main()
