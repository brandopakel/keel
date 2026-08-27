#!/usr/bin/env bash
# Off-loopback measurement using a veth pair across network namespaces.
#
# Every other number in this directory is loopback, which skips the real driver
# path, has an enormous MTU, and never drops or reorders. A veth pair between
# two namespaces still shares a kernel, but traffic traverses an actual network
# device with a 1500-byte MTU and real queueing, so it exercises the paths that
# loopback elides - which is where per-syscall costs and Nagle-style
# interactions look different.
#
# Linux only, requires root for the namespace setup.
set -uo pipefail
BIN="${BIN:?set BIN}"; OUT="${OUT:-bench/results/offloopback.csv}"
NS=benchsrv; SRV_IP=10.200.0.2; CLI_IP=10.200.0.1

setup() {
  sudo ip netns del $NS 2>/dev/null || true
  sudo ip link del veth0 2>/dev/null || true
  sudo ip netns add $NS
  sudo ip link add veth0 type veth peer name veth1
  sudo ip link set veth1 netns $NS
  sudo ip addr add ${CLI_IP}/24 dev veth0
  sudo ip link set veth0 up
  sudo ip netns exec $NS ip addr add ${SRV_IP}/24 dev veth1
  sudo ip netns exec $NS ip link set veth1 up
  sudo ip netns exec $NS ip link set lo up
  echo "  veth pair up: client ${CLI_IP} <-> server ${SRV_IP} (MTU $(cat /sys/class/net/veth0/mtu))"
}
teardown() { sudo ip netns del $NS 2>/dev/null || true; sudo ip link del veth0 2>/dev/null || true; }

MISSING=""
setup
echo "server,command,conns,pipeline,datasize,rep,rps,p50_ms" > "$OUT"
PORT=8300
for srv in ${SERVERS:-redis kqueue-nobuf kqueue net net-small net-direct net-chan}; do
  PORT=$((PORT+1))
  if [ "$srv" = redis ]; then
    # protected-mode refuses non-loopback clients without a password, which is
    # exactly what this test is: a client reaching the server across a veth pair.
    sudo ip netns exec $NS redis-server --port $PORT --bind $SRV_IP \
      --protected-mode no --save '' --appendonly no >/tmp/ol-redis.log 2>&1 &
  else
    sudo ip netns exec $NS "$BIN" -host $SRV_IP -port $PORT -mode "$srv" >/tmp/ol-$srv.log 2>&1 &
  fi
  ok=no
  for _ in $(seq 1 80); do
    [ "$(redis-cli -h $SRV_IP -p $PORT ping 2>/dev/null)" = "PONG" ] && { ok=yes; break; }
    sleep 0.15
  done
  if [ "$ok" != yes ]; then
    echo "  FAILED: $srv never bound on ${SRV_IP}:${PORT}"
    echo "  --- server log ---"; sudo cat /tmp/ol-$srv.log 2>/dev/null | tail -20
    echo "  --- namespace state ---"; sudo ip netns exec $NS ss -lntp 2>/dev/null | head
    MISSING="$MISSING $srv"
    continue
  fi
  echo "  $srv on ${SRV_IP}:${PORT}"
  for spec in "PING 50 1 3 -t ping_mbulk" "PING 50 16 3 -t ping_mbulk" "SET 50 1 512 -t set -d 512" "SET 50 1 32768 -t set -d 32768"; do
    set -- $spec; label=$1; c=$2; pl=$3; d=$4; shift 4
    for rep in 1 2 3 4 5; do
      line=$(redis-benchmark -h $SRV_IP -p $PORT -n 30000 -c $c -P $pl -q "$@" 2>/dev/null | tr '\r' '\n' | grep -m1 -o '[0-9.]\+ requests per second.*')
      rps=$(echo "$line" | grep -o '^[0-9.]\+'); p50=$(echo "$line" | grep -o 'p50=[0-9.]\+' | cut -d= -f2)
      [ -z "$rps" ] && rps=0 && p50=0
      echo "$srv,$label,$c,$pl,$d,$rep,$rps,$p50" >> "$OUT"
    done
  done
  sudo pkill -f "$BIN" 2>/dev/null; sudo pkill -f redis-server 2>/dev/null; sleep 1
done
teardown
if [ -n "$MISSING" ]; then
  echo "INCOMPLETE: no data for:$MISSING"
  exit 1
fi
echo "done -> $OUT (all servers measured)"
