#!/usr/bin/env python3
"""Reduce an iothreads CSV to medians and ratios against -io-threads 1.

Medians, not means, for the same reason as summarise.py: throughput runs are
right-skewed and one slow rep should not move the number. Spread is reported
where it exceeds the noise floor, because the first three-rep pass at this
sweep produced a clear regression that five reps showed to be noise - see the
"Single samples lie" entry in README.md.
"""
import csv, sys, statistics as st
from collections import defaultdict

path = sys.argv[1] if len(sys.argv) > 1 else "bench/results/iothreads-darwin.csv"
rows = list(csv.DictReader(open(path)))
if not rows:
    sys.exit("no rows")

agg = defaultdict(list)
for r in rows:
    try:
        v = float(r["rps"])
    except ValueError:
        continue
    if v > 0:
        agg[(r["server"], r["suite"], r["command"], r["conns"], r["pipeline"], r["datasize"])].append(v)

threads = sorted({k[0].split("-")[1] for k in agg}, key=int)
baseline = threads[0]


def med(k):
    return st.median(agg[k]) if agg.get(k) else None


def spread(k):
    v = agg.get(k)
    if not v or len(v) < 2:
        return None
    m = st.median(v)
    return (max(v) - min(v)) / m * 100 if m else None


print(f"# I/O threads ({len(rows)} raw runs, {len(agg[next(iter(agg))])} reps per cell)\n")
print(f"Ratios are against `-io-threads {baseline}`. `±` marks cells whose spread exceeds")
print("the ~13% noise floor; those are indicative only.\n")

scenarios = sorted(
    {k[1:] for k in agg},
    key=lambda k: (k[0], k[1], int(k[4]), int(k[2]), int(k[3])),
)

print("| scenario | " + " | ".join(f"t={t}" for t in threads) + " | " +
      " | ".join(f"x{t}" for t in threads[1:]) + " |")
print("|" + "---|" * (2 * len(threads)))
for sc in scenarios:
    base = med((f"iothreads-{baseline}",) + sc)
    if not base:
        continue
    cells, ratios = [], []
    for t in threads:
        k = (f"iothreads-{t}",) + sc
        m, sp = med(k), spread(k)
        cells.append(f"{m:,.0f}" + (f" ±{sp:.0f}%" if sp and sp > 13 else "") if m else "—")
        if t != baseline:
            ratios.append(f"**{m/base:.2f}x**" if m and m / base >= 1.15 else
                          (f"{m/base:.2f}x" if m else "—"))
    label = f"`{sc[0]}` {sc[1]} c={sc[2]} P={sc[3]} d={sc[4]}"
    print(f"| {label} | " + " | ".join(cells) + " | " + " | ".join(ratios) + " |")
