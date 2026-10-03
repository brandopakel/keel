#!/usr/bin/env python3
"""Compare paired Go benchmark runs of a baseline and a candidate.

Each input file holds the output of several `go test -bench -benchmem` runs,
one per round. Before each run the workflow writes two configuration lines, in
the `key: value` form of Go's benchmark format:

    keel-round: 3
    keel-first: candidate

A round runs both binaries back to back, and the order alternates from round to
round (baseline then candidate, candidate then baseline, and so on: ABBA). Each
candidate sample is paired with the baseline sample of its own round, the run
next to it in time, so drift between rounds cancels out of each ratio. Half the
pairs run the candidate first, so any cost of running second falls on both
sides equally. The comparison checks that the orders balance, and reports the
size of that cost as the order effect.

For every benchmark this reports the median of the paired ns/op ratios
(candidate over baseline), a 95% bootstrap interval for it that resamples
rounds within each order, and the range of the pairs. With an even number of
pairs the median is the geometric midpoint of the middle two, so a pure order
effect cancels exactly. allocs/op, B/op and every other metric a benchmark
reports, such as evictions/op, are shown for both sides.

Time is reported, not enforced. The measured noise floor of this job, and the
reasoning for leaving time ungated, are in the header of
.github/workflows/command-path.yml. The repo's rule for a change on the command
path is a median paired ratio of at least 0.98 in matched runs. A ratio above
1/0.98 is marked so that a reviewer can weigh it against its interval.

The exit status is 1, with every reason listed, when:
- a line is neither a benchmark result, a `key: value` line nor PASS; this
  includes a failed benchmark, its log, and a result that does not parse;
- the rounds are malformed: not numbered 1..n, a round on one side only, the
  sides disagreeing on which ran first, or unbalanced orders;
- a benchmark is missing from one side, or from one round of a side, or
  appears twice in a round, so that the sample counts differ;
- a metric is reported on one side and not the other, or allocs/op is
  missing (allocs/op is the gate);
- the candidate allocates more per operation than the baseline;
- evictions/op leaves its sane range on either side, or differs between them;
- the GOMAXPROCS suffix differs between the sides, or nothing is compared.
"""
import argparse
from collections import defaultdict
import json
import math
import random
import re
import statistics
import sys

# Go's benchmark format: a configuration line's key starts with a lower-case
# letter and has no spaces or upper-case letters. Neither it nor a result line
# is indented; a benchmark's log output is.
CONFIG = re.compile(r'^([a-z][^\sA-Z:]*):(?:\s+(.*?))?\s*$')
SUFFIX = re.compile(r'^(.*)-(\d+)$')
ORDERS = ('baseline', 'candidate')
BUDGET = 1 / 0.98
RESAMPLES = 2000
SEED = 88

# Metrics whose value is a property of the work done rather than of the
# machine. Every timed write in BenchmarkCommandPathUnderEviction adds a key of
# the same size to a full budget, so a working evictor removes about one key
# per write. Under half means that the timed path is not the eviction path (the
# benchmark's own floor). Over one and a half is more than the writes and the
# preloaded keys can account for, so the counter is broken. Between the sides,
# the median may move by 5% at most. A larger move means the two sides did
# different work, and their times are not comparable.
SANE = {'evictions/op': {'low': 0.5, 'high': 1.5, 'max_change': 0.05}}


class Run:
    """One side's samples: name -> round -> {unit: value}."""

    def __init__(self, label):
        self.label = label
        self.first = {}                   # round -> 'baseline' or 'candidate'
        self.samples = defaultdict(dict)
        self.order = []                   # names in order of first appearance
        self.procs = None
        self.problems = []


def parse_result(fields):
    """The metrics of a result line, or the reason it does not parse."""
    if len(fields) < 4 or len(fields) % 2:
        return None, 'result does not parse'
    if not fields[1].isdigit():
        return None, f'iteration count {fields[1]!r} is not a number'
    metrics = {}
    for value, unit in zip(fields[2::2], fields[3::2]):
        try:
            number = float(value)
        except ValueError:
            return None, f'{value!r} before {unit} is not a number'
        if not math.isfinite(number) or unit in metrics:
            return None, f'{unit} is repeated or not finite'
        metrics[unit] = number
    if metrics.get('ns/op', 0) <= 0:
        return None, 'no positive ns/op'
    return metrics, None


