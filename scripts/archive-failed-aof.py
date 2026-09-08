#!/usr/bin/env python3
"""Bound failed-AOF evidence output; prune only a verified complete archive.

Incompressible logs that exceed the output/free-space budget retain their source
and bounded prefix/tail samples. A nonzero exit stops further benchmark arms.
Partial samples are explicitly insufficient for full replay validation.
"""
import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import shutil
import stat
import tempfile
import zlib


class ArchiveBudgetExceeded(Exception):
    pass


class LimitedWriter:
    def __init__(self, file, limit):
        self.file, self.limit, self.written = file, limit, 0

    def write(self, data):
        if len(data) > self.limit - self.written:
            raise ArchiveBudgetExceeded('compressed evidence budget exceeded')
        count = self.file.write(data)
        self.written += count
        return count


def digest_file(path):
    digest, size = hashlib.sha256(), 0
    with path.open('rb') as source:
        while chunk := source.read(1 << 20):
            digest.update(chunk)
            size += len(chunk)
    return size, digest.hexdigest()


def verify_archive(path, size, digest):
    checked, count = hashlib.sha256(), 0
    with gzip.open(path, 'rb') as source:
        while chunk := source.read(1 << 20):
            checked.update(chunk)
            count += len(chunk)
            if count > size:
                raise RuntimeError('failed AOF archive exceeds its source length')
    if count != size or checked.hexdigest() != digest:
        raise RuntimeError('failed AOF archive verification mismatch')


def write_report(path, report):
    """Replace the small journal atomically before advancing archive ownership."""
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(mode='w', dir=path.parent,
                                         prefix='.failed-aof-', delete=False) as output:
            temporary = Path(output.name)
            output.write(json.dumps(report, indent=2) + '\n')
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)


def verified_existing(path, size, digest):
    """Reuse only regular, complete artifacts matching the recorded source bytes."""
    if not stat.S_ISREG(path.lstat().st_mode):
        raise FileExistsError('existing evidence is not a regular file')
    try:
        verify_archive(path, size, digest)
    except (OSError, EOFError, RuntimeError, zlib.error) as error:
        raise FileExistsError('existing evidence does not match the source') from error


def finish_complete(path, report_path, report, archive):
    verified_existing(archive, report['bytes'], report['sha256'])
    compressed_size, compressed_digest = digest_file(archive)
    report.update(complete=True, state='removal_pending', artifact=archive.name,
                  compressed_bytes=compressed_size, compressed_sha256=compressed_digest,
                  preserved_bytes=report['bytes'],
                  limitation='Complete archive verified against the original digest.')
    # The digest and removal intent survive a stop immediately after unlink.
    write_report(report_path, report)
    path.unlink(missing_ok=True)
    report.update(source_removed=True, state='complete')
    write_report(report_path, report)
    return report


