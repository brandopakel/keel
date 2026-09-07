#!/usr/bin/env python3
"""Run a short disposable validation command with local time and disk limits.

Use {out} in command arguments for this wrapper's newly created output directory.
The command and all children share an owned process group and inherit a per-file
limit. Completed output stays available for publication; this tool never deletes
evidence. Long validation belongs on hosted runners.
"""
import argparse
import json
import math
import os
from pathlib import Path
import resource
import shutil
import signal
import subprocess
import sys
import time


def directory_bytes(root):
    total = 0
    for folder, directories, files in os.walk(root, followlinks=False):
        directories[:] = [name for name in directories if not (Path(folder)/name).is_symlink()]
        for name in files:
            try:
                total += (Path(folder)/name).lstat().st_size
            except FileNotFoundError:
                pass  # A rewrite can replace a file during the sample.
    return total


def stop_group(process):
    # The group was created by this wrapper. Stop descendants even when their
    # immediate parent has already exited, so a smoke cannot leave a server.
    for sig in (signal.SIGTERM, signal.SIGKILL):
        try:
            os.killpg(process.pid, sig)
        except ProcessLookupError:
            break
        if sig == signal.SIGTERM:
            time.sleep(.2)
    process.wait(timeout=5)


def run(args):
    if not math.isfinite(args.seconds) or not 0 < args.seconds <= 120:
        raise ValueError('local checks must be at most 120 seconds; use hosted validation for longer work')
    if not 1 <= args.max_output_mib <= 1024 or not 1 <= args.max_file_mib <= args.max_output_mib:
        raise ValueError('output budget must be 1..1024 MiB and file budget no larger than output budget')
    if not math.isfinite(args.min_free_gib) or args.min_free_gib < 2:
        raise ValueError('keep at least 2 GiB free (default 10 GiB)')
    command = args.command[1:] if args.command[:1] == ['--'] else args.command
    if not command:
        raise ValueError('supply a validation command after --')
    root = args.out.absolute()
    if root.exists() or root.is_symlink():
        raise ValueError('output directory must be fresh')
    root.parent.mkdir(parents=True, exist_ok=True)
    if shutil.disk_usage(root.parent).free < args.min_free_gib * 2**30:
        raise ValueError('insufficient free disk space before validation')
    root.mkdir(mode=0o700)
    root = root.resolve()
    command = [arg.replace('{out}', str(root)) for arg in command]
    report = dict(status='running', command=command, seconds_limit=args.seconds,
                  output_limit_bytes=args.max_output_mib*2**20,
                  file_limit_bytes=args.max_file_mib*2**20,
                  minimum_free_bytes=int(args.min_free_gib*2**30))
    process = None
    previous_limit = resource.getrlimit(resource.RLIMIT_FSIZE)
    started = time.monotonic()
    previous_handlers = {}
    def check_limits():
        used = directory_bytes(root)
        report['peak_output_bytes'] = max(used, report.get('peak_output_bytes', 0))
        if time.monotonic()-started >= args.seconds:
            raise TimeoutError('local validation time budget exhausted')
        if used > report['output_limit_bytes']:
            raise RuntimeError('local validation output budget exhausted')
        if shutil.disk_usage(root).free < report['minimum_free_bytes']:
            raise RuntimeError('local validation minimum free-space reserve reached')
    try:
        # Set only the soft limit and restore it afterward. Children inherit it
        # without preexec_fn, which is unsafe in a multithreaded parent.
        ceiling = args.max_file_mib*2**20
        for inherited in previous_limit:
            if inherited != resource.RLIM_INFINITY:
                ceiling = min(ceiling, inherited)
        resource.setrlimit(resource.RLIMIT_FSIZE, (ceiling, previous_limit[1]))
        report['effective_file_limit_bytes'] = ceiling
        def interrupted(signum, frame):
            raise InterruptedError(f'validation interrupted by signal {signum}')
        for sig in (signal.SIGTERM, signal.SIGINT):
            previous_handlers[sig] = signal.signal(sig, interrupted)
        temporary = root/'tmp'
        temporary.mkdir()
        cache = root/'go-cache'
        cache.mkdir()
        env = dict(os.environ, KEEL_LOCAL_VALIDATION_ROOT=str(root), TMPDIR=str(temporary),
                   GOTMPDIR=str(temporary), GOCACHE=str(cache))
        with (root/'command.log').open('wb') as log:
            process = subprocess.Popen(command, stdout=log, stderr=log, env=env,
                                       start_new_session=True)
            report['pid'] = process.pid
            while True:
                check_limits()
                code = process.poll()
                if code is not None:
                    report['command_exit_code'] = code
                    if code != 0:
                        raise RuntimeError(f'validation command exited with {code}')
                    report['status'] = 'command_completed'
                    break
                time.sleep(.25)
    except BaseException as exc:
        report.update(status='failed', failure=repr(exc))
    finally:
        # Ignore repeat interrupts during bounded cleanup, then restore callers.
        for sig in previous_handlers:
            signal.signal(sig, signal.SIG_IGN)
        try:
            if process is not None:
                stop_group(process)
            if report['status'] == 'command_completed':
                # The command (or a child) may have written after the last
                # sample. Check again after exit/descendant cleanup, before
                # success and before deleting caches or temporary evidence.
                check_limits()
                report['status'] = 'passed'
        except Exception as exc:
            report.update(status='failed', cleanup_failure=repr(exc))
        finally:
            resource.setrlimit(resource.RLIMIT_FSIZE, previous_limit)
            for sig, handler in previous_handlers.items():
                signal.signal(sig, handler)
            # Compilation caches are reproducible, including after a failure.
            # Preserve other temporary failure files for diagnosis/publication.
            try:
                shutil.rmtree(root/'go-cache', ignore_errors=False)
                report['go_cache_pruned'] = True
                if report['status'] == 'passed':
                    shutil.rmtree(root/'tmp', ignore_errors=False)
                    report['temporary_files_pruned'] = True
            except FileNotFoundError:
                pass
            except OSError as exc:
                report.update(status='failed', cache_cleanup_failure=repr(exc))
            report['elapsed_seconds'] = time.monotonic()-started
            (root/'local-resource-report.json').write_text(json.dumps(report, indent=2)+'\n')
    print(json.dumps(report, indent=2))
    return 0 if report['status'] == 'passed' else 1


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--seconds', type=float, default=120)
    parser.add_argument('--max-output-mib', type=int, default=512)
    parser.add_argument('--max-file-mib', type=int, default=256)
    parser.add_argument('--min-free-gib', type=float, default=10)
    parser.add_argument('command', nargs=argparse.REMAINDER)
    try:
        raise SystemExit(run(parser.parse_args()))
    except ValueError as exc:
        parser.error(str(exc))
