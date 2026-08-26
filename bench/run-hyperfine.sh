#!/usr/bin/env bash
# hyperfine pass: measures the noise floor of each configuration separately.
#
# A single global noise figure is not enough - spread varies with concurrency,
# so a delta that is significant at c=1 may be noise at c=1000. This produces a
# per-configuration floor to check every other reading against.
set -uo pipefail
BIN="${BIN:?set BIN}"; OUT="${OUT:-bench/results/hyperfine}"; mkdir -p "$OUT"
PORT=${PORT_BASE:-12000}
SRV_PORT=""
start() {  # sets SRV_PORT; must NOT be called in a subshell
  local kind=$1
  PORT=$((PORT+1)); local p=$PORT; SRV_PORT=$p
  if [ "$kind" = redis ]; then redis-server --port "$p" --save '' --appendonly no >/dev/null 2>&1 &
  else "$BIN" -port "$p" -mode "$kind" >/dev/null 2>&1 & fi
  for _ in $(seq 1 50); do redis-cli -p "$p" ping >/dev/null 2>&1 && break; perl -e 'select(undef,undef,undef,0.1)'; done
  local owner comm
  owner=$(lsof -ti:$p 2>/dev/null | head -1); comm=$(ps -o comm= -p "$owner" 2>/dev/null)
  case "$kind" in
    redis) echo "$comm" | grep -q redis-server || { echo "FATAL :$p owned by $comm" >&2; exit 1; } ;;
    *)     echo "$comm" | grep -q "$(basename "$BIN")" || { echo "FATAL :$p owned by $comm" >&2; exit 1; } ;;
  esac
}
stop() { pkill -f "$BIN" 2>/dev/null; pkill -f "redis-server" 2>/dev/null; perl -e 'select(undef,undef,undef,0.4)'; }

for srv in ${SERVERS:-redis kqueue kqueue-wbuf net}; do
  start "$srv"; p=$SRV_PORT
  for c in 1 50 1000; do
    hyperfine --warmup 2 --runs 10 --export-json "$OUT/${srv}-c${c}.json" \
      "redis-benchmark -h 127.0.0.1 -p $p -t ping_mbulk -n 20000 -c $c -P 1 -q" >/dev/null 2>&1
  done
  stop
  echo "  $srv done"
done
python3 - "$OUT" <<'PY'
import json,glob,os,sys
d=sys.argv[1]
print("\n| server | conns | mean ms | stddev | RSD | min detectable (2σ) |")
print("|---|---|---|---|---|---|")
for f in sorted(glob.glob(os.path.join(d,"*.json"))):
    r=json.load(open(f))["results"][0]
    srv,c=os.path.basename(f)[:-5].rsplit("-c",1)
    rsd=r["stddev"]/r["mean"]*100
    print(f'| {srv} | {c} | {r["mean"]*1000:.1f} | {r["stddev"]*1000:.1f} | {rsd:.2f}% | {2*rsd:.2f}% |')
PY
