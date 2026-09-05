#!/usr/bin/env bash
# memtier_benchmark pass: randomised keys and payloads, which redis-benchmark
# does not do. Fixed-size payloads and sequential keys can flatter an allocator
# and a hash table; random sizes and access patterns are the more honest test.
set -euo pipefail
source "$(dirname "$0")/process.sh"
BIN="${BIN:?set BIN}"; OUT="${OUT:-bench/results/memtier.csv}"; REPS="${REPS:-3}"
PORT=${PORT_BASE:-11000}
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
    j=$(mktemp)
    memtier_benchmark -s 127.0.0.1 -p "$p" -P redis \
      -t 4 -c 25 --test-time=6 --hide-histogram --json-out-file="$j" \
      --pipeline="$pl" --key-pattern="$kp" "$@" >/dev/null 2>&1
    python3 - "$j" "$srv" "$scen" "$kp" "$ds" "$pl" "$rep" "$OUT" <<'PY'
import json,sys
j,srv,scen,kp,ds,pl,rep,out = sys.argv[1:9]
try: d=json.load(open(j))["ALL STATS"]["Totals"]
except Exception as exc: sys.exit(f"invalid memtier output: {exc}")
row=[srv,scen,kp,ds,pl,rep,f'{d.get("Ops/sec",0):.2f}',
     f'{d.get("p50 Latency",0):.3f}',f'{d.get("p99 Latency",0):.3f}',f'{d.get("p99.9 Latency",0):.3f}']
open(out,"a").write(",".join(map(str,row))+"\n")
PY
    rm -f "$j"
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
