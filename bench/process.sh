# Shared process ownership for local benchmarks. Source this file.
SRV_PID=""
stop_owned() {
  if [ -n "$SRV_PID" ]; then
    kill "$SRV_PID" 2>/dev/null || true
    wait "$SRV_PID" 2>/dev/null || true
    SRV_PID=""
  fi
}
trap stop_owned EXIT
verify_owned() {
  local port=$1 ready=no owner
  for _ in $(seq 1 80); do
    kill -0 "$SRV_PID" 2>/dev/null || { echo "server exited" >&2; exit 1; }
    if [ "$(redis-cli -h 127.0.0.1 -p "$port" ping 2>/dev/null)" = PONG ]; then ready=yes; break; fi
    sleep .1
  done
  [ "$ready" = yes ] || { echo "server readiness timed out" >&2; exit 1; }
  owner=$(lsof -nP -tiTCP:"$port" -sTCP:LISTEN)
  [ "$owner" = "$SRV_PID" ] || { echo "listener PID mismatch: $owner != $SRV_PID" >&2; exit 1; }
}
