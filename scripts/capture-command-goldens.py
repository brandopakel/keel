#!/usr/bin/env python3
"""Capture the command corpora: every command's replies and log records.

The corpora are the byte-identical gate of phase 4 of the embedding plan
(docs/embedding-plan.md): what every command answers, on a RESP2 and on a
RESP3 connection, and what it writes to the log, captured once from develop
and compared on every run since. internal/core/command_golden_test.go holds
the commands the engine runs and internal/server/connection_golden_test.go
the commands the server answers itself; each writes its fixture when its
capture test runs with KEEL_CAPTURE_COMMAND_GOLDEN naming a directory, and
each reads it back on every run.

This script is how the fixture is written, so that it is written the same
way every time:

1. It reads the revision each test names (commandGoldenRevision and
   connectionGoldenRevision) and refuses to run unless both name the
   checkout's HEAD, in the short form `git rev-parse --short=7` prints, and
   the working tree holds no change outside tests, fixtures, scripts and
   docs. The fixture then describes develop, and nothing else.
2. It runs both capture tests through scripts/run-local-validation.py, with
   the wrapper's time and disk limits and its disposable Go cache, into a
   fresh directory under dist/. Each capture runs every case three times in
   each protocol and refuses a case that is not the same from run to run.
3. It copies what was captured into the fixture directories the tests name,
   replacing what was there, and runs the replay tests once through the
   wrapper, so that the fixture it leaves is one the tests pass on.

With --check it runs only the replay tests, the way CI does, and writes
nothing into the tree.
"""
import argparse
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys

ROOT = Path(__file__).resolve().parent.parent
WRAPPER = ROOT / 'scripts' / 'run-local-validation.py'
# Each test file, the constant naming its revision, the one naming its
# fixture directory, and the directory the capture writes under the root
# KEEL_CAPTURE_COMMAND_GOLDEN names.
SOURCES = (
    ('internal/core/command_golden_test.go', 'commandGoldenRevision', 'commandGoldenDir', 'core'),
    ('internal/server/connection_golden_test.go', 'connectionGoldenRevision', 'connectionGoldenDir', 'server'),
)
CAPTURE_TESTS = '^Test(Command|Connection)GoldenCapture$'
REPLAY_TESTS = '^Test(Command|Connection)Golden'
PACKAGES = ('./internal/core', './internal/server')
# Changes to these do not change what a command answers or records, so a
# tree that differs from HEAD only there still describes HEAD: tests,
# fixtures, scripts, documents, and internal/golden, the fixture format,
# which only tests import.
HARMLESS = re.compile(r'(^|/)(testdata/|docs/|scripts/|internal/golden/|[^/]*_test\.go$|[^/]*\.md$)')


def read_constant(source, name):
    """The string a Go constant is declared as, which may be built from
    another constant: `const x = "a" + y`."""
    text = source.read_text()
    constants = dict(re.findall(r'^\s*(\w+)\s*=\s*(.+?)\s*$', text, re.MULTILINE))
    if name not in constants:
        raise ValueError(f'{source}: no constant {name}')

    def value(expr):
        parts = []
        for term in expr.split('+'):
            term = term.strip()
            if term.startswith('"'):
                parts.append(json.loads(term))
            elif term in constants:
                parts.append(value(constants[term]))
            else:
                raise ValueError(f'{source}: cannot read {name} from {expr!r}')
        return ''.join(parts)

    return value(constants[name])


def fixture_targets(root=ROOT):
    """For each corpus, its revision and the directory its fixture lives in."""
    targets = []
    for source, revision, directory, captured in SOURCES:
        path = root / source
        fixture = (path.parent / read_constant(path, directory)).resolve()
        targets.append(dict(source=source, revision=read_constant(path, revision), fixture=fixture, captured=captured))
    return targets


def head_revision(root=ROOT):
    return subprocess.run(['git', '-C', str(root), 'rev-parse', '--short=7', 'HEAD'], check=True,
                          capture_output=True, text=True).stdout.strip()


def changed_source(root=ROOT):
    """Paths the working tree changes against HEAD, apart from the harmless."""
    status = subprocess.run(['git', '-C', str(root), 'status', '--porcelain', '--untracked-files=all'],
                            check=True, capture_output=True, text=True).stdout
    changed = []
    for line in status.splitlines():
        path = line[3:].split(' -> ')[-1]
        if not HARMLESS.search(path):
            changed.append(path)
    return changed


def summarize(fixture):
    """Count a fixture directory's cases, commands and records."""
    cases = commands = records = 0
    for path in sorted(fixture.glob('*.txt')):
        for line in path.read_text().splitlines():
            cases += line.startswith('case ')
            commands += line.startswith('> ')
            records += line.startswith('~')
    return dict(cases=cases, commands=commands, records=records)


def run_wrapped(out, seconds, env, *command):
    """Run command through the local validation wrapper, into out."""
    argv = [sys.executable, str(WRAPPER), '--out', str(out), '--seconds', str(seconds), '--',
            'env', *[f'{k}={v}' for k, v in env.items()], *command]
    return subprocess.run(argv, cwd=ROOT).returncode


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--out', type=Path, default=Path('dist/command-goldens'),
                    help='fresh output directory for the wrapper (default dist/command-goldens)')
    ap.add_argument('--check', action='store_true', help='run the replay tests only; write nothing into the tree')
    ap.add_argument('--seconds', type=int, default=120, help='the wrapper deadline for each go test (default 120)')
    args = ap.parse_args(argv)

    targets = fixture_targets()
    head = head_revision()
    for target in targets:
        if target['revision'] != head:
            print(f"{target['source']} names revision {target['revision']}, and HEAD is {head}: "
                  'set the constant to the revision being captured first', file=sys.stderr)
            return 2
    if not args.check:
        changed = changed_source()
        if changed:
            print('the working tree changes what a command may answer; commit or stash first:', file=sys.stderr)
            for path in changed:
                print(f'  {path}', file=sys.stderr)
            return 2
        capture_root = (args.out / 'fixtures').resolve()
        status = run_wrapped(args.out, args.seconds, {'KEEL_CAPTURE_COMMAND_GOLDEN': str(capture_root)},
                             'go', 'test', '-count=1', '-run', CAPTURE_TESTS, *PACKAGES)
        if status != 0:
            print(f'capture failed with status {status}; see {args.out}', file=sys.stderr)
            return 1
        for target in targets:
            captured = capture_root / target['captured']
            if not (captured / 'manifest.json').exists():
                print(f'{captured} holds no manifest: the capture test wrote nothing', file=sys.stderr)
                return 1
            shutil.rmtree(target['fixture'], ignore_errors=True)
            shutil.copytree(captured, target['fixture'])
            print(f"{target['fixture'].relative_to(ROOT)}: {summarize(target['fixture'])}")
    check_out = args.out.with_name(args.out.name + '-check') if not args.check else args.out
    status = run_wrapped(check_out, args.seconds, {}, 'go', 'test', '-count=1', '-run', REPLAY_TESTS, *PACKAGES)
    if status != 0:
        print(f'the replay tests failed with status {status}; see {check_out}', file=sys.stderr)
        return 1
    print('the replay tests pass on the fixture')
    return 0


if __name__ == '__main__':
    sys.exit(main())
