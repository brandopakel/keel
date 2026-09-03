#!/usr/bin/env bash
# I/O threads sweep: how much does moving reads, parsing and writes off the loop
# thread buy, and where?
#
# Every run is the same server, -mode kqueue, differing only in -io-threads.
# 1 is the default and the baseline: everything on the event loop thread, which
# is the configuration every other result in this directory was measured under.
#
# The suites are chosen around where threading can and cannot help. Command
# execution is never threaded, so a workload dominated by execution has nothing
# to gain and only the barrier to lose; a workload dominated by copying bytes is
# where the extra cores are. Hence the concurrency sweep (does it need enough
# ready connections to be worth a handoff?), the size sweep (is the win in the
# copying?) and the pipeline sweep (does coalescing already do the job?).
set -uo pipefail

BIN="${BIN:?set BIN to the keel binary}"
OUT="${OUT:-bench/results/iothreads.csv}"
REPS="${REPS:-5}"
PORT=${PORT_BASE:-13000}
THREADS="${THREADS:-1 2 4 8}"

echo "server,suite,command,conns,pipeline,datasize,rep,rps,p50_ms" > "$OUT"

SRV_PID=""
SRV_PORT=""

# start_server verifies that the process answering on the port is the one just
# launched. A failed bind leaves the process alive with no listener, so without
# this check the sweep would silently measure whichever server got there first -
# the mistake documented in README.md that made four implementations agree to
# within 2% and read as a finding.
start_server() { # $1=io-threads ; sets SRV_PORT and SRV_PID
  local threads=$1
  PORT=$((PORT+1))
  SRV_PORT=$PORT
  "$BIN" -port "$SRV_PORT" -mode kqueue -io-threads "$threads" \
      >"/tmp/bench-iothreads-$threads.log" 2>&1 &
  SRV_PID=$!
  local ready=no
  for _ in $(seq 1 80); do
    if [ "$(redis-cli -h 127.0.0.1 -p "$SRV_PORT" ping 2>/dev/null)" = "PONG" ]; then ready=yes; break; fi
    perl -e 'select(undef,undef,undef,0.1)'
  done
  [ "$ready" = yes ] || { echo "FATAL: io-threads=$threads never bound :$SRV_PORT" >&2; exit 1; }
  local owner; owner=$(lsof -ti:"$SRV_PORT" 2>/dev/null | head -1)
  local comm;  comm=$(ps -o comm= -p "$owner" 2>/dev/null)
  echo "$comm" | grep -q "$(basename "$BIN")" \
    || { echo "FATAL: :$SRV_PORT owned by $comm, expected $(basename "$BIN")" >&2; exit 1; }
  echo "    [io-threads=$threads on :$SRV_PORT pid=$owner verified]" >&2
}

stop_servers() {
  [ -n "$SRV_PID" ] && kill "$SRV_PID" 2>/dev/null
  pkill -f "$BIN" 2>/dev/null
  SRV_PID=""; SRV_PORT=""
  perl -e 'select(undef,undef,undef,0.6)'
}

# run_bench <label-server> <suite> <port> <command-label> <n> <conns> <pipe> <datasize> <args...>
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

for t in $THREADS; do
  srv="iothreads-$t"
  echo ">>> $srv"

  # --- Suite 1: concurrency. A phase is only distributed once enough
  # connections are ready at the same time, so this is where the threshold
  # shows up: below it, threading is barrier cost and nothing else.
  start_server "$t"; p=$SRV_PORT
  for c in 1 10 50 200 1000; do
    run_bench "$srv" conc "$p" PING 50000 "$c" 1 3 -t ping_mbulk
  done
  stop_servers

  # --- Suite 2: value size. Large values are where one core's copying was the
  # wall, and so where the extra threads have the most to move.
  start_server "$t"; p=$SRV_PORT
  for d in 8 512 8192 65536 262144; do
    run_bench "$srv" size "$p" SET 20000 50 1 "$d" -t set -d "$d"
    run_bench "$srv" size "$p" GET 20000 50 1 "$d" -t get -d "$d"
  done
  stop_servers

  # --- Suite 3: pipeline depth. Coalescing already removed the syscall ceiling
  # here, so this asks whether there is anything left for threads to take.
  start_server "$t"; p=$SRV_PORT
  for pl in 1 8 64; do
    run_bench "$srv" pipe "$p" PING 200000 50 "$pl" 3 -t ping_mbulk
  done
  stop_servers
done
echo "done -> $OUT"
