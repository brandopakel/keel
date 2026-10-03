#!/usr/bin/env python3
"""Run outside projects' own Redis-backed test suites against one arm.

Each suite in pins.json runs unmodified, at its pinned commit, with `go test`,
against a fresh server behind the wire tap on 127.0.0.1:6379; the suites find
it there because several of them hard-code that address. Exact counts come
from `go test -json`, and Ginkgo's own spec totals from its output, so a suite
that silently ran nothing cannot pass for one that ran everything.

Suites marked `gates` decide the exit status. The others exist to record, in
the same format, exactly how a feature Keel does not support fails.
"""
import argparse
import collections
import json
import os
import re
import subprocess
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import pilot_lib  # noqa: E402
from pins import PINS  # noqa: E402

# Colour codes, with or without their escape byte: test2json drops the
# non-printing ESC from Ginkgo's output and leaves the rest of the code.
ANSI = re.compile(r"\x1b?\[[0-9;]*m")
GINKGO_RAN = re.compile(r"Ran (\d+) of (\d+) Specs")
GINKGO_RESULT = re.compile(r"(\d+) Passed \| (\d+) Failed \| (\d+) Pending \| (\d+) Skipped")


def parse_go_test_json(path):
    final = {}
    output = collections.defaultdict(list)
    packages = {}
    ginkgo = []
    with Path(path).open() as events:
        for line in events:
            line = line.strip()
            if not line.startswith("{"):
                continue
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                continue
            action, pkg, test = event.get("Action"), event.get("Package", ""), event.get("Test")
            if action in ("output", "build-output"):
                text = ANSI.sub("", event.get("Output", ""))
                output[(pkg, test)].append(text)
                if m := GINKGO_RAN.search(text):
                    ginkgo.append({"package": pkg, "ran": int(m[1]), "of": int(m[2])})
                if (m := GINKGO_RESULT.search(text)) and ginkgo:
                    ginkgo[-1].update(passed=int(m[1]), failed=int(m[2]), pending=int(m[3]), skipped=int(m[4]))
            elif action in ("pass", "fail", "skip", "build-fail"):
                if test:
                    final[(pkg, test)] = action
                else:
                    packages[pkg] = action
    counts = {"top_level": collections.Counter(), "subtests": collections.Counter()}
    for (pkg, test), action in final.items():
        counts["subtests" if "/" in test else "top_level"][action] += 1
    failing = []
    for (pkg, test), action in sorted(final.items()):
        if action == "fail":
            failing.append({"package": pkg, "test": test,
                            "output_tail": "".join(output[(pkg, test)][-30:])[-4000:]})
    for pkg, action in sorted(packages.items()):
        if action != "pass" and not any(f["package"] == pkg for f in failing):
            failing.append({"package": pkg, "test": None, "result": action,
                            "output_tail": "".join(output[(pkg, None)][-40:])[-4000:]})
    return {"packages": packages, "top_level": dict(counts["top_level"]),
            "subtests": dict(counts["subtests"]), "ginkgo": ginkgo, "failing": failing}


def run_suite(suite, args):
    out = Path(args.out) / suite["name"]
    out.mkdir(parents=True, exist_ok=True)
    checkout = Path(args.checkouts) / suite["checkout"]
    pin = PINS["checkouts"][suite["checkout"]]
    server = pilot_lib.CacheServer(args.arm, args.server_bin, out / "server", maxmemory=args.maxmemory)
    tap = pilot_lib.Tap(out / "wire.jsonl")
    result = {"name": suite["name"], "repo": pin["repo"], "commit": pin["commit"],
              "packages": suite["packages"], "run": suite.get("run"), "gates": suite["gates"],
              "reason": suite.get("reason")}
    command = ["go", "test", "-count=1", "-json", "-timeout", "15m"]
    if suite.get("run"):
        command += ["-run", suite["run"]]
    command += suite["packages"]
    result["command"] = command
    began = time.monotonic()
    try:
        server.start()
        tap.start()
        with (out / "go-test.json").open("wb") as stdout, (out / "go-test.stderr").open("wb") as stderr:
            env = dict(os.environ, CI=os.environ.get("CI", "true"))
            result["exit_code"] = subprocess.run(command, cwd=checkout, stdout=stdout, stderr=stderr,
                                                 env=env).returncode
        result["info"] = pilot_lib.info_digest(server.info())
    except Exception as exc:  # recorded, never discarded
        result["harness_error"] = repr(exc)
        result.setdefault("exit_code", None)
    finally:
        tap.stop()
        result["server_exit_code"] = server.stop()
    result["seconds"] = round(time.monotonic() - began, 1)
    if (out / "go-test.json").exists():
        result.update(parse_go_test_json(out / "go-test.json"))
    if (out / "wire.jsonl").exists():
        result["wire"] = pilot_lib.summarize_wire([out / "wire.jsonl"])
    result["passed"] = suite_passed(result)
    return result


def suite_passed(result):
    """A suite passes only if go test succeeded and actually ran something.

    `go test -run` that matches nothing exits 0 with "no tests to run", and a
    pin bump that renames tests looks the same, so a zero count is a failure.
    """
    ran = sum((result.get("top_level") or {}).values())
    return result.get("exit_code") == 0 and "harness_error" not in result and ran > 0


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--arm", choices=pilot_lib.ARMS, required=True)
    ap.add_argument("--server-bin", required=True)
    ap.add_argument("--checkouts", required=True, help="directory holding the pinned checkouts")
    ap.add_argument("--out", required=True)
    ap.add_argument("--maxmemory", default="256mb")
    ap.add_argument("--suite", action="append", help="run only the named suite(s)")
    args = ap.parse_args()
    Path(args.out).mkdir(parents=True, exist_ok=True)
    selected = [s for s in PINS["suites"] if not args.suite or s["name"] in args.suite]
    server = pilot_lib.CacheServer(args.arm, args.server_bin, Path(args.out) / "probe")
    report = {"arm": args.arm, "server": {"binary_sha256": pilot_lib.sha256(args.server_bin),
                                          "version": server.version(), "maxmemory": args.maxmemory,
                                          "command": server.command()},
              "tools": pilot_lib.tool_versions(), "suites": []}
    for suite in selected:
        print(f"== {suite['name']}", flush=True)
        result = run_suite(suite, args)
        report["suites"].append(result)
        print(json.dumps({k: result.get(k) for k in ("name", "passed", "exit_code", "top_level", "subtests",
                                                      "ginkgo", "seconds")}), flush=True)
    report["passed"] = all(s["passed"] for s in report["suites"] if s["gates"])
    pilot_lib.write_json(Path(args.out) / "regression.json", report)
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    sys.exit(main())
