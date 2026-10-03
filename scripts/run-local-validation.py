#!/usr/bin/env python3
"""Run a short disposable validation command with local time and disk limits.

Use {out} in command arguments for this wrapper's newly created output directory.
The command and all children share an owned process group and inherit a per-file
limit. Completed output stays available for publication; this tool never deletes
evidence. Long validation belongs on hosted runners.

Exit status: 0 when the command passed every check; 1 when the command failed
or the wrapper could not finish its own checks; 2 for arguments refused before
launch; 3 when one of this wrapper's limits stopped or broke the command. A
limit belongs to this local harness, not to the code under test, so the report's
limit_hit field and the last line on stderr name the limit and the flag that
changes it. A failed command whose output reports a write refused for its size
("file too large", SIGXFSZ), while no file came near this wrapper's per-file
limit, is named as a possible hit and keeps status 1: the command may have set
a smaller limit of its own.
"""
import argparse
import hashlib
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
import tempfile

REPORT_RESERVE_BYTES = 64 << 10
# Defaults for --max-output-mib and --max-file-mib. CI's test footprint record
# (scripts/test-file-footprint.py) reads these to warn before a test outgrows them.
DEFAULT_MAX_OUTPUT_MIB, DEFAULT_MAX_FILE_MIB = 512, 256
EXIT_FAILED, EXIT_LIMIT = 1, 3
# Text a write refused by RLIMIT_FSIZE leaves in command output: the EFBIG
# message from C, Go and Python ("file too large", "[Errno 27] File too
# large"), and the SIGXFSZ description from Go and shells ("file size limit
# exceeded").
FILE_LIMIT_SIGNATURES = (b'file too large', b'file size limit exceeded')
LIMIT_NAMES = dict(time='time', output='output budget', free_space='free-space reserve',
                   file_size='per-file size')


class LimitReached(RuntimeError):
    """One of this wrapper's own limits stopped the command."""

    def __init__(self, limit, message):
        super().__init__(message)
        self.limit = limit


def scan_output(root):
    """Return total bytes, the largest file's bytes and that file's path."""
    total, largest, largest_path = 0, 0, None
    def traversal_failed(error):
        if isinstance(error, FileNotFoundError):
            return  # Go and test harnesses remove temporary directories during a sample.
        raise error
    for folder, directories, files in os.walk(root, followlinks=False, onerror=traversal_failed):
        directories[:] = [name for name in directories if not (Path(folder)/name).is_symlink()]
        for name in files:
            try:
                size = (Path(folder)/name).lstat().st_size
            except FileNotFoundError:
                continue  # A rewrite can replace a file during the sample.
            total += size
            if size > largest:
                largest, largest_path = size, Path(folder)/name
    return total, largest, largest_path


def directory_bytes(root):
    return scan_output(root)[0]


def signature_lines(path, most=3):
    """Up to `most` output lines reporting a write refused for its size.

    The log is read in bounded chunks: it may itself be the file that reached
    the ceiling, and it need not contain newlines.
    """
    def matches(line):
        lower = line.lower()
        return any(s in lower for s in FILE_LIMIT_SIGNATURES)
    def excerpt(line):
        line = line.strip()
        if len(line) > 400:  # Keep the message and the path before it.
            lower = line.lower()
            at = min(i for i in (lower.find(s) for s in FILE_LIMIT_SIGNATURES) if i >= 0)
            line = line[max(0, at-320):at+80]
        return line.decode('utf-8', 'replace')
    found = []
    try:
        log = os.fdopen(os.open(path, os.O_RDONLY | getattr(os, 'O_NOFOLLOW', 0)), 'rb')
    except OSError:
        return found
    with log:
        pending = b''
        while len(found) < most:
            chunk = log.read(1 << 20)
            *lines, pending = (pending + chunk).split(b'\n')
            if not chunk:
                lines.append(pending)  # The last line need not end in a newline.
            found += [line for line in lines if matches(line)]
            if not chunk:
                break
            if matches(pending):
                # Too long a line to carry whole: keep the match, drop the rest.
                found.append(pending)
                pending = b''
            pending = pending[-4096:]  # Enough to join a signature split across reads.
    return [excerpt(line) for line in found[:most]]


