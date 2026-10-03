#!/usr/bin/env python3
"""Run a test command and record the files each test leaves on disk.

CI runs the Go suite through this so that a test which outgrows the local
validation wrapper's budget (scripts/run-local-validation.py) is noticed in CI
before it fails someone's local check as an apparent product failure.

The command gets fresh TMPDIR and GOTMPDIR directories under --out. Since Go
1.26, t.TempDir is created under GOTMPDIR when it is set, and go test also keeps
its build work there in go-build* directories; other temporary files go to
TMPDIR. Every top-level entry is sampled: a t.TempDir parent is named after its
test, with "/" removed and random digits appended. The record keeps each entry's
peak bytes and its largest file. It is sampled, not a quota, so a file that
grows and is removed between samples can be missed.

The command's output passes through unchanged and its exit status is this
script's exit status. Entries above the warning thresholds become GitHub
warning annotations, and a table goes to $GITHUB_STEP_SUMMARY when it is set.
"""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time


def wrapper_defaults():
    path = Path(__file__).with_name('run-local-validation.py')
    spec = importlib.util.spec_from_file_location('local_validation_defaults', path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module.DEFAULT_MAX_FILE_MIB, module.DEFAULT_MAX_OUTPUT_MIB


def entry_name(name):
    if name.startswith('go-build'):
        return 'go-build (test binaries and build work)'
    return re.sub(r'\d+$', '', name) or name


def sample(roots, entries, peak):
    """Fold one scan of every root's top-level entries into entries."""
    total = 0
    for label, root in roots.items():
        try:
            children = list(os.scandir(root))
        except FileNotFoundError:
            continue
        for child in children:
            size, largest, largest_path = 0, 0, None
            paths = [child.path]
            while paths:
                path = paths.pop()
                try:
                    if os.path.islink(path):
                        continue
                    if os.path.isdir(path):
                        paths += [os.path.join(path, name) for name in os.listdir(path)]
                        continue
                    length = os.lstat(path).st_size
                except OSError:
                    # Tests remove their directories while running, and may
                    # make one unreadable on purpose; neither stops the record.
                    continue
                size += length
                if length > largest:
                    largest, largest_path = length, path
            name = entry_name(child.name)
            record = entries.setdefault(name, dict(name=name, root=label, peak_bytes=0,
                                                   peak_file_bytes=0, peak_file=None))
            record['peak_bytes'] = max(record['peak_bytes'], size)
            if largest > record['peak_file_bytes']:
                record['peak_file_bytes'] = largest
                record['peak_file'] = os.path.relpath(largest_path, root)
            if not name.startswith('go-build'):
                total += size
    peak['test_bytes'] = max(peak['test_bytes'], total)


def mib(n):
    return f'{n/2**20:.1f} MiB'


def annotation(text):
    """Escape text for a GitHub workflow command."""
    return text.replace('%', '%25').replace('\r', '%0D').replace('\n', '%0A')


def main():
    file_default, output_default = wrapper_defaults()
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--out', type=Path, required=True, help='fresh directory for temporaries and the record')
    parser.add_argument('--interval', type=float, default=.1, help='seconds between samples')
    parser.add_argument('--warn-file-mib', type=float, default=file_default/2,
                        help='warn when a test leaves a file this large (default: half the '
                             f'local wrapper\'s {file_default} MiB --max-file-mib default)')
    parser.add_argument('--warn-test-mib', type=float, default=output_default/4,
                        help='warn when one test holds this much on disk at once (default: a '
                             f'quarter of the local wrapper\'s {output_default} MiB --max-output-mib '
                             'default, which a fresh Go cache shares)')
    parser.add_argument('--top', type=int, default=15, help='entries to list in the summary')
    parser.add_argument('command', nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command[1:] if args.command[:1] == ['--'] else args.command
    if not command:
        parser.error('supply a test command after --')
    out = args.out.absolute()
    if out.exists() or out.is_symlink():
        parser.error('output directory must be fresh')
    out.mkdir(parents=True)
    roots = dict(TMPDIR=out/'tmp', GOTMPDIR=out/'go-tmp')
    for root in roots.values():
        root.mkdir()
    env = dict(os.environ, **{name: str(root) for name, root in roots.items()})
    entries, peak, samples, errors = {}, dict(test_bytes=0), 0, []
    def take_sample():
        # A failed sample is recorded rather than raised: the record must
        # never end the run early or leave the test command running alone.
        try:
            sample(roots, entries, peak)
        except Exception as exc:
            errors.append(repr(exc))
    started = time.monotonic()
    process = subprocess.Popen(command, env=env)
    try:
        while process.poll() is None:
            take_sample()
            samples += 1
            time.sleep(args.interval)
    finally:
        if process.poll() is None:
            process.terminate()
            process.wait()
    take_sample()
    elapsed = time.monotonic() - started
    tests = sorted((e for e in entries.values() if not e['name'].startswith('go-build')),
                   key=lambda e: (e['peak_file_bytes'], e['peak_bytes']), reverse=True)
    build = [e for e in entries.values() if e['name'].startswith('go-build')]
    warn_file, warn_test = args.warn_file_mib*2**20, args.warn_test_mib*2**20
    warnings = []
    for entry in tests:
        reasons = []
        if entry['peak_file_bytes'] >= warn_file:
            reasons.append(f"a {mib(entry['peak_file_bytes'])} file ({entry['peak_file']})")
        if entry['peak_bytes'] >= warn_test:
            reasons.append(f"{mib(entry['peak_bytes'])} on disk at once")
        if reasons:
            warnings.append(f"{entry['name']} wrote {' and '.join(reasons)}. The local validation "
                            f"wrapper allows {file_default} MiB per file (--max-file-mib) and "
                            f"{output_default} MiB in all, Go cache included (--max-output-mib). "
                            'Make the test write less, or skip it under testing.Short() so local '
                            'checks can pass -short while CI keeps running it.')
    record = dict(command=command, exit_code=process.returncode, elapsed_seconds=elapsed,
                  samples=samples, interval_seconds=args.interval,
                  local_wrapper_defaults=dict(max_file_mib=file_default, max_output_mib=output_default),
                  warn_file_bytes=int(warn_file), warn_test_bytes=int(warn_test),
                  peak_concurrent_test_bytes=peak['test_bytes'],
                  peak_build_bytes=max((e['peak_bytes'] for e in build), default=0),
                  sampling_errors=errors[:20], warnings=warnings, tests=tests)
    (out/'test-file-footprint.json').write_text(json.dumps(record, indent=2)+'\n')
    for warning in warnings:
        print(f'::warning title=Test outgrows the local validation budget::{annotation(warning)}')
    lines = ['### Test file footprint', '',
             f"Sampled every {args.interval:g} s ({samples} samples) while `{' '.join(command)}` ran. "
             f"Peak of all test temporaries at once: {mib(peak['test_bytes'])}; go test build work: "
             f"{mib(record['peak_build_bytes'])}. Warnings at {mib(warn_file)} per file or "
             f"{mib(warn_test)} per test; the local wrapper allows {file_default} MiB per file and "
             f'{output_default} MiB in all.', '',
             '| Test temp directory | Peak on disk | Largest file | File |', '| --- | ---: | ---: | --- |']
    lines += [f"| {e['name']} | {mib(e['peak_bytes'])} | {mib(e['peak_file_bytes'])} | {e['peak_file'] or ''} |"
              for e in tests[:args.top]]
    lines += [''] + [f'- Warning: {w}' for w in warnings] + ['']
    summary = os.environ.get('GITHUB_STEP_SUMMARY')
    if summary:
        with open(summary, 'a') as output:
            output.write('\n'.join(lines))
    print('\n'.join(lines), file=sys.stderr)
    return process.returncode


if __name__ == '__main__':
    raise SystemExit(main())
