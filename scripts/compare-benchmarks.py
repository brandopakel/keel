#!/usr/bin/env python3
"""Compare paired Go benchmark runs of a baseline and a candidate.

Each input file holds the output of several `go test -bench -benchmem` runs.
The two files must come from runs interleaved one for one (baseline, candidate,
baseline, ...), so run i of one is paired with run i of the other and machine
drift between pairs cancels out of each ratio. For every benchmark present in
both, this reports the median of the paired ns/op ratios and the median
allocs/op on each side.

Time is reported, not enforced: a shared hosted runner moves by more than the
few percent a change might cost, and a gate on it would fail at random. The
repo's rule for a change on the command path is a median paired ratio of at
least 0.98 in matched runs, which this prints for a reviewer to apply.
Allocations are deterministic, so a candidate allocating more per operation
than its baseline fails the comparison, as do unequal run counts for a
benchmark and having no benchmark in common at all.
"""
import argparse
import json
import re
import statistics
import sys
from collections import defaultdict

LINE = re.compile(r'^(Benchmark\S+?)(?:-\d+)?\s+\d+\s+([\d.]+) ns/op(?:\s+([\d.]+) B/op\s+([\d.]+) allocs/op)?')


def runs(path):
    """Benchmark name -> list of (ns/op, allocs/op), one entry per run."""
    out = defaultdict(list)
    for line in open(path):
        m = LINE.match(line.strip())
        if m:
            allocs = float(m.group(4)) if m.group(4) is not None else None
            out[m.group(1)].append((float(m.group(2)), allocs))
    return out


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('baseline')
    ap.add_argument('candidate')
    ap.add_argument('--json', help='write the comparison here as well')
    args = ap.parse_args()
    base, cand = runs(args.baseline), runs(args.candidate)
    rows, failures = [], []
    for name in sorted(set(base) & set(cand)):
        if len(base[name]) != len(cand[name]):
            # A run missing from the middle would shift every later pair onto
            # the wrong baseline run, so unequal counts are refused, not zipped.
            failures.append(f"{name}: {len(base[name])} baseline runs but {len(cand[name])} candidate runs")
            continue
        pairs = list(zip(base[name], cand[name]))
        ratio = statistics.median(c[0] / b[0] for b, c in pairs)
        base_allocs = [b[1] for b, _ in pairs if b[1] is not None]
        cand_allocs = [c[1] for _, c in pairs if c[1] is not None]
        row = {'name': name, 'pairs': len(pairs), 'median_ratio': round(ratio, 4),
               'base_ns': round(statistics.median(b[0] for b, _ in pairs), 2),
               'cand_ns': round(statistics.median(c[0] for _, c in pairs), 2)}
        if base_allocs and cand_allocs:
            row['base_allocs'] = statistics.median(base_allocs)
            row['cand_allocs'] = statistics.median(cand_allocs)
            if row['cand_allocs'] > row['base_allocs']:
                failures.append(f"{name}: {row['base_allocs']:g} -> {row['cand_allocs']:g} allocs/op")
        rows.append(row)
    only = sorted(set(base) ^ set(cand))
    if not rows:
        # A candidate that produced no results would otherwise pass, having
        # measured nothing.
        failures.append('no benchmark is present on both sides')

    print('| Benchmark | Pairs | Baseline ns/op | Candidate ns/op | Median paired ratio | Allocs/op |')
    print('| --- | --- | --- | --- | --- | --- |')
    for r in rows:
        allocs = f"{r['base_allocs']:g} -> {r['cand_allocs']:g}" if 'base_allocs' in r else 'n/a'
        mark = ' (more than 2% slower)' if r['median_ratio'] > 1 / 0.98 else ''
        print(f"| {r['name']} | {r['pairs']} | {r['base_ns']} | {r['cand_ns']} | {r['median_ratio']}{mark} | {allocs} |")
    if only:
        print(f"\nIn one side only, not compared: {', '.join(only)}")
    for f in failures:
        print(f"\nFAILED {f}")
    if args.json:
        with open(args.json, 'w') as f:
            json.dump({'rows': rows, 'one_side_only': only, 'failures': failures}, f, indent=2)
    sys.exit(1 if failures else 0)


if __name__ == '__main__':
    main()