def preserve(path, compressed_limit=64 << 20, sample_limit=64 << 10):
    """Archive a stopped benchmark's owned log; retries reconcile published files."""
    report_path = path.with_name('failed-aof.json')
    archive = path.with_name(path.name + '.gz')
    partial = path.with_name(path.name + '.gz.partial')
    report = json.loads(report_path.read_text()) if report_path.exists() else None
    if path.exists() or path.is_symlink():
        before = path.lstat()
        if not stat.S_ISREG(before.st_mode):
            raise ValueError('failed evidence must be an owned regular file')
        size, digest = digest_file(path)
        after = path.lstat()
        if (before.st_ino, before.st_size, before.st_mtime_ns) != (after.st_ino, after.st_size, after.st_mtime_ns):
            raise RuntimeError('failed AOF changed while hashing')
        if report and (report.get('sha256') != digest or report.get('bytes') != size):
            raise FileExistsError('existing evidence describes a different AOF')
    elif report and report.get('complete'):
        size, digest = report['bytes'], report['sha256']
    else:
        raise FileNotFoundError('original AOF missing without a completed archive journal')

    free = shutil.disk_usage(path.parent).free
    limit = min(compressed_limit, max(0, free - (8 << 20)))
    if report is None:
        report = {'bytes': size, 'sha256': digest, 'complete': False,
                  'source_removed': False, 'compressed_limit_bytes': limit,
                  'state': 'preparing', 'samples': [],
                  'limitation': 'Partial samples cannot validate complete replay.'}
    limit = min(limit, report['compressed_limit_bytes'])
    if archive.exists() or archive.is_symlink():
        # Also recovers a legacy publication that preceded its first journal.
        return finish_complete(path, report_path, report, archive)
    if report.get('complete'):
        raise FileNotFoundError('completed evidence archive is missing')
    if report.get('state') == 'partial' or (report_path.exists() and 'state' not in report):
        for sample in report['samples']:
            if sample['artifact'] not in (path.name + '.prefix.gz', path.name + '.tail.gz'):
                raise ValueError('unexpected sample name')
            verified_existing(path.with_name(sample['artifact']), sample['bytes'], sample['sha256'])
        return report
    write_report(report_path, report)

    staging_blocked = partial.exists() or partial.is_symlink()
    if staging_blocked:
        try:
            verified_existing(partial, size, digest)
        except FileExistsError:
            # Keep unknown/torn staging bytes, but still publish bounded samples.
            report['reason'] = 'unfinished archive staging retained; preserving samples'
        else:
            os.link(partial, archive)
            return finish_complete(path, report_path, report, archive)

    if limit > 0 and report.get('state') != 'sampling' and not staging_blocked:
        owns_partial = False
        try:
            with partial.open('xb') as file:
                owns_partial = True
                with gzip.GzipFile(fileobj=LimitedWriter(file, limit), mode='wb', mtime=0) as output:
                    with path.open('rb') as source:
                        while chunk := source.read(1 << 20):
                            output.write(chunk)
            verify_archive(partial, size, digest)
            os.link(partial, archive)
        except ArchiveBudgetExceeded:
            report['reason'] = 'compressed output budget exceeded; original retained'
        finally:
            if owns_partial:
                partial.unlink(missing_ok=True)
        if archive.exists():
            return finish_complete(path, report_path, report, archive)

    # Persist the sample plan before publishing either end. Retries use the same
    # offsets even if the free-space estimate changes between invocations.
    if report.get('state') != 'sampling':
        free = shutil.disk_usage(path.parent).free
        take = min(sample_limit, max(0, (free - (1 << 20)) // 4))
        report.update(state='sampling', sample_bytes=take)
        write_report(report_path, report)
    take = report['sample_bytes']
    samples = []
    with path.open('rb') as source:
        for label, offset in [('prefix', 0), ('tail', max(0, size-take))]:
            if take == 0:
                break
            source.seek(offset)
            body = source.read(take)
            sample = path.with_name(path.name + '.' + label + '.gz')
            digest = hashlib.sha256(body).hexdigest()
            if sample.exists() or sample.is_symlink():
                verified_existing(sample, len(body), digest)
            else:
                # A killed process may leave its staging entry behind. A new
                # bounded sample uses a unique entry without deleting that evidence.
                sample_partial = None
                try:
                    with tempfile.NamedTemporaryFile(mode='wb', dir=sample.parent,
                                                     prefix='.' + sample.name + '-',
                                                     suffix='.partial', delete=False) as output:
                        sample_partial = Path(output.name)
                        output.write(gzip.compress(body, mtime=0))
                    os.link(sample_partial, sample)
                finally:
                    if sample_partial is not None:
                        sample_partial.unlink(missing_ok=True)
            samples.append({'artifact': sample.name, 'offset': offset,
                            'bytes': len(body), 'sha256': digest})
    report.update(state='partial', samples=samples, preserved_bytes=min(size, 2*take))
    report.setdefault('reason', 'insufficient free space for a complete archive; original retained')
    write_report(report_path, report)
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('root', type=Path)
    args = parser.parse_args()
    incomplete = False
    paths = set(args.root.rglob('store.aof'))
    # A stop after unlink leaves only a journal and archive to reconcile.
    paths.update(report.with_name('store.aof') for report in args.root.rglob('failed-aof.json'))
    for path in sorted(paths):
        if path.is_symlink():
            continue
        try:
            report = preserve(path)
        except Exception as error:
            print(json.dumps({'file': str(path), 'complete': False, 'error': repr(error)}))
            incomplete = True
            continue
        print(json.dumps({'file': str(path), **report}))
        incomplete |= not report['complete']
    return int(incomplete)


if __name__ == '__main__':
    raise SystemExit(main())
