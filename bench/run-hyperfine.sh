#!/usr/bin/env bash
# Per-configuration noise floors.
#
# A single global noise figure is not enough: spread varies with concurrency, so
# a delta that is significant at c=1 can be noise at c=50.
#
# Two hazards this script has to work around, both learned the hard way:
#
#   Ports. A run at c=1000 opens 1000 sockets, and TIME_WAIT holds each for
#   ~15s against an ephemeral range of only ~16k. Back-to-back runs exhaust the
#   range, redis-benchmark then fails with "Cannot assign requested address",
#   and hyperfine aborts leaving an empty JSON. So: drain before every
#   configuration, and scale the run count to the socket budget.
#
#   Identity. A server that fails to bind stays alive with no listener (a failed
#   bind returns from RunAsyncTCPServer while main blocks on wg.Wait()), so a
#   stale server on the same port answers the readiness probe and gets measured
#   under the wrong label. So: assert via lsof that the process owning the port
#   is the one just started.
set -uo pipefail
BIN="${BIN:?set BIN}"; OUT="${OUT:-bench/results/hyperfine}"; mkdir -p "$OUT"
PORT=${PORT_BASE:-30000}
DRAIN_BELOW=${DRAIN_BELOW:-1200}

drain_ports() {
  for _ in $(seq 1 60); do
    local tw; tw=$(netstat -an 2>/dev/null | grep -c TIME_WAIT)
    [ "$tw" -lt "$DRAIN_BELOW" ] && { echo "      (TIME_WAIT=$tw)"; return; }
    perl -e 'select(undef,undef,undef,5)'
  done
  echo "      (WARNING: ports still busy)"
}

# runs_for <conns> -- keep total sockets well inside the ephemeral range
runs_for() { case "$1" in 1|10) echo 10 ;; 50|200) echo 10 ;; *) echo 6 ;; esac; }

for srv in ${SERVERS:-redis kqueue kqueue-wbuf net}; do
  for c in 1 50 1000; do
    f="$OUT/${srv}-c${c}.json"
    runs=$(runs_for "$c")
    echo "  $srv c=$c (${runs} runs)"
    drain_ports
    pkill -f "$BIN" 2>/dev/null; pkill -f "redis-server" 2>/dev/null
    perl -e 'select(undef,undef,undef,1)'
    PORT=$((PORT+1))
    if [ "$srv" = redis ]; then
      redis-server --port "$PORT" --save '' --appendonly no >/dev/null 2>&1 &
    else
      "$BIN" -port "$PORT" -mode "$srv" >/dev/null 2>&1 &
    fi
    ready=no
    for _ in $(seq 1 80); do
      [ "$(redis-cli -p "$PORT" ping 2>/dev/null)" = "PONG" ] && { ready=yes; break; }
      perl -e 'select(undef,undef,undef,0.1)'
    done
    [ "$ready" = yes ] || { echo "      FATAL: $srv never bound :$PORT"; continue; }
    owner=$(lsof -ti:"$PORT" 2>/dev/null | head -1)
    comm=$(ps -o comm= -p "$owner" 2>/dev/null)
    case "$srv" in
      redis) echo "$comm" | grep -q redis-server || { echo "      FATAL: :$PORT owned by $comm"; continue; } ;;
      *)     echo "$comm" | grep -q "$(basename "$BIN")" || { echo "      FATAL: :$PORT owned by $comm"; continue; } ;;
    esac
    # sanity probe before spending 10 timed runs on it
    probe=$(redis-benchmark -h 127.0.0.1 -p "$PORT" -t ping_mbulk -n 20000 -c "$c" -P 1 -q 2>/dev/null \
              | tr '\r' '\n' | grep -o '[0-9.]* requests per second' | tail -1)
    [ -z "$probe" ] && { echo "      FATAL: probe produced nothing"; continue; }
    hyperfine --warmup 1 --runs "$runs" --export-json "$f" \
      "redis-benchmark -h 127.0.0.1 -p $PORT -t ping_mbulk -n 20000 -c $c -P 1 -q" >/dev/null 2>&1
    sz=$(wc -c < "$f" 2>/dev/null | tr -d ' ')
    echo "      probe=$probe -> ${sz} bytes"
    pkill -f "$BIN" 2>/dev/null; pkill -f "redis-server" 2>/dev/null
  done
done
echo "done -> $OUT"
