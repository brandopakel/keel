#!/usr/bin/env bash
# Separate-host RESP benchmark: server on a Windows machine in WSL2
# (Ubuntu-24.04, mirrored networking) reached over SSH, memtier on this host.
# Needs /opt/keel/keel and redis-server in the distro, and inbound TCP
# 7379/7380 allowed. Usage: HOST=... KEY=... SSH_USER=... bench/run-separate-host-wsl.sh
# Keel and Redis get identical workloads, persistence and client placement,
# a fresh process and empty data directory per repeat, interleaved order.
set -uo pipefail
HOST=${HOST:?server tailnet address}; KEY=${KEY:?ssh key for the server}; OUT=${OUT:-$PWD/results}; REPS=${REPS:-3}; SECS=${SECS:-30}
mkdir -p "$OUT/raw"; PASS=$(openssl rand -hex 16)
SSH=(ssh -i "$KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ServerAliveInterval=15 "${SSH_USER:?Windows account}@$HOST")
csv=$OUT/summary.csv; [ -f "$csv" ] || echo "server,persistence,pipeline,rep,ops_sec,p50_ms,p99_ms,p999_ms,errors" > "$csv"

server_cmd() { # srv persistence
  local aof=no fs=everysec; [ "$2" = everysec ] && aof=yes
  if [ "$1" = keel ]; then
    local ao=false; [ $aof = yes ] && ao=true
    echo "cd /tmp/bench && KP=$PASS exec timeout $((SECS+60)) /opt/keel/keel -host 0.0.0.0 -port 7379 -requirepass-env KP -appendonly=$ao -appendfsync $fs"
  else
    echo "cd /tmp/bench && exec timeout $((SECS+60)) redis-server --bind 0.0.0.0 --port 7380 --protected-mode no --requirepass $PASS --appendonly $aof --appendfsync $fs --save '' --dir /tmp/bench"
  fi
}
port() { [ "$1" = keel ] && echo 7379 || echo 7380; }

for persistence in none everysec; do
 for pl in 1 16; do
  for rep in $(seq 1 "$REPS"); do
   for srv in keel redis; do
    [ $((rep % 2)) = 0 ] && srv=$([ $srv = keel ] && echo redis || echo keel)  # alternate who goes first
    p=$(port $srv); tag=$srv-$persistence-p$pl-$rep
    printf '%s\n' "pkill -f 'keel -host|redis-server'; sleep 0.5; rm -rf /tmp/bench && mkdir -p /tmp/bench && $(server_cmd $srv $persistence)" \
      | "${SSH[@]}" "wsl -d Ubuntu-24.04 -u root -- bash -s" > "$OUT/raw/$tag.server.log" 2>&1 &
    sp=$!
    for i in $(seq 1 60); do nc -z -G 1 $HOST $p 2>/dev/null && break; sleep 0.5; done
    memtier_benchmark -s $HOST -p $p -a $PASS -P redis -t 4 -c 25 --test-time=$SECS \
      --ratio=1:10 --data-size=256 --key-pattern=R:R --key-maximum=100000 --pipeline=$pl \
      --hide-histogram --json-out-file="$OUT/raw/$tag.json" > "$OUT/raw/$tag.log" 2>&1
    python3 - "$OUT/raw/$tag.json" $srv $persistence $pl $rep "$csv" <<'PY'
import json,sys
j,srv,per,pl,rep,out=sys.argv[1:7]
try: d=json.load(open(j))["ALL STATS"]["Totals"]
except Exception as e: open(out,"a").write(f"{srv},{per},{pl},{rep},FAILED,,,,{e!r}\n"); sys.exit()
pc=d.get("Percentile Latencies",{})
g=lambda t: next((float(v) for k,v in pc.items() if k.startswith("p") and float(k[1:])==t), float("nan"))
open(out,"a").write(f"{srv},{per},{pl},{rep},{float(d['Ops/sec']):.0f},{g(50):.3f},{g(99):.3f},{g(99.9):.3f},{d.get('Errors',0) if isinstance(d.get('Errors',0),(int,float)) else 0}\n")
PY
    printf '%s\n' "pkill -f 'keel -host|redis-server'" | "${SSH[@]}" "wsl -d Ubuntu-24.04 -u root -- bash -s" >/dev/null 2>&1
    wait $sp 2>/dev/null
    tail -1 "$csv"
   done
  done
 done
done