def describe_limit(report, limit):
    """The limit's value, the flag that changes it, and what to do instead."""
    def mib(n):
        return f'{n/2**20:g} MiB'
    if limit == 'time':
        return (f"{report['seconds_limit']:g} s", '--seconds',
                'raise --seconds (a local check may take at most 120), narrow the command '
                'to one package or test, or run it on hosted CI')
    if limit == 'output':
        return (mib(report['output_limit_bytes']), '--max-output-mib',
                'raise --max-output-mib (at most 1024; the disposable Go cache counts '
                'toward it) or run the check on hosted CI')
    if limit == 'free_space':
        return (f"{report['minimum_free_bytes']/2**30:g} GiB kept free", '--min-free-gib',
                'free disk space, or lower --min-free-gib (at least 2)')
    effective = report.get('effective_file_limit_bytes', report['file_limit_bytes'])
    if effective < report['file_limit_bytes']:
        advice = (f'the calling shell already limits files to {mib(effective)} '
                  '(ulimit -f), below --max-file-mib; raise that limit first')
    else:
        advice = ('raise --max-file-mib (at most --max-output-mib), make the test write '
                  'less, or skip it under testing.Short() and run it on hosted CI')
    return f'{mib(effective)} per file', '--max-file-mib', advice


def diagnose(report, root, stopped, exit_code):
    """Record which of this wrapper's limits explains a failed run, if any.

    stopped is the LimitReached that ended the run, or None.
    """
    try:
        record_peak_file(report, root, *scan_output(root)[1:])
    except OSError:
        pass  # The failure is already recorded; this scan only adds evidence.
    confidence, evidence = 'confirmed', []
    if stopped is None:
        ceiling = report.get('effective_file_limit_bytes', report['file_limit_bytes'])
        peak = report.get('peak_file_bytes', 0)
        killed = exit_code == -signal.SIGXFSZ
        if killed:
            evidence.append('the command was killed by SIGXFSZ, the file-size limit signal')
        if peak >= ceiling:
            evidence.append(f"{report['peak_file']} reached the {ceiling}-byte ceiling")
        lines = [f'command output: {line}' for line in signature_lines(root/'command.log')]
        if evidence:
            # A file can also end exactly at the ceiling by chance; a refused
            # write's message or signal settles it.
            confidence = 'confirmed' if killed or lines else 'likely'
        elif lines:
            lines.append(f"largest file seen: {report.get('peak_file')} at {peak} bytes")
            # A test may set a much smaller limit of its own on purpose. A file
            # seen at half this wrapper's ceiling ties the error to the wrapper.
            # Samples are a quarter second apart, so a file that reached the
            # ceiling and was then deleted may have been seen only part-way.
            confidence = 'likely' if peak*2 >= ceiling else 'possible'
        else:
            return
        evidence += lines
        limit = 'file_size'
    else:
        limit = stopped.limit
        evidence.append(str(stopped))
        if limit == 'output':
            # Say what filled the budget: often the disposable Go cache.
            usage = []
            try:
                children = sorted(root.iterdir())
            except OSError:
                children = []
            for child in children:
                try:
                    usage.append((scan_output(child)[0] if child.is_dir() and not child.is_symlink()
                                  else child.lstat().st_size, child.name))
                except OSError:
                    pass
            evidence.append('usage by entry: ' + ', '.join(
                f'{name} {size/2**20:.1f} MiB' for size, name in sorted(usage, reverse=True)[:6]))
    value, flag, advice = describe_limit(report, limit)
    name = LIMIT_NAMES[limit]
    if confidence == 'possible':
        message = (f'the command failed and reported a write refused for its size, but no file '
                   f'came near the local {name} limit ({value}, {flag}); the error may be the '
                   f'command\'s own: {evidence[0]}')
    else:
        message = (f'stopped by the local {name} limit ({value}, {flag}), which belongs to this '
                   f'harness rather than the code under test: {evidence[0]}. To proceed, {advice}')
    report['limit_hit'] = dict(limit=limit, flag=flag, value=value, confidence=confidence,
                               evidence=evidence, advice=advice, message=message)


