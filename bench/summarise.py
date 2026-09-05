#!/usr/bin/env python3
"""Reduce the raw matrix CSV to medians and print comparison tables.

Medians, not means: throughput runs are right-skewed and a single slow run
should not move the number. Spread is reported as (max-min)/median so a reading
can be checked against the measured noise floor before it is believed.
"""
import csv, sys, statistics as st
from collections import defaultdict

path = sys.argv[1] if len(sys.argv) > 1 else "bench/results/matrix.csv"
rows = list(csv.DictReader(open(path)))
if not rows:
    sys.exit("no rows")

ORDER = ["redis", "kqueue-nobuf", "kqueue", "net"]
LABEL = {"redis": "redis-server", "kqueue-nobuf": "A1 loop", "kqueue": "A2 loop+wbuf", "net": "B net.Conn"}

agg = defaultdict(list)
for r in rows:
    try:
        v = float(r["rps"])
    except ValueError:
        sys.exit("invalid throughput row; refusing a partial summary")
    if v >= 0:
        agg[(r["server"], r["suite"], r["command"], r["conns"], r["pipeline"], r["datasize"])].append(v)

def med(k):
    return st.median(agg[k]) if agg.get(k) else None

def spread(k):
    v = agg.get(k)
    if not v or len(v) < 2:
        return None
    m = st.median(v)
    return (max(v) - min(v)) / m * 100 if m else None

def table(suite, axis_idx, axis_name, fixed_label=None):
    keys = [k for k in agg if k[1] == suite]
    if not keys:
        return
    axis = sorted({k[axis_idx] for k in keys}, key=lambda x: float(x))
    cmds = sorted({k[2] for k in keys})
    for cmd in cmds:
        sub = [k for k in keys if k[2] == cmd]
        if not sub:
            continue
        print(f"\n### {suite} — {cmd}  ({axis_name} sweep)\n")
        hdr = f"| {axis_name} | " + " | ".join(LABEL[s] for s in ORDER if any(k[0] == s for k in sub)) + " |"
        print(hdr)
        print("|" + "---|" * (hdr.count("|") - 1))
        for a in axis:
            cells = []
            for s in ORDER:
                match = [k for k in sub if k[0] == s and k[axis_idx] == a]
                if not match:
                    continue
                m = med(match[0]); sp = spread(match[0])
                cells.append(f"{m:,.0f}" + (f" ±{sp:.0f}%" if sp and sp > 13 else ""))
            if cells:
                print(f"| {a} | " + " | ".join(cells) + " |")

print(f"# Benchmark medians ({len(rows)} raw runs)\n")
print("Reps per cell:", len(next(iter(agg.values()))))
print("\n`±` marks cells whose spread exceeds the ~13% noise floor — treat those as indicative only.")

table("conc", 3, "conns")
table("pipe", 4, "pipeline")
table("size", 5, "value bytes")

# command table is a plain comparison, not a sweep
cmdkeys = [k for k in agg if k[1] == "cmd"]
if cmdkeys:
    print("\n### command types (c=50, P=1)\n")
    cmds = sorted({k[2] for k in cmdkeys})
    servers = [s for s in ORDER if any(k[0] == s for k in cmdkeys)]
    print("| command | " + " | ".join(LABEL[s] for s in servers) + " |")
    print("|" + "---|" * (len(servers) + 1))
    for c in cmds:
        cells = []
        for s in servers:
            match = [k for k in cmdkeys if k[0] == s and k[2] == c]
            cells.append(f"{med(match[0]):,.0f}" if match and med(match[0]) is not None else "—")
        print(f"| {c} | " + " | ".join(cells) + " |")

# the headline comparison the issue asks for
print("\n### The comparison the issue asks for: A2 vs B\n")
print("Same framing, same write policy, same execution semantics — differing only")
print("in whether I/O readiness comes from hand-rolled epoll/kqueue or the Go netpoller.\n")
print("| scenario | A2 loop+wbuf | B net.Conn | B/A2 |")
print("|---|---|---|---|")
for k in sorted([k for k in agg if k[0] == "kqueue"], key=lambda k: (k[1], float(k[3]), float(k[4]), float(k[5]))):
    bk = ("net",) + k[1:]
    a, b = med(k), med(bk)
    if a and b:
        scen = f"{k[2]} c={k[3]} P={k[4]} d={k[5]}"
        print(f"| {scen} | {a:,.0f} | {b:,.0f} | {b/a:.2f}x |")