def parse(text, label):
    run = Run(label)
    current = None
    raw = []                              # (name as printed, round, metrics)
    seen = set()
    for number, line in enumerate(text.splitlines(), 1):
        s = line.rstrip()
        where = f'{label} line {number}'
        if not s or s == 'PASS' or re.match(r'^ok\s', s):
            continue
        if s.startswith('Benchmark'):
            fields = s.split()
            metrics, problem = parse_result(fields)
            if problem:
                run.problems.append(f'{where}: {problem}: {s!r}')
            elif current is None:
                run.problems.append(f'{where}: result before any keel-round line: {s!r}')
            elif (fields[0], current) in seen:
                run.problems.append(f'{where}: {fields[0]} appears twice in round {current}')
            else:
                seen.add((fields[0], current))
                raw.append((fields[0], current, metrics))
            continue
        m = CONFIG.match(s)
        if not m:
            run.problems.append(f'{where}: unexpected line {s.strip()!r}')
            continue
        key, value = m.group(1), m.group(2) or ''
        if key == 'keel-round':
            if not value.isdigit() or int(value) in run.first:
                run.problems.append(f'{where}: round {value!r} is not a new round number')
                current = None
                continue
            current = int(value)
            run.first[current] = None
        elif key == 'keel-first':
            if current is None or run.first[current] is not None or value not in ORDERS:
                run.problems.append(f'{where}: keel-first {value!r} does not follow a new keel-round line')
                continue
            run.first[current] = value
        # Any other key (goos, goarch, pkg, cpu) is the run's configuration.
    for r, first in sorted(run.first.items()):
        if first is None:
            run.problems.append(f'{label}: round {r} has no keel-first line')

    # Go appends -GOMAXPROCS to every name unless it is 1. A suffix shared by
    # every result is therefore the processor count. Without a shared suffix,
    # names such as MGET-10 keep their number.
    suffixes = {SUFFIX.match(name).group(2) if SUFFIX.match(name) else None for name, _, _ in raw}
    strip = len(suffixes) == 1 and None not in suffixes
    run.procs = int(next(iter(suffixes))) if strip else 1
    for name, r, metrics in raw:
        name = SUFFIX.match(name).group(1) if strip else name
        if name not in run.samples:
            run.order.append(name)
        run.samples[name][r] = metrics
    return run


def gmedian(values):
    """The median on a log scale: the plain median of an odd count, and the
    geometric midpoint of the middle two of an even one."""
    return math.exp(statistics.median(math.log(v) for v in values))


def quantile(sorted_values, q):
    position = q * (len(sorted_values) - 1)
    low = math.floor(position)
    high = min(low + 1, len(sorted_values) - 1)
    return sorted_values[low] + (sorted_values[high] - sorted_values[low]) * (position - low)


def order_effect(pairs):
    """How much slower the second run of a pair is than the first. Pairs that
    ran the baseline first carry the effect in their ratio, and pairs that ran
    the candidate first carry its inverse."""
    second = [ratio for _, first, ratio in pairs if first == 'baseline']
    first = [ratio for _, first, ratio in pairs if first == 'candidate']
    if not second or not first:
        return None
    return math.sqrt(gmedian(second) / gmedian(first))


def check_rounds(base, cand, failures):
    """The rounds both sides share and agree on, by which side ran first."""
    rounds = sorted(set(base.first) | set(cand.first))
    for r in rounds:
        if r not in base.first or r not in cand.first:
            failures.append(f"round {r} has no {'candidate' if r not in cand.first else 'baseline'} run")
        elif base.first[r] != cand.first[r]:
            failures.append(f'round {r}: baseline says {base.first[r]} ran first, candidate says {cand.first[r]}')
    if rounds and rounds != list(range(1, len(rounds) + 1)):
        failures.append(f'rounds are not numbered 1..{len(rounds)}: {rounds}')
    valid = [r for r in rounds if r in base.first and r in cand.first
             and base.first[r] is not None and base.first[r] == cand.first[r]]
    by_order = {o: [r for r in valid if base.first[r] == o] for o in ORDERS}
    if valid and len(by_order['baseline']) != len(by_order['candidate']):
        # With every round in one order, a cost of running second would land
        # on one side alone and read as a difference between the two.
        failures.append(f"orders are unbalanced: {len(by_order['baseline'])} rounds ran the baseline first "
                        f"and {len(by_order['candidate'])} the candidate first")
    return valid, by_order