def record_peak_file(report, root, largest, largest_path):
    if largest_path is not None and largest > report.get('peak_file_bytes', -1):
        report['peak_file_bytes'] = largest
        report['peak_file'] = str(Path(largest_path).relative_to(root))


def stop_group(process):
    # The group was created by this wrapper. Stop descendants even when their
    # immediate parent has already exited, so a smoke cannot leave a server.
    for sig in (signal.SIGTERM, signal.SIGKILL):
        process.poll()  # Reap an exited parent; still stop any remaining group members.
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
    if shutil.disk_usage(root.parent).free < args.min_free_gib * 2**30 + REPORT_RESERVE_BYTES:
        raise ValueError('insufficient free disk space before validation (reserve set by --min-free-gib)')
    root.mkdir(mode=0o700)
    root = root.resolve()
    root_identity = (root.stat().st_dev, root.stat().st_ino)
    command = [arg.replace('{out}', str(root)) for arg in command]
    report = dict(status='running', command=command, seconds_limit=args.seconds,
                  output_limit_bytes=args.max_output_mib*2**20,
                  file_limit_bytes=args.max_file_mib*2**20,
                  status_report_reserve_bytes=REPORT_RESERVE_BYTES,
                  minimum_free_bytes=int(args.min_free_gib*2**30))
    process = None
    stopped = None
    previous_limit = resource.getrlimit(resource.RLIMIT_FSIZE)
    started = time.monotonic()
    previous_handlers = {}
    def check_limits():
        used, largest, largest_path = scan_output(root)
        report['peak_output_bytes'] = max(used, report.get('peak_output_bytes', 0))
        record_peak_file(report, root, largest, largest_path)
        if time.monotonic()-started >= args.seconds:
            raise LimitReached('time', 'local validation time budget exhausted')
        if used + REPORT_RESERVE_BYTES > report['output_limit_bytes']:
            raise LimitReached('output', 'local validation output budget exhausted')
        if shutil.disk_usage(root).free < report['minimum_free_bytes'] + REPORT_RESERVE_BYTES:
            raise LimitReached('free_space', 'local validation minimum free-space reserve reached')
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
        go_temporary = root/'go-tmp'
        go_temporary.mkdir()
        env = dict(os.environ, KEEL_LOCAL_VALIDATION_ROOT=str(root), TMPDIR=str(temporary),
                   GOTMPDIR=str(go_temporary), GOCACHE=str(cache))
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
        if isinstance(exc, LimitReached):
            stopped = exc
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
            if isinstance(exc, LimitReached) and stopped is None:
                stopped = exc
        finally:
            resource.setrlimit(resource.RLIMIT_FSIZE, previous_limit)
            for sig, handler in previous_handlers.items():
                signal.signal(sig, handler)
            if report['status'] != 'passed':
                try:
                    diagnose(report, root, stopped, report.get('command_exit_code'))
                except Exception as exc:
                    report['limit_diagnosis_failure'] = repr(exc)
            elif report.get('peak_file_bytes', 0) >= report['effective_file_limit_bytes']:
                # Kernels shorten a write that crosses the ceiling without an
                # error, so a command can pass with a silently truncated file.
                report['file_limit_warning'] = (
                    f"{report['peak_file']} reached the {report['effective_file_limit_bytes']}-byte "
                    'per-file ceiling (--max-file-mib); writes past it were cut short or refused, '
                    'so check that the command did not ignore one')
            # Compilation caches are reproducible, including after a failure.
            # Preserve other temporary failure files for diagnosis/publication.
            try:
                current = root.lstat()
                if root.is_symlink() or (current.st_dev, current.st_ino) != root_identity:
                    raise RuntimeError('validation output root identity changed')
                root.chmod(0o700)
                for name, field in [('go-cache', 'go_cache_pruned'),
                                    ('go-tmp', 'go_temporary_files_pruned')]:
                    try:
                        shutil.rmtree(root/name, ignore_errors=False)
                    except FileNotFoundError:
                        pass
                    report[field] = True
                if report['status'] == 'passed':
                    shutil.rmtree(root/'tmp', ignore_errors=False)
                    report['temporary_files_pruned'] = True
            except FileNotFoundError:
                pass
            except OSError as exc:
                report.update(status='failed', cache_cleanup_failure=repr(exc))
            report['elapsed_seconds'] = time.monotonic()-started
            body = (json.dumps(report, indent=2)+'\n').encode()
            if len(body) > REPORT_RESERVE_BYTES:
                report = dict(status='failed', failure='resource report exceeded its reserved size',
                              oversized_report_sha256=hashlib.sha256(body).hexdigest(),
                              limit_hit=report.get('limit_hit'))
                body = (json.dumps(report, indent=2)+'\n').encode()
            try:
                current = root.lstat()
                if root.is_symlink() or (current.st_dev, current.st_ino) != root_identity:
                    raise RuntimeError('validation output root identity changed')
                # Never follow or overwrite a command-created report path.
                with (root/'local-resource-report.json').open('xb') as output:
                    output.write(body)
            except Exception as exc:
                report.update(status='failed', report_persistence_failure=repr(exc))
                try:
                    # Preserve command-created paths and write beside the run.
                    # This is a cooperative resource guard, not a filesystem sandbox.
                    with tempfile.NamedTemporaryFile(mode='w', dir=root.parent,
                            prefix=root.name+'-resource-failure-', suffix='.json', delete=False) as output:
                        report['fallback_report_path'] = output.name
                        output.write(json.dumps(report, indent=2)+'\n')
                except Exception as fallback_exc:
                    report['fallback_persistence_failure'] = repr(fallback_exc)
                # Always reach the stdout report even if disk persistence fails.
    print(json.dumps(report, indent=2))
    if report['status'] == 'passed':
        if 'file_limit_warning' in report:
            print(f"run-local-validation: warning: {report['file_limit_warning']}", file=sys.stderr)
        return 0
    hit = report.get('limit_hit')
    if hit:
        print(f"run-local-validation: {hit['message']}", file=sys.stderr)
        if hit['confidence'] != 'possible':
            return EXIT_LIMIT
    return EXIT_FAILED


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--out', type=Path, required=True,
                        help='fresh output directory to create, normally below dist/')
    parser.add_argument('--seconds', type=float, default=120,
                        help='stop the command after this many seconds (default and maximum 120)')
    parser.add_argument('--max-output-mib', type=int, default=DEFAULT_MAX_OUTPUT_MIB,
                        help='stop once the output directory, including the disposable Go '
                             f'cache, holds this many MiB (default {DEFAULT_MAX_OUTPUT_MIB}, at most 1024)')
    parser.add_argument('--max-file-mib', type=int, default=DEFAULT_MAX_FILE_MIB,
                        help='per-file size limit inherited by the command and its children; '
                             'a larger write fails with EFBIG or SIGXFSZ (default '
                             f'{DEFAULT_MAX_FILE_MIB}, at most --max-output-mib)')
    parser.add_argument('--min-free-gib', type=float, default=10,
                        help='stop if free disk space falls below this many GiB (default 10, '
                             'at least 2)')
    parser.add_argument('command', nargs=argparse.REMAINDER)
    try:
        raise SystemExit(run(parser.parse_args()))
    except ValueError as exc:
        parser.error(str(exc))
