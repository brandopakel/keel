#!/usr/bin/env python3
"""Verify and unpack raw Keel evidence from an authenticated Bencher Jobs API response."""
import argparse
import base64
import hashlib
import io
import json
import tarfile
from pathlib import Path


def unpack(document, destination):
    if 'keel_evidence_v1' in document:
        envelopes = [document['keel_evidence_v1']]
    else:
        envelopes = []
        for result in (document.get('output') or {}).get('results', []):
            for line in (result.get('stderr') or '').splitlines():
                try:
                    value = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if isinstance(value, dict) and 'keel_evidence_v1' in value:
                    envelopes.append(value['keel_evidence_v1'])
    if len(envelopes) != 1:
        raise ValueError('expected one intact evidence envelope; fetch the job with authenticated view access')
    envelope = envelopes[0]
    encoded = envelope['tar_gzip_base64']
    if len(encoded) > 8 * 1024 * 1024:
        raise ValueError('evidence exceeds the supported output limit')
    raw = base64.b64decode(encoded, validate=True)
    if len(raw) != envelope['bytes'] or hashlib.sha256(raw).hexdigest() != envelope['sha256']:
        raise ValueError('evidence length/checksum mismatch')
    with tarfile.open(fileobj=io.BytesIO(raw)) as archive:
        members = archive.getmembers()
        if len(members) > 1000 or sum(m.size for m in members) > 256 * 1024 * 1024:
            raise ValueError('unpacked evidence exceeds the supported limit')
        for member in members:
            path = Path(member.name)
            if (path.is_absolute() or '..' in path.parts or not path.parts or
                    path.parts[0] != 'evidence' or not (member.isfile() or member.isdir())):
                raise ValueError('unexpected archive path or member type')
        destination.mkdir(parents=True, exist_ok=False)
        archive.extractall(destination, filter='data')
    (destination / 'evidence.tar.gz').write_bytes(raw)
    return {'sha256': envelope['sha256'], 'compressed_bytes': len(raw), 'files': len(members)}


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('job_json', type=Path)
    parser.add_argument('--out', type=Path, required=True)
    args = parser.parse_args()
    print(json.dumps(unpack(json.loads(args.job_json.read_text()), args.out)))
