#!/usr/bin/env bash
# A/B two keel binaries over the same suites.
#
# Written to answer one question: the event loop was restructured into read /
# execute / write phases so that I/O threads could exist, and that restructuring
# also changed the path taken when threading is off - which is the default, and
# the configuration every other result in this directory was measured under. A
# feature that is off by default must cost nothing when it is off, and nothing
# else here measures that.
#
# Both servers run at once and the arms alternate rep by rep. Running all of A
# and then all of B leaves the comparison exposed to anything that drifts over
# the length of the run - thermal state, background work, page cache - and a few
# percent is exactly the size of effect that drift produces. An idle server is
# parked in kqueue and costs nothing, so keeping both up is free.
set -uo pipefail

BIN_A="${BIN_A:?set BIN_A to the baseline binary}"
BIN_B="${BIN_B:?set BIN_B to the candidate binary}"
LABEL_A="${LABEL_A:-A}"
LABEL_B="${LABEL_B:-B}"
ARGS_A="${ARGS_A:--mode kqueue}"
ARGS_B="${ARGS_B:--mode kqueue}"
OUT="${OUT:-bench/results/ab.csv}"
REPS="${REPS:-5}"
PORT=${PORT_BASE:-17000}

echo "server,suite,command,conns,pipeline,datasize,rep,rps,p50_ms" > "$OUT"

SRV_PIDS=""
SRV_PORT=""

# start_one sets SRV_PORT rather than echoing it.
#
# It echoed it once, and the caller read it with p=$(start_one ...). Command
# substitution runs in a subshell, so PORT=$((PORT+1)) advanced a copy and the
# parent's counter never moved: both arms were handed the same port, the second
# server failed to bind, and every one of its readings came back zero. The exit
# 1 below could not help either, because it exited the subshell and left the
# script running. This is the first failure documented in README.md, reproduced
# in a new script twelve years of hindsight later - which is why the ownership
# assertion is worth keeping even when the port logic looks obviously fine.
start_one() { # $1=binary $2=args $3=label ; sets SRV_PORT
  local bin=$1 args=$2 label=$3
  PORT=$((PORT+1))
  local p=$PORT
  SRV_PORT=$p
  # shellcheck disable=SC2086
  "$bin" -port "$p" $args >"/tmp/ab-$label.log" 2>&1 &
  local pid=$!
  SRV_PIDS="$SRV_PIDS $pid"
  local ready=no
  for _ in $(seq 1 80); do
    if [ "$(redis-cli -h 127.0.0.1 -p "$p" ping 2>/dev/null)" = "PONG" ]; then ready=yes; break; fi
    perl -e 'select(undef,undef,undef,0.1)'
  done
  [ "$ready" = yes ] || { echo "FATAL: $label never bound :$p" >&2; exit 1; }
  # The process answering on this port must be exactly the one just launched.
  #
  # Comparing process names was not enough. It compared them by substring, so
  # "memkv" matched a running "memkv-base" and an arm that had failed to bind
  # was accepted as verified - which is how a whole A/B run came to measure one
  # binary against itself and report the reassuring answer. The pid is the thing
  # that cannot be coincidentally right.
  local owner; owner=$(lsof -ti:"$p" 2>/dev/null | head -1)
  [ "$owner" = "$pid" ] \
    || { echo "FATAL: :$p owned by pid $owner, expected the $label just started (pid $pid)" >&2; exit 1; }
  echo "    [$label on :$p pid=$owner verified]" >&2
}

bench_once() { # $1=port ; args...
  local p=$1; shift
  redis-benchmark -h 127.0.0.1 -p "$p" -q "$@" 2>/dev/null | tr '\r' '\n' \
    | grep -m1 -o '[0-9.]\+ requests per second.*'
}

# pair <suite> <label> <conns> <pipe> <datasize> <benchmark args...>
pair() {
  local suite=$1 label=$2 c=$3 pl=$4 d=$5; shift 5
  for rep in $(seq 1 "$REPS"); do
    for arm in A B; do
      local p lbl line rps p50
      if [ "$arm" = A ]; then p=$PORT_A; lbl=$LABEL_A; else p=$PORT_B; lbl=$LABEL_B; fi
      line=$(bench_once "$p" "$@")
      rps=$(echo "$line" | grep -o '^[0-9.]\+')
      p50=$(echo "$line" | grep -o 'p50=[0-9.]\+' | cut -d= -f2)
      [ -z "$rps" ] && rps=0 && p50=0
      echo "$lbl,$suite,$label,$c,$pl,$d,$rep,$rps,$p50" >> "$OUT"
    done
  done
}

start_one "$BIN_A" "$ARGS_A" "$LABEL_A"; PORT_A=$SRV_PORT
start_one "$BIN_B" "$ARGS_B" "$LABEL_B"; PORT_B=$SRV_PORT
[ "$PORT_A" != "$PORT_B" ] || { echo "FATAL: both arms on :$PORT_A" >&2; exit 1; }
# shellcheck disable=SC2064
trap "kill $SRV_PIDS 2>/dev/null" EXIT

# Concurrency: the phased loop reads every ready connection before executing any
# of them, where the old one finished each connection before looking at the
# next. If that batching costs anything, more ready connections is where.
for c in 1 10 50 200 1000; do
  pair conc PING "$c" 1 3 -n 50000 -c "$c" -P 1 -t ping_mbulk
done

# Pipeline depth: a batch of several commands is now coalesced into a shared
# arena rather than a fresh buffer per batch. Deeper pipelines, bigger batches.
for pl in 1 8 64; do
  pair pipe PING 50 "$pl" 3 -n 200000 -c 50 -P "$pl" -t ping_mbulk
done

# Value size: a batch of one keeps its reply by reference in both trees, so this
# is the check that the bypass really was preserved and a large value is not
# being copied through the arena.
for d in 8 512 8192 65536 262144; do
  n=20000; [ "$d" -ge 65536 ] && n=8000
  pair size SET 50 1 "$d" -n "$n" -c 50 -P 1 -t set -d "$d"
  pair size GET 50 1 "$d" -n "$n" -c 50 -P 1 -t get -d "$d"
done

echo "done -> $OUT"
