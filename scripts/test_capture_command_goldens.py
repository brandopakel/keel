import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('capture_command_goldens', Path(__file__).with_name('capture-command-goldens.py'))
ccg = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ccg)

SOURCE = '''package core

const connectionGoldenDir = "testdata/connection"

const (
	commandGoldenFamily = "strings"
	commandGoldenDir    = "testdata/commands-" + commandGoldenFamily
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
            self.assertEqual(ccg.read_constant(source, 'commandGoldenFamily'), 'strings')
            self.assertEqual(ccg.read_constant(source, 'commandGoldenDir'), 'testdata/commands-strings')
            self.assertEqual(ccg.read_constant(source, 'connectionGoldenDir'), 'testdata/connection')
            with self.assertRaises(ValueError):
                ccg.read_constant(source, 'nosuch')

    def test_targets_name_the_checked_in_fixtures(self):
        targets = ccg.fixture_targets()
        self.assertEqual([t['captured'] for t in targets], ['core', 'server'])
        self.assertEqual([t['fixture'].relative_to(ccg.ROOT).as_posix() for t in targets],
                         ['internal/core/testdata/commands', 'internal/server/testdata/connection'])
        for target in targets:
            self.assertTrue((target['fixture'] / 'manifest.json').exists(), target['fixture'])
            # Every file has its own provenance; the directory's name has none.
            for name, entry in ccg.manifest_files(target['fixture']).items():
                self.assertTrue((target['fixture'] / name).exists(), name)
                self.assertRegex(entry['source_revision'], r'^[0-9a-f]{7,40}$', name)
                self.assertRegex(entry['captured'], r'^\d{4}-\d{2}-\d{2}$', name)


class Fixtures(unittest.TestCase):
    def test_summary_counts_cases_commands_and_records(self):
        with tempfile.TemporaryDirectory() as tmp:
            (Path(tmp) / 'a.txt').write_text(FIXTURE)
            (Path(tmp) / 'manifest.json').write_text('{}')
            self.assertEqual(ccg.summarize(Path(tmp)), dict(cases=2, commands=3, records=2))


class Manifests(unittest.TestCase):
    def test_a_fixture_without_a_manifest_holds_nothing(self):
        with tempfile.TemporaryDirectory() as tmp:
            self.assertEqual(ccg.manifest_files(Path(tmp)), {})
            entry = dict(sha256='aa', cases=1, commands=2, records=0, source_revision='abc1234', captured='2026-10-14')
            (Path(tmp) / 'manifest.json').write_text(json.dumps(dict(note='n', files={'a.txt': entry})))
            self.assertEqual(ccg.manifest_files(Path(tmp)), {'a.txt': entry})

    def test_compares_entries_by_digest(self):
        held = {'a.txt': dict(sha256='aa', source_revision='1111111'), 'b.txt': dict(sha256='bb'),
                'c.txt': dict(sha256='cc')}
        captured = {'a.txt': dict(sha256='aa', source_revision='1111111'), 'b.txt': dict(sha256='b2'),
                    'd.txt': dict(sha256='dd')}
        self.assertEqual(ccg.compare_entries(held, captured), (['b.txt'], ['d.txt'], ['c.txt']))
        self.assertEqual(ccg.compare_entries(held, held), ([], [], []))


class Revisions(unittest.TestCase):
    def test_head_revision_is_short(self):
        self.assertRegex(ccg.head_revision(), r'^[0-9a-f]{7,40}$')

    def test_changed_source_against_head_is_the_working_tree(self):
        for path in ccg.changed_source('HEAD'):
            self.assertNotRegex(path, ccg.HARMLESS)


class Harmless(unittest.TestCase):
    def test_what_may_differ_from_head(self):
        for path in ('internal/core/x_test.go', 'internal/core/testdata/commands/strings.txt', 'docs/plan.md',
                     'scripts/capture-command-goldens.py', 'internal/golden/golden.go', 'README.md'):
            self.assertRegex(path, ccg.HARMLESS, path)
        for path in ('internal/core/eval.go', 'cmd/keel/main.go', 'go.mod', 'internal/server/server.go'):
            self.assertNotRegex(path, ccg.HARMLESS, path)


if __name__ == '__main__':
    unittest.main()