def check_metrics(name, b, c, row, failures):
    """Show every metric for both sides, and check the gated and sane ones."""
    units = defaultdict(list)
    for side, samples in (('baseline', b), ('candidate', c)):
        for r, metrics in samples.items():
            units[frozenset(metrics)].append(f'{side} round {r}')
    every = set().union(*units)
    if len(units) > 1:
        gaps = sorted(f"{', '.join(sorted(every - u))} absent from {where[0]}"
                      for u, where in units.items() if every - u)
        failures.append(f'{name}: the two sides report different metrics: {"; ".join(gaps)}')
    for unit in sorted(every - {'ns/op'}):
        bv = [m[unit] for m in b.values() if unit in m]
        cv = [m[unit] for m in c.values() if unit in m]
        if bv:
            row['base_metrics'][unit] = statistics.median(bv)
        if cv:
            row['cand_metrics'][unit] = statistics.median(cv)
        sane = SANE.get(unit)
        if not sane:
            continue
        for side, values in (('baseline', bv), ('candidate', cv)):
            outside = [v for v in values if not sane['low'] <= v <= sane['high']]
            if outside:
                failures.append(f"{name}: {side} {unit} {', '.join(f'{v:g}' for v in outside)} "
                                f"outside {sane['low']:g} to {sane['high']:g}")
        if bv and cv:
            mb, mc = statistics.median(bv), statistics.median(cv)
            if abs(mc - mb) > sane['max_change'] * mb:
                failures.append(f'{name}: {unit} {mb:g} -> {mc:g}: the two sides did different work, '
                                'so their times are not comparable')
    allocs = row['base_metrics'].get('allocs/op'), row['cand_metrics'].get('allocs/op')
    if None in allocs:
        failures.append(f'{name}: no allocs/op on both sides to gate; run with -benchmem')
    elif allocs[1] > allocs[0]:
        failures.append(f'{name}: {allocs[0]:g} -> {allocs[1]:g} allocs/op')


def compare(base, cand, resamples=RESAMPLES, seed=SEED):
    failures = list(base.problems) + list(cand.problems)
    valid, by_order = check_rounds(base, cand, failures)
    if base.samples and cand.samples and base.procs != cand.procs:
        failures.append(f'baseline ran with GOMAXPROCS {base.procs}, candidate with {cand.procs}')

    only = {'baseline': [n for n in base.order if n not in cand.samples],
            'candidate': [n for n in cand.order if n not in base.samples]}
    for n in only['baseline']:
        failures.append(f'{n}: in the baseline only; a removed or renamed benchmark breaks the comparison')
    for n in only['candidate']:
        failures.append(f'{n}: in the candidate only, so it has no baseline to compare with')

    rows = []
    for name in (n for n in base.order if n in cand.samples):
        b, c = base.samples[name], cand.samples[name]
        for side, samples in (('baseline', b), ('candidate', c)):
            missing = [r for r in valid if r not in samples]
            if missing:
                failures.append(f'{name}: {len(b)} baseline samples but {len(c)} candidate samples; '
                                f'no {side} sample in round {", ".join(map(str, missing))}')
        pairs = [(r, base.first[r], c[r]['ns/op'] / b[r]['ns/op']) for r in valid if r in b and r in c]
        if not pairs:
            continue
        ratios = [ratio for _, _, ratio in pairs]
        effect = order_effect(pairs)
        row = {'name': name, 'pairs': len(pairs),
               'median_ratio': round(gmedian(ratios), 4),
               'pair_range': [round(min(ratios), 4), round(max(ratios), 4)],
               'order_effect': None if effect is None else round(effect, 4),
               'base_ns': round(statistics.median(m['ns/op'] for m in b.values()), 2),
               'cand_ns': round(statistics.median(m['ns/op'] for m in c.values()), 2),
               'base_metrics': {}, 'cand_metrics': {},
               'pair_ratios': [{'round': r, 'first': f, 'ratio': round(x, 5)} for r, f, x in pairs]}
        check_metrics(name, b, c, row, failures)
        rows.append(row)

    if not rows:
        # A candidate that produced no results would otherwise pass, having
        # measured nothing.
        failures.append('no benchmark is present on both sides')
    return {'rounds': {'count': len(valid), 'baseline_first': by_order['baseline'],
                       'candidate_first': by_order['candidate']},
            'procs': cand.procs, 'rows': rows, 'overall': summarise(rows, by_order, resamples, seed),
            'one_side_only': only, 'failures': failures}


