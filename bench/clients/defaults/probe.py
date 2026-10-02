# Default-configuration probe for redis-py: only host, port and password are set.
import json, os, sys
import redis

port, server = int(sys.argv[1]), sys.argv[2]
pw = os.environ.get("PROBE_PASSWORD") or None
lib = f"redis-py {redis.__version__}"
for scen in ["default", "named", "pipeline", "tx", "close"]:
    key = f"probe:{lib}:{scen}"
    kw = dict(host="127.0.0.1", port=port, password=pw, socket_timeout=3, socket_connect_timeout=3)
    if scen == "named":
        kw["client_name"] = "probe"
    try:
        r = redis.Redis(**kw)
        if scen in ("default", "named", "close"):
            r.set(key, "v")
            assert r.get(key) == b"v", "GET mismatch"
        elif scen == "pipeline":
            p = r.pipeline(transaction=False); p.set(key, "v"); p.get(key)
            assert p.execute()[1] == b"v", "GET mismatch"
        else:
            p = r.pipeline(); p.set(key, "v"); p.get(key)  # library default: transaction=True
            assert p.execute()[1] == b"v", "GET mismatch"
        if scen == "close":
            r.quit() if hasattr(r, "quit") else r.close()
        else:
            r.close()
        ok, err = True, None
    except Exception as e:
        ok, err = False, f"{type(e).__name__}: {e}"
    print(json.dumps({"library": lib, "server": server, "scenario": scen, "ok": ok, "error": err}), flush=True)
