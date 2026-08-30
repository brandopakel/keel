#!/usr/bin/env python3
"""Reduce an A/B CSV from run-ab.sh to medians and a paired ratio.

Two numbers per scenario, because they answer different questions.

`B/A medians` is the ratio of the two medians, the same reduction the rest of
this directory uses.

`paired` is the median of the per-rep ratios. run-ab.sh alternates the arms rep
by rep, so rep i of A and rep i of B ran within a second of each other under the
same conditions. Dividing them first cancels whatever drifted between the start
and the end of the run, which for a few-percent effect is most of what would
otherwise be called the result. Where the two columns disagree, the paired one
is measuring the change and the unpaired one is measuring the afternoon.
"""
import csv, sys, statistics as st
from collections import defaultdict

path = sys.argv[1] if len(sys.argv) > 1 else "bench/results/ab.csv"
rows = list(csv.DictReader(open(path)))
if not rows:
    sys.exit("no rows")

arms = []
for r in rows:
    if r["server"] not in arms:
        arms.append(r["server"])
if len(arms) != 2:
    sys.exit(f"expected exactly two arms, got {arms}")
A, B = arms

byrep = defaultdict(dict)
for r in rows:
    try:
        v = float(r["rps"])
    except ValueError:
        continue
    if v > 0:
        scenario = (r["suite"], r["command"], r["conns"], r["pipeline"], r["datasize"])
        byrep[(scenario, r["rep"])][r["server"]] = v

scenarios = sorted(
    {s for s, _ in byrep},
    key=lambda k: (k[0], k[1], int(k[4]), int(k[2]), int(k[3])),
)

print(f"# {B} against {A} ({len(rows)} raw runs)\n")
print("`spread` is (max-min)/median of the paired ratios: how much the two arms")
print("disagreed rep to rep. A ratio whose spread swamps its distance from 1.00")
print("is not a result.\n")
print(f"| scenario | {A} | {B} | B/A medians | paired | spread |")
print("|---|---|---|---|---|---|")

worst = None
for sc in scenarios:
    a_vals, b_vals, ratios = [], [], []
    for (s, _), arm in byrep.items():
        if s != sc:
            continue
        if A in arm and B in arm:
            a_vals.append(arm[A])
            b_vals.append(arm[B])
            ratios.append(arm[B] / arm[A])
    if not ratios:
        continue
    ma, mb, mr = st.median(a_vals), st.median(b_vals), st.median(ratios)
    spread = (max(ratios) - min(ratios)) / mr * 100
    label = f"`{sc[0]}` {sc[1]} c={sc[2]} P={sc[3]} d={sc[4]}"
    flag = "**" if abs(mr - 1) > 0.05 and spread < abs(mr - 1) * 100 else ""
    print(f"| {label} | {ma:,.0f} | {mb:,.0f} | {mb/ma:.3f}x | {flag}{mr:.3f}x{flag} | ±{spread:.0f}% |")
    if worst is None or mr < worst[0]:
        worst = (mr, label, spread)

if worst:
    print(f"\nWorst paired ratio: {worst[0]:.3f}x on {worst[1]} (spread ±{worst[2]:.0f}%)")