def summarise(rows, by_order, resamples, seed):
    """Bootstrap intervals. Each draw takes rounds with replacement within each
    order, which keeps the design balanced. It recomputes every row's median,
    and the median across rows, from the same rounds, since the rows of a round
    come from one process and share its luck."""
    if not rows:
        return None
    ratio = {row['name']: {p['round']: p['ratio'] for p in row['pair_ratios']} for row in rows}
    rng = random.Random(seed)
    draws = {name: [] for name in ratio}
    overall = []
    for _ in range(resamples):
        drawn = []
        for o in ORDERS:
            drawn += [rng.choice(by_order[o]) for _ in by_order[o]]
        medians = []
        for name, by_round in ratio.items():
            sample = [by_round[r] for r in drawn if r in by_round]
            if sample:
                draws[name].append(gmedian(sample))
                medians.append(draws[name][-1])
        overall.append(gmedian(medians))
    for row in rows:
        d = sorted(draws[row['name']])
        row['interval_95'] = [round(quantile(d, .025), 4), round(quantile(d, .975), 4)]
    overall.sort()
    effects = [row['order_effect'] for row in rows if row['order_effect'] is not None]
    medians = [row['median_ratio'] for row in rows]
    return {'median_ratio': round(gmedian(medians), 4),
            'interval_95': [round(quantile(overall, .025), 4), round(quantile(overall, .975), 4)],
            'row_range': [min(medians), max(medians)],
            'order_effect': round(gmedian(effects), 4) if effects else None,
            'resamples': resamples}


def fmt(value):
    return '-' if value is None else f'{value:g}'


def markdown(result):
    out = ['| Benchmark | Pairs | Baseline ns/op | Candidate ns/op | Median paired ratio | 95% interval '
           '| Pair range | Allocs/op | B/op | Other metrics |',
           '| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |']
    for r in result['rows']:
        def sides(unit):
            return f"{fmt(r['base_metrics'].get(unit))} -> {fmt(r['cand_metrics'].get(unit))}"
        other = ', '.join(f'{u} {sides(u)}' for u in sorted(set(r['base_metrics']) | set(r['cand_metrics']))
                          if u not in ('allocs/op', 'B/op'))
        mark = ' (over the 0.98 budget)' if r['median_ratio'] > BUDGET else ''
        out.append(f"| {r['name']} | {r['pairs']} | {r['base_ns']:g} | {r['cand_ns']:g} "
                   f"| {r['median_ratio']:.3f}{mark} | {r['interval_95'][0]:.3f} to {r['interval_95'][1]:.3f} "
                   f"| {r['pair_range'][0]:.3f} to {r['pair_range'][1]:.3f} | {sides('allocs/op')} "
                   f"| {sides('B/op')} | {other} |")
    o, rounds = result['overall'], result['rounds']
    if o:
        effect = ('Measuring the order effect needs rounds in both orders.' if o['order_effect'] is None else
                  f"Running second rather than first scales time by {o['order_effect']:.3f}, and the "
                  'alternating order cancels that.')
        out += ['', f"Across {len(result['rows'])} benchmarks: median paired ratio {o['median_ratio']:.3f}, "
                f"95% interval {o['interval_95'][0]:.3f} to {o['interval_95'][1]:.3f}. Benchmark medians "
                f"range from {o['row_range'][0]:.3f} to {o['row_range'][1]:.3f}. {effect}"]
    out += ['', f"{rounds['count']} rounds: {len(rounds['baseline_first'])} ran the baseline first and "
            f"{len(rounds['candidate_first'])} the candidate first. Each ratio pairs a candidate run with the "
            "baseline run next to it, and the intervals resample rounds within each order. Time is reported, "
            "not gated; allocations are gated."]
    if result['failures']:
        out += ['', '**FAILED**', '']
        out += [f'- {f}' for f in result['failures']]
    return '\n'.join(out)


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('baseline')
    ap.add_argument('candidate')
    ap.add_argument('--json', help='write the comparison here as well')
    args = ap.parse_args(argv)
    with open(args.baseline) as b, open(args.candidate) as c:
        result = compare(parse(b.read(), 'baseline'), parse(c.read(), 'candidate'))
    print(markdown(result))
    if args.json:
        with open(args.json, 'w') as f:
            json.dump(result, f, indent=2)
    return 1 if result['failures'] else 0


if __name__ == '__main__':
    sys.exit(main())
