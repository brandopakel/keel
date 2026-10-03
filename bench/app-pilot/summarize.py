#!/usr/bin/env python3
"""Combine both arms of the application pilot into one JSON and markdown summary.

The summary carries what is needed to trust or redo a comparison: the exact
commits and binary checksums, tool versions, per-suite test counts, latency per
endpoint, memory, the command mix seen on the wire, and the restart results.
Missing inputs are reported as missing rather than skipped, because a job that
failed before writing its results is itself a result.
"""
import argparse
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from pins import PINS  # noqa: E402

ARMS = ("keel", "redis")
# Endpoints worth a row in the markdown table; every endpoint stays in the JSON.
KEY_ENDPOINTS = ("POST /user/login", "GET /user/settings", "GET /", "GET /{owner}/{repo}",
                 "GET /{owner}/{repo}/issues", "GET /{owner}/{repo}/issues/{n}",
                 "GET /{owner}/{repo}/commits/branch/main", "GET /{owner}/{repo}/issues?q=",
                 "GET /{owner}/{repo}/search?q=", "POST /{owner}/{repo}/issues/{n}/comments",
                 "POST /api/v1/repos/{owner}/{repo}/issues", "git push")


def load(path):
    try:
        return json.loads(Path(path).read_text())
    except (OSError, json.JSONDecodeError) as exc:
        return {"missing": f"{path}: {exc!r}"}


def find(root, name, arm=None):
    pattern = f"**/{name}"
    for path in sorted(Path(root).glob(pattern)):
        if arm is None or f"-{arm}" in str(path.relative_to(root)).split("/")[0]:
            return path
    return None


def kib_to_mib(kib):
    return None if kib is None else round(kib / 1024, 1)


def build(root):
    root = Path(root)
    servers = find(root, "servers-provenance.json")
    gitea = find(root, "gitea-provenance.json")
    summary = {"pins": PINS, "provenance": {"servers": load(servers) if servers else {"missing": True},
                                            "gitea": load(gitea) if gitea else {"missing": True}},
               "regression": {}, "app": {}}
    for arm in ARMS:
        reg = find(root, "regression.json", arm)
        app = find(root, "results.json", arm)
        summary["regression"][arm] = load(reg) if reg else {"missing": "no regression.json was uploaded"}
        summary["app"][arm] = load(app) if app else {"missing": "no results.json was uploaded"}
    return summary


def compact(summary):
    """The fields a reader compares first, per arm."""
    out = {"provenance": summary["provenance"], "regression": {}, "app": {}}
    for arm in ARMS:
        reg = summary["regression"][arm]
        out["regression"][arm] = {"passed": reg.get("passed"), "missing": reg.get("missing"), "suites": [
            {k: s.get(k) for k in ("name", "commit", "gates", "passed", "exit_code", "top_level", "subtests",
                                   "ginkgo", "seconds")} |
            {"failing": [f"{f['package']} {f['test']}" for f in s.get("failing", [])],
             "unsupported": (s.get("wire") or {}).get("unsupported")}
            for s in reg.get("suites", [])]}
        app = summary["app"][arm]
        if "missing" in app:
            out["app"][arm] = app
            continue
        req = app.get("requests", {})
        out["app"][arm] = {
            "passed": app.get("passed"), "checks": app.get("checks"), "harness_error": app.get("harness_error"),
            "host": (app.get("tools") or {}).get("host"), "position": (app.get("config") or {}).get("position"),
            "server_command": (app.get("server") or {}).get("command"),
            "requests": {k: req.get(k) for k in ("total", "errors", "error_count_outside_restart", "by_phase")},
            "steady_get_latency": req.get("steady_all"),
            "endpoints": {k: v for k, v in (req.get("endpoints") or {}).items() if k in KEY_ENDPOINTS},
            "rss": app.get("rss"), "info": app.get("info"), "restart": app.get("restart"),
            "webhooks": {k: (app.get("webhooks") or {}).get(k) for k in
                         ("expected", "delivered_expected", "deliveries_received", "restart_window_seconds")} |
            {"missing": len((app.get("webhooks") or {}).get("missing", [])),
             "missing_outside_restart_window": len((app.get("webhooks") or {}).get("missing_outside_restart_window", []))},
            "wire": {k: (app.get("wire") or {}).get(k) for k in ("connections", "commands", "errors", "unsupported",
                                                                 "connections_refused_while_server_down")},
            "gitea_log": {k: (app.get("gitea_log") or {}).get(k) for k in ("lines", "error_lines")},
        }
    return out


