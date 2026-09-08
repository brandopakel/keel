#!/usr/bin/env python3
"""Pin the socket option value in the exact memtier 2.5.1 source used by CI."""
import argparse
import hashlib
import json
from pathlib import Path

UPSTREAM = '620bd2aebb230f759e848d9e9a3cb31204ea9e9f94911af77e6d54d493d5efb6'
PREPARED = 'f887a460f0a009b50aaae5e41a3658feb99b59fa8dd28e7cd49ca83703fdfae2'


def prepare(root):
    path = root / 'shard_connection.cpp'
    body = path.read_bytes()
    digest = hashlib.sha256(body).hexdigest()
    if digest == UPSTREAM:
        body = body.replace(b'    int flags;\n', b'    int flags = 1;\n', 1)
        if hashlib.sha256(body).hexdigest() != PREPARED:
            raise ValueError('unexpected patched memtier source')
        path.write_bytes(body)
    elif digest != PREPARED:
        raise ValueError('unexpected memtier source; refusing an unverified patch')
    report = {'version': '2.5.1', 'source_file': path.name,
              'source_url': 'https://github.com/redis/memtier_benchmark/blob/2.5.1/shard_connection.cpp#L392-L425',
              'upstream_sha256': UPSTREAM, 'prepared_sha256': PREPARED,
              'change': 'Initialize flags=1 before SO_KEEPALIVE and TCP_NODELAY; later fcntl assigns its own flags.',
              'limitation': 'Source preparation is not a runtime socket assertion; the matched workflow checks setsockopt using strace.'}
    (root / 'keel-preparation.json').write_text(json.dumps(report, indent=2)+'\n')
    return report


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('source', type=Path)
    args = parser.parse_args()
    print(json.dumps(prepare(args.source)))
