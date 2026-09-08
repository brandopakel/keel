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
    if count != size or checked.hexdigest() != digest:
        raise RuntimeError('failed AOF archive verification mismatch')


def preserve(path, compressed_limit=64 << 20, sample_limit=64 << 10):
    before = path.lstat()
    if not stat.S_ISREG(before.st_mode):
        raise ValueError('failed evidence must be an owned regular file')
    report_path = path.with_name('failed-aof.json')
    size, digest = digest_file(path)
    after = path.lstat()
    if (before.st_ino, before.st_size, before.st_mtime_ns) != (after.st_ino, after.st_size, after.st_mtime_ns):
        raise RuntimeError('failed AOF changed while hashing')
    if report_path.exists():
        report = json.loads(report_path.read_text())
        if report.get('sha256') != digest or report.get('bytes') != size:
            raise FileExistsError('existing evidence describes a different AOF')
        if report.get('complete'):
            if report.get('artifact') != path.name + '.gz':
                raise ValueError('unexpected completed archive name')
            verify_archive(path.with_name(report['artifact']), size, digest)
            path.unlink()
            report['source_removed'] = True
            report_path.write_text(json.dumps(report, indent=2) + '\n')
        return report

    free = shutil.disk_usage(path.parent).free
    limit = min(compressed_limit, max(0, free - (8 << 20)))
    archive = path.with_name(path.name + '.gz')
    partial = path.with_name(path.name + '.gz.partial')
    report = {'bytes': size, 'sha256': digest, 'complete': False,
              'source_removed': False, 'compressed_limit_bytes': limit,
              'samples': [], 'limitation': 'Partial samples cannot validate complete replay.'}
    complete = False
    if limit > 0:
        owns_partial = False
        try:
            with partial.open('xb') as file:
                owns_partial = True
                with gzip.GzipFile(fileobj=LimitedWriter(file, limit), mode='wb', mtime=0) as output:
                    with path.open('rb') as source:
                        while chunk := source.read(1 << 20):
                            output.write(chunk)
            # Verify every uncompressed byte before removing the original.
            verify_archive(partial, size, digest)
            # Publish without replacing any existing directory entry, including
            # a symlink or another preservation attempt's archive.
            os.link(partial, archive)
            complete = True
        except ArchiveBudgetExceeded:
            report['reason'] = 'compressed output budget exceeded; original retained'
        finally:
            if owns_partial:
                partial.unlink(missing_ok=True)

    if complete:
        compressed_size, compressed_digest = digest_file(archive)
        report.update(complete=True, artifact=archive.name,
                      compressed_bytes=compressed_size, compressed_sha256=compressed_digest,
                      preserved_bytes=size, limitation='Complete archive verified against the original digest.')
    else:
        # Leave metadata headroom and keep both ends, especially a torn suffix.
        free = shutil.disk_usage(path.parent).free
        take = min(sample_limit, max(0, (free - (1 << 20)) // 4))
        with path.open('rb') as source:
            for label, offset in [('prefix', 0), ('tail', max(0, size-take))]:
                if take == 0:
                    break
                source.seek(offset)
                body = source.read(take)
                sample = path.with_name(path.name + '.' + label + '.gz')
                with sample.open('xb') as output:
                    output.write(gzip.compress(body, mtime=0))
                report['samples'].append({'artifact': sample.name, 'offset': offset,
                                          'bytes': len(body), 'sha256': hashlib.sha256(body).hexdigest()})
        report['preserved_bytes'] = min(size, 2*take)
        report.setdefault('reason', 'insufficient free space for a complete archive; original retained')

    # A crash between unlink and the second report leaves complete, verified
    # evidence with source_removed=false; it never claims a partial archive is full.
    report_path.write_text(json.dumps(report, indent=2) + '\n')
    if complete:
        path.unlink()
        report['source_removed'] = True
        report_path.write_text(json.dumps(report, indent=2) + '\n')
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('root', type=Path)
    args = parser.parse_args()
    incomplete = False
    for path in args.root.rglob('store.aof'):
        if path.is_symlink() or not path.is_file():
            continue
        report = preserve(path)
        print(json.dumps({'file': str(path), **report}))
        incomplete |= not report['complete']
    return int(incomplete)


if __name__ == '__main__':
    raise SystemExit(main())
