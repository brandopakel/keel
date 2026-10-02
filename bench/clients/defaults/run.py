#!/usr/bin/env python3
"""Run client libraries with their default settings against Keel.

The pinned matrix in bench/clients configures every library for Keel: RESP2,
no client name, non-transactional pipelines. This probe does the opposite. Each
library gets only a host, a port and, in the authenticated arm, a password, and
then does what an application does first: a SET and a GET, the same with a
client name set, a pipeline, the library's transaction API, and its quit().

What each library sends on its own is the point. redis-py 8 and node-redis 6
open every connection with HELLO 3 and have no fallback; ioredis 6 sends HELLO 3
with the password in it; all of them send CLIENT SETINFO. Every connection runs
through a logging proxy so the handshake is on record next to the outcome.

The outcomes are compared with expected.json, and any difference fails the run:
a scenario that used to pass and now fails is a regression, and one that used
to fail and now passes means expected.json has to say so. Run with
--update-expected after a change that is meant to move the table. With
--redis-server, the same probes also run against Redis, as the reference.
"""
import argparse
import json
import os
import socket
import subprocess
import sys
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
PASSWORD = "probe-password"  # a public synthetic fixture


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def wait_listening(port, proc, what):
    deadline = time.time() + 10
    while time.time() < deadline:
        if proc.poll() is not None:
            sys.exit(f"{what} exited with {proc.returncode} before listening")
        try:
            socket.create_connection(("127.0.0.1", port), timeout=0.2).close()
            return
        except OSError:
            time.sleep(0.05)
    sys.exit(f"{what} did not listen on {port}")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--bin", required=True, help="Keel binary")
    ap.add_argument("--go-probe", required=True, help="binary built from go/")
    ap.add_argument("--node-dir", required=True, help="node/ with its locked dependencies installed")
    ap.add_argument("--python", action="append", required=True, help="a Python with one redis-py version installed; repeat")
    ap.add_argument("--redis-server", help="also run every probe against this redis-server, as the reference")
    ap.add_argument("--expected", default=str(HERE / "expected.json"))
    ap.add_argument("--update-expected", action="store_true")
    ap.add_argument("--out", required=True)
    args = ap.parse_args()
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=False)

    arms = [("keel-noauth", None), ("keel-auth", PASSWORD)]
    if args.redis_server:
        arms += [("redis-noauth", None), ("redis-auth", PASSWORD)]
    procs, results = [], []
    try:
        for arm, password in arms:
            port, front = free_port(), free_port()
            log = open(out / f"{arm}.log", "w")
            if arm.startswith("keel"):
                cmd = [args.bin, "-port", str(port), "-maxmemory", "64mb"]
                env = dict(os.environ)
                if password:
                    cmd += ["-requirepass-env", "KEEL_PROBE_PASSWORD"]
                    env["KEEL_PROBE_PASSWORD"] = password
            else:
                cmd = [args.redis_server, "--port", str(port), "--save", "", "--appendonly", "no"]
                env = dict(os.environ)
                if password:
                    cmd += ["--requirepass", password]
            server = subprocess.Popen(cmd, stdout=log, stderr=subprocess.STDOUT, env=env)
            procs.append(server)
            wait_listening(port, server, arm)
            proxy = subprocess.Popen([sys.executable, str(HERE / "proxy.py"), str(front), str(port), str(out / f"wire-{arm}.log")])
            procs.append(proxy)
            wait_listening(front, proxy, f"proxy for {arm}")

            env = dict(os.environ, PROBE_PASSWORD=password or "")
            probes = [[args.go_probe, f"127.0.0.1:{front}", arm]]
            probes += [[py, str(HERE / "probe.py"), str(front), arm] for py in args.python]
            probes += [["node", str(Path(args.node_dir) / "probe.mjs"), str(front), arm]]
            for probe in probes:
                run = subprocess.run(probe, capture_output=True, text=True, env=env, timeout=120,
                                     cwd=args.node_dir if probe[0] == "node" else None)
                with open(out / "probe-stderr.log", "a") as f:
                    f.write(f"$ {' '.join(probe)}\n{run.stderr}")
                if run.returncode != 0:
                    sys.exit(f"probe {probe} exited with {run.returncode}; see probe-stderr.log")
                results += [json.loads(line) for line in run.stdout.splitlines() if line.strip()]
    finally:
        for p in reversed(procs):
            p.terminate()
        for p in procs:
            try:
                p.wait(timeout=5)
            except subprocess.TimeoutExpired:
                p.kill()

    with open(out / "results.jsonl", "w") as f:
        for r in results:
            f.write(json.dumps(r) + "\n")
    table = {}
    for r in results:
        table.setdefault(r["server"], {}).setdefault(r["library"], {})[r["scenario"]] = "ok" if r["ok"] else "fail"
    (out / "summary.json").write_text(json.dumps(table, indent=2, sort_keys=True) + "\n")
    for arm in table:
        print(arm)
        for lib, scenarios in sorted(table[arm].items()):
            print(f"  {lib:20s} " + "  ".join(f"{s}:{v}" for s, v in sorted(scenarios.items())))

    observed = {arm: table.get(arm, {}) for arm, _ in arms if arm.startswith("keel")}
    if args.update_expected:
        Path(args.expected).write_text(json.dumps(observed, indent=2, sort_keys=True) + "\n")
        print(f"wrote {args.expected}")
        return
    expected = json.loads(Path(args.expected).read_text())
    errors = {(r["server"], r["library"], r["scenario"]): r["error"] for r in results if not r["ok"]}
    problems = []
    for arm in sorted(set(expected) | set(observed)):
        libs = set(expected.get(arm, {})) | set(observed.get(arm, {}))
        for lib in sorted(libs):
            want, got = expected.get(arm, {}).get(lib, {}), observed.get(arm, {}).get(lib, {})
            for scen in sorted(set(want) | set(got)):
                w, g = want.get(scen, "absent"), got.get(scen, "absent")
                if w == g:
                    continue
                if w == "ok":
                    problems.append(f"REGRESSION {arm} {lib} {scen}: {errors.get((arm, lib, scen))}")
                else:
                    problems.append(f"CHANGED {arm} {lib} {scen}: expected {w}, got {g}; update expected.json if intended")
    for p in problems:
        print(p)
    sys.exit(1 if problems else 0)


if __name__ == "__main__":
    main()
