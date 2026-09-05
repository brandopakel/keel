#!/usr/bin/env bash
# memtier_benchmark pass: randomised keys and payloads, which redis-benchmark
# does not do. Fixed-size payloads and sequential keys can flatter an allocator
# and a hash table; random sizes and access patterns are the more honest test.
set -euo pipefail
source "$(dirname "$0")/process.sh"
BIN="${BIN:?set BIN}"; OUT="${OUT:-bench/results/memtier.csv}"; REPS="${REPS:-3}"
PORT=${PORT_BASE:-11000}
RAW="${OUT%.csv}-raw"
mkdir -p "$RAW"
memtier_benchmark --version > "$RAW/tool-version.txt" 2>&1
echo "server,scenario,keypattern,datasize,pipeline,rep,ops_sec,p50_ms,p99_ms,p999_ms" > "$OUT"

SRV_PORT=""
start() {  # sets SRV_PORT; must NOT be called in a subshell
  local kind=$1
  PORT=$((PORT+1)); local p=$PORT; SRV_PORT=$p
  if [ "$kind" = redis ]; then redis-server --port "$p" --save '' --appendonly no >/dev/null 2>&1 &
  else "$BIN" -port "$p" -mode "$kind" >"/tmp/mt-$kind.log" 2>&1 & fi
  SRV_PID=$!
  verify_owned "$p"
}
stop() { stop_owned; }

# scenario: label | extra memtier args
run() { # srv scenario kp ds pl port args...
  local srv=$1 scen=$2 kp=$3 ds=$4 pl=$5 p=$6; shift 6
  for rep in $(seq 1 "$REPS"); do
    local j
    j="$RAW/$srv-$scen-$rep.json"
    memtier_benchmark -s 127.0.0.1 -p "$p" -P redis \
      -t 4 -c 25 --test-time=6 --hide-histogram --json-out-file="$j" \
      --pipeline="$pl" --key-pattern="$kp" "$@" >"$RAW/$srv-$scen-$rep.log" 2>&1
    python3 - "$j" "$srv" "$scen" "$kp" "$ds" "$pl" "$rep" "$OUT" <<'PY'
import json,sys
j,srv,scen,kp,ds,pl,rep,out = sys.argv[1:9]
try: d=json.load(open(j))["ALL STATS"]["Totals"]
except Exception as exc: sys.exit(f"invalid memtier output: {exc}")
def percentile(target):
    # Current memtier stores percentile values in a nested object. Older
    # versions may expose the human-readable column names at the top level.
    for key, value in d.get("Percentile Latencies", {}).items():
        if key.startswith("p"):
            try: matched = float(key[1:]) == target
            except ValueError: continue
            if matched: return float(value)
    return float(d[f"p{target:g} Latency"])
try:
    rate=float(d["Ops/sec"])
    if rate<=0: raise ValueError("nonpositive throughput")
    row=[srv,scen,kp,ds,pl,rep,f'{rate:.2f}',
         f'{percentile(50):.3f}',f'{percentile(99):.3f}',f'{percentile(99.9):.3f}']
except (KeyError,TypeError,ValueError) as exc:
    sys.exit(f"missing or invalid memtier metrics: {exc}")
open(out,"a").write(",".join(map(str,row))+"\n")
PY
  done
}

for srv in ${SERVERS:-redis kqueue-nobuf kqueue net net-small net-direct net-chan}; do
  echo ">>> memtier $srv"
  start "$srv"; p=$SRV_PORT
  # random keys, fixed small payload
  run "$srv" uniform      "R:R" 32          1 "$p" --ratio=1:10 -d 32
  # random keys, RANDOM payload sizes across three orders of magnitude
  run "$srv" randomsize   "R:R" "8-16384"   1 "$p" --ratio=1:10 --data-size-range=8-16384 --data-size-pattern=R --random-data
  # gaussian key access - models a hot subset, stresses the dict differently
  run "$srv" gaussian     "G:G" 32          1 "$p" --ratio=1:10 -d 32 --key-stddev=100
  # write-heavy
  run "$srv" writeheavy   "R:R" 128         1 "$p" --ratio=10:1 -d 128
  # pipelined mixed
  run "$srv" pipelined    "R:R" 32         16 "$p" --ratio=1:10 -d 32
  stop
done
echo "done -> $OUT"
