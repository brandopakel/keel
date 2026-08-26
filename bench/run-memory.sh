#!/usr/bin/env bash
# Memory under connection load. This is the axis the upstream issue predicts the
# event loop loses on ("increased heap memory consumption") and the axis where
# goroutine-per-connection genuinely costs something: each connection carries a
# goroutine stack plus two bufio buffers, where the event loop carries a slice entry.
set -uo pipefail
BIN="${BIN:?}"; OUT="${OUT:-bench/results/memory.csv}"; PORT=${PORT_BASE:-13000}
echo "server,conns,rss_kb,goroutine_cost_kb_per_conn" > "$OUT"
for srv in ${SERVERS:-redis kqueue kqueue-wbuf net net-small net-direct net-chan}; do
  for n in 0 100 500 1000 5000; do
    p=$((++PORT))
    if [ "$srv" = redis ]; then redis-server --port $p --save '' --appendonly no >/dev/null 2>&1 &
    else "$BIN" -port $p -mode "$srv" >/dev/null 2>&1 & fi
    for _ in $(seq 1 60); do redis-cli -p $p ping >/dev/null 2>&1 && break; perl -e 'select(undef,undef,undef,0.1)'; done
    pid=$(lsof -ti:$p 2>/dev/null | head -1)
    base=$(ps -o rss= -p "$pid" 2>/dev/null | tr -d ' ')
    if [ "$n" -gt 0 ]; then
      python3 - "$p" "$n" <<'PY'
import socket,sys,time
p,n=int(sys.argv[1]),int(sys.argv[2]); socks=[]
for _ in range(n):
    try:
        s=socket.create_connection(("127.0.0.1",p),timeout=5); s.sendall(b"*1\r\n$4\r\nPING\r\n"); s.recv(64); socks.append(s)
    except Exception: break
time.sleep(2)
print(len(socks))
PY
    fi
    rss=$(ps -o rss= -p "$pid" 2>/dev/null | tr -d ' ')
    [ -z "$rss" ] && rss=0
    delta=0
    [ "$n" -gt 0 ] && [ -n "$base" ] && delta=$(python3 -c "print(f'{($rss-$base)/$n:.2f}')" 2>/dev/null || echo 0)
    echo "$srv,$n,$rss,$delta" >> "$OUT"
    pkill -f "$BIN" 2>/dev/null; pkill -f "redis-server --port 1" 2>/dev/null; perl -e 'select(undef,undef,undef,0.5)'
  done
  echo "  $srv done"
done