def largest(mapping):
    values = [v for v in (mapping or {}).values() if v is not None]
    return max(values) if values else None


def cell(value):
    return "–" if value is None else str(value)


def markdown(summary, compact_summary):
    lines = ["# Application pilot: Gitea on Keel and on Redis", ""]
    prov = summary["provenance"]
    servers, gitea = prov.get("servers", {}), prov.get("gitea", {})
    lines += ["## Provenance", "", "| Item | Value |", "| --- | --- |"]
    for label, value in [
        ("Keel commit", servers.get("keel_commit")), ("Keel binary SHA-256", servers.get("keel_sha256")),
        ("Keel version", servers.get("keel_version")),
        ("Redis version", servers.get("redis_version")), ("Redis tarball SHA-256", servers.get("redis_tarball_sha256")),
        ("Redis binary SHA-256", servers.get("redis_sha256")),
        ("Gitea release", f"{PINS['gitea_app']['tag']} ({PINS['gitea_app']['commit']})"),
        ("Gitea binary SHA-256", gitea.get("gitea_sha256")), ("Gitea version", gitea.get("gitea_version")),
        ("Go", servers.get("go")), ("Runner", servers.get("runner")),
    ]:
        lines.append(f"| {label} | `{cell(value)}` |")
    for name, pin in PINS["checkouts"].items():
        lines.append(f"| {name} | `{pin['repo']}` at `{pin['commit']}` |")

    lines += ["", "## Regression suites (unmodified, pinned)", "",
              "| Suite | Gates | Keel | Redis |", "| --- | --- | --- | --- |"]
    names = []
    for arm in ARMS:
        for s in compact_summary["regression"][arm].get("suites", []):
            if s["name"] not in names:
                names.append(s["name"])

    def describe(s):
        if s is None:
            return "not run"
        top, sub = s.get("top_level") or {}, s.get("subtests") or {}
        text = f"{'pass' if s.get('passed') else 'FAIL'}: {top.get('pass', 0)}/{sum(top.values())} tests"
        if sub:
            text += f", {sub.get('pass', 0)}/{sum(sub.values())} subtests"
        for g in s.get("ginkgo") or []:
            text += f", Ginkgo {g.get('passed', '?')}/{g.get('ran', '?')} specs"
        if s.get("failing"):
            text += "; failing: " + ", ".join(s["failing"][:4])
        return text

    for name in names:
        row = {arm: next((s for s in compact_summary["regression"][arm].get("suites", []) if s["name"] == name),
                         None) for arm in ARMS}
        gates = next((s["gates"] for s in row.values() if s), None)
        lines.append(f"| {name} | {'yes' if gates else 'no'} | {describe(row['keel'])} | {describe(row['redis'])} |")

    lines += ["", "## Gitea web application", ""]
    apps = compact_summary["app"]
    lines += ["| Measure | Keel | Redis |", "| --- | --- | --- |"]

    def get(arm, *path):
        value = apps.get(arm, {})
        for part in path:
            value = value.get(part) if isinstance(value, dict) else None
        return value

    rows = [
        ("Host (both arms should match)", lambda a: get(a, "host")),
        ("Position on that host", lambda a: get(a, "position")),
        ("Checks passed", lambda a: get(a, "passed")),
        ("Requests / errors", lambda a: f"{get(a, 'requests', 'total')} / {get(a, 'requests', 'errors')}"),
        ("Errors outside the restart window", lambda a: get(a, "requests", "error_count_outside_restart")),
        ("Steady GET p50 / p95 / p99 ms", lambda a: "{} / {} / {}".format(
            get(a, "steady_get_latency", "p50_ms"), get(a, "steady_get_latency", "p95_ms"),
            get(a, "steady_get_latency", "p99_ms"))),
        ("Server RSS max MiB", lambda a: kib_to_mib(get(a, "rss", "server", "max_kib"))),
        ("Gitea RSS max MiB", lambda a: kib_to_mib(get(a, "rss", "gitea", "max_kib"))),
        ("Server used_memory at end", lambda a: get(a, "info", "end", "used_memory")),
        ("Server down (kill to PING) s", lambda a: get(a, "restart", "server_down_seconds")),
        ("Restart to PING s (AOF replay)", lambda a: get(a, "restart", "server_restart_to_ready_seconds")),
        ("AOF bytes replayed", lambda a: get(a, "restart", "aof_bytes_replayed")),
        ("Signed-in page after PING s (max)", lambda a: largest(get(a, "restart", "app_recovery_after_ready_seconds"))),
        ("Sessions survived", lambda a: f"{get(a, 'restart', 'sessions_survived')}/{get(a, 'restart', 'sessions_checked')}"),
        ("Probe writes acked / lost", lambda a: f"{get(a, 'restart', 'probe', 'acknowledged')} / "
                                                f"{get(a, 'restart', 'probe', 'lost')}"),
        ("Probe RPO ms", lambda a: get(a, "restart", "probe", "rpo_ms")),
        ("Webhooks expected / missing", lambda a: f"{get(a, 'webhooks', 'expected')} / {get(a, 'webhooks', 'missing')}"),
        ("Webhooks missing outside restart window", lambda a: get(a, "webhooks", "missing_outside_restart_window")),
        ("Unsupported commands seen", lambda a: ", ".join(get(a, "wire", "unsupported") or []) or "none"),
    ]
    for label, fn in rows:
        values = []
        for arm in ARMS:
            try:
                values.append(cell(fn(arm)) if "missing" not in apps.get(arm, {}) else "missing")
            except Exception:  # a partial result renders as unknown rather than failing the summary
                values.append("–")
        lines.append(f"| {label} | {values[0]} | {values[1]} |")

    for arm in ARMS:
        if get(arm, "server_command"):
            lines += ["", f"{arm} server command line: `" + " ".join(get(arm, "server_command")[1:]) + "`"]
    lines += ["", "### Latency by endpoint (ms, outside the restart window)", "",
              "| Endpoint | Keel p50 / p95 / p99 | Redis p50 / p95 / p99 |", "| --- | --- | --- |"]
    for endpoint in KEY_ENDPOINTS:
        cells = []
        for arm in ARMS:
            e = (get(arm, "endpoints") or {}).get(endpoint)
            cells.append("–" if not e else f"{e['p50_ms']} / {e['p95_ms']} / {e['p99_ms']} (n={e['count']})")
        lines.append(f"| `{endpoint}` | {cells[0]} | {cells[1]} |")

    lines += ["", "### Commands on the wire", "", "| Command | Keel | Redis |", "| --- | --- | --- |"]
    commands = sorted({c for arm in ARMS for c in (get(arm, "wire", "commands") or {})})
    for command in commands:
        cells = [cell(((get(arm, "wire", "commands") or {}).get(command) or {}).get("count")) for arm in ARMS]
        lines.append(f"| `{command}` | {cells[0]} | {cells[1]} |")
    for arm in ARMS:
        errors = get(arm, "wire", "errors") or []
        if errors:
            lines += ["", f"Error replies on the {arm} arm:", ""]
            lines += [f"- `{e['command']}` ×{e['count']}: {e['error']}" for e in errors[:20]]
    for arm in ARMS:
        if get(arm, "harness_error"):
            lines += ["", f"Harness error on the {arm} arm: `{get(arm, 'harness_error')}`"]
        checks = get(arm, "checks") or {}
        failed = [k for k, v in checks.items() if not v]
        if failed:
            lines += ["", f"Failed checks on the {arm} arm: " + ", ".join(failed)]
    return "\n".join(lines) + "\n"


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--artifacts", required=True, help="directory the pilot artifacts were downloaded into")
    ap.add_argument("--out", required=True)
    args = ap.parse_args()
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    summary = build(args.artifacts)
    short = compact(summary)
    (out / "summary-full.json").write_text(json.dumps(summary, indent=2, sort_keys=True) + "\n")
    (out / "summary.json").write_text(json.dumps(short, indent=2, sort_keys=True) + "\n")
    (out / "summary.md").write_text(markdown(summary, short))
    print((out / "summary.md").read_text())
    return 0


if __name__ == "__main__":
    sys.exit(main())
