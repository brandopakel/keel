#!/usr/bin/env bash
# Four-way benchmark: redis-server vs the three MemKV I/O implementations.
#
#   redis        real redis-server, as a calibration reference
#   kqueue       event loop, one write syscall per reply     (A1, current design)
#   kqueue-wbuf  event loop, replies coalesced per read      (A2)
#   net          net.Listener, goroutine per connection      (B)
#
# A1 vs A2 isolates write buffering. A2 vs B isolates the I/O mechanism, which
# is the comparison the upstream issue actually asks for.
set -uo pipefail

BIN="${BIN:?set BIN to the memkv binary}"
OUT="${OUT:-bench/results/matrix.csv}"
REPS="${REPS:-5}"
PORT=${PORT_BASE:-10000}

echo "server,suite,command,conns,pipeline,datasize,rep,rps,p50_ms" > "$OUT"

# Ports are assigned by mutating a global directly. An earlier version used
# p=$(next_port), which runs in a subshell: the parent's counter never advanced,
# every server tried the same port, and because a failed bind leaves the process
# alive with no listener, the benchmark silently measured whichever server had
# got there first. Every reading was wrong and they all agreed with each other,
# which is exactly what made it hard to spot.
SRV_PID=""
SRV_PORT=""

start_server() { # $1=kind ; sets SRV_PORT and SRV_PID
  local kind=$1
  PORT=$((PORT+1))
  SRV_PORT=$PORT
  if [ "$kind" = redis ]; then
    redis-server --port "$SRV_PORT" --save '' --appendonly no --daemonize no >/tmp/bench-redis.log 2>&1 &
  else
    "$BIN" -port "$SRV_PORT" -mode "$kind" >"/tmp/bench-$kind.log" 2>&1 &
  fi
  SRV_PID=$!
  local ready=no
  for _ in $(seq 1 80); do
    if [ "$(redis-cli -h 127.0.0.1 -p "$SRV_PORT" ping 2>/dev/null)" = "PONG" ]; then ready=yes; break; fi
    perl -e 'select(undef,undef,undef,0.1)'
  done
  [ "$ready" = yes ] || { echo "FATAL: $kind never bound :$SRV_PORT" >&2; exit 1; }
  # Assert the process answering on this port is the one we just started, so a
  # stale server from an earlier iteration can never be measured under the wrong label.
  local owner; owner=$(lsof -ti:"$SRV_PORT" 2>/dev/null | head -1)
  local comm;  comm=$(ps -o comm= -p "$owner" 2>/dev/null)
  case "$kind" in
    redis) echo "$comm" | grep -q redis-server || { echo "FATAL: :$SRV_PORT owned by $comm, expected redis-server" >&2; exit 1; } ;;
    *)     echo "$comm" | grep -q "$(basename "$BIN")" || { echo "FATAL: :$SRV_PORT owned by $comm, expected $(basename "$BIN")" >&2; exit 1; } ;;
  esac
  echo "    [$kind on :$SRV_PORT pid=$owner verified]" >&2
}

stop_servers() {
  [ -n "$SRV_PID" ] && kill "$SRV_PID" 2>/dev/null
  pkill -f "$BIN" 2>/dev/null
  pkill -f "redis-server" 2>/dev/null
  SRV_PID=""; SRV_PORT=""
  perl -e 'select(undef,undef,undef,0.6)'
}

# run_bench <server> <suite> <port> <label> <n> <conns> <pipe> <datasize> <args...>
run_bench() {
  local srv=$1 suite=$2 p=$3 label=$4 n=$5 c=$6 pl=$7 d=$8; shift 8
  for rep in $(seq 1 "$REPS"); do
    local line rps p50
    line=$(redis-benchmark -h 127.0.0.1 -p "$p" -n "$n" -c "$c" -P "$pl" -q "$@" 2>/dev/null \
             | tr '\r' '\n' | grep -m1 -o '[0-9.]\+ requests per second.*')
    rps=$(echo "$line" | grep -o '^[0-9.]\+')
    p50=$(echo "$line" | grep -o 'p50=[0-9.]\+' | cut -d= -f2)
    [ -z "$rps" ] && rps=0 && p50=0
    echo "$srv,$suite,$label,$c,$pl,$d,$rep,$rps,$p50" >> "$OUT"
  done
}

SERVERS="${SERVERS:-redis kqueue kqueue-wbuf net net-small net-direct net-chan}"

for srv in $SERVERS; do
  echo ">>> $srv"

  # --- Suite 1: concurrency sweep (isolates connection scaling) ---
  start_server "$srv"; p=$SRV_PORT
  for c in 1 10 50 200 1000; do
    run_bench "$srv" conc "$p" PING 50000 "$c" 1 3 -t ping_mbulk
  done
  stop_servers

  # --- Suite 2: pipeline sweep (isolates syscall amortisation) ---
  start_server "$srv"; p=$SRV_PORT
  for pl in 1 8 16 64; do
    run_bench "$srv" pipe "$p" PING 200000 50 "$pl" 3 -t ping_mbulk
  done
  stop_servers

  # --- Suite 3: value-size sweep (isolates payload cost) ---
  start_server "$srv"; p=$SRV_PORT
  for d in 8 32 128 512 2048 8192 32768; do
    run_bench "$srv" size "$p" SET 30000 50 1 "$d" -t set -d "$d"
  done
  stop_servers

  # --- Suite 4: command types (isolates data-structure cost from I/O) ---
  start_server "$srv"; p=$SRV_PORT
  run_bench "$srv" cmd "$p" PING   50000 50 1 3 -t ping_mbulk
  run_bench "$srv" cmd "$p" SET    50000 50 1 32 -t set -d 32
  run_bench "$srv" cmd "$p" GET    50000 50 1 32 -t get -d 32
  run_bench "$srv" cmd "$p" INCR   50000 50 1 3 -t incr
  run_bench "$srv" cmd "$p" SADD   50000 50 1 3 sadd myset "element:__rand_int__"
  run_bench "$srv" cmd "$p" ZADD   50000 50 1 3 zadd myzset 1.0 "member:__rand_int__"
  run_bench "$srv" cmd "$p" GEOADD 50000 50 1 3 geoadd geo 13.361389 38.115556 "p:__rand_int__"
  stop_servers
done
echo "done -> $OUT"
