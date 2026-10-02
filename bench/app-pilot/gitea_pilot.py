#!/usr/bin/env python3
"""Run the real Gitea web application on one arm and measure what it does.

Gitea keeps its cache, its web sessions and its job queues in the server under
test; the global lock and the websocket broker stay in memory, because Gitea
implements them with Lua scripts and Pub/Sub, which Keel does not provide (the
regression suite records exactly how those fail). Its database is SQLite.

The workload is what a small team does: an administrator creates accounts,
people sign in through the web form, create repositories, push over HTTP, open
issues, comment through the web form and browse. Pages are read repeatedly so
cached values are read back, and searches go through the issue and code
indexers. Every push, issue and comment queues a webhook delivery to a local
sink, so the queue is measured end to end by counting what arrives.

Midway, under load, the server is killed with SIGKILL and restarted from its
append-only file. The run records how long the server took to answer again,
how long until signed-in pages worked, whether sessions created before the
crash still sign their owners in, which queued deliveries never arrived, and,
from a separate writer, how many acknowledged writes were lost.

A SIGKILL is a process crash, not a power cut: data already handed to the
kernel survives it, so this measures what each server writes before replying,
not what its fsync policy protects against an operating-system crash.
"""
import argparse
import base64
import concurrent.futures
import http.client
import http.server
import json
import os
import re
import shutil
import subprocess
import sys
import threading
import time
import urllib.parse
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import pilot_lib  # noqa: E402
from pins import PINS  # noqa: E402

GITEA_PORT = 3000
SINK_PORT = 9000
PASSWORD = "pilot-password-1"
ADMIN = "pilotadmin"
# Phases during which request errors are expected: the server is down or the
# application is still reconnecting. Everything else must be error-free.
RESTART_PHASES = {"outage", "recovery"}


class Recorder:
    def __init__(self):
        self.lock = threading.Lock()
        self.samples = []
        self.phase = "setup"
        self.began = time.monotonic()

    def add(self, **sample):
        sample.setdefault("phase", self.phase)
        sample["t"] = round(time.monotonic() - self.began, 4)
        with self.lock:
            self.samples.append(sample)


class Browser:
    """One person's client: a cookie jar and a keep-alive connection.

    It never follows redirects, so a signed-in page that bounces to the login
    form is visible as a 303 rather than hidden behind a 200 for the form.
    """

    def __init__(self, recorder, user=None, password=None):
        self.recorder, self.user, self.password = recorder, user, password
        self.cookies = {}
        self.conn = None

    def request(self, method, path, endpoint, *, form=None, body=None, basic=False, ok=(200,)):
        headers = {"User-Agent": "keel-application-pilot"}
        payload = None
        if form is not None:
            payload = urllib.parse.urlencode(form).encode()
            headers["Content-Type"] = "application/x-www-form-urlencoded"
        elif body is not None:
            payload = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        if basic:
            token = base64.b64encode(f"{self.user}:{self.password}".encode()).decode()
            headers["Authorization"] = f"Basic {token}"
        if self.cookies and not basic:
            headers["Cookie"] = "; ".join(f"{k}={v}" for k, v in self.cookies.items())
        began = time.monotonic()
        status, data, error = None, b"", None
        try:
            if self.conn is None:
                self.conn = http.client.HTTPConnection("127.0.0.1", GITEA_PORT, timeout=60)
            self.conn.request(method, path, body=payload, headers=headers)
            response = self.conn.getresponse()
            data = response.read()
            status = response.status
            for cookie in response.msg.get_all("Set-Cookie") or ():
                name, _, rest = cookie.partition("=")
                value = rest.split(";", 1)[0]
                if "max-age=0" in cookie.lower() or value == "":
                    self.cookies.pop(name, None)
                else:
                    self.cookies[name] = value
        except (OSError, http.client.HTTPException) as exc:
            error = repr(exc)
            self.close()
        seconds = time.monotonic() - began
        success = error is None and status in ok
        if self.recorder is not None:  # readiness polling is not part of the workload
            self.recorder.add(endpoint=f"{method} {endpoint}", status=status, seconds=seconds, ok=success,
                              error=error if error else (None if success else f"status {status}"))
        return status, data, success

    def close(self):
        if self.conn is not None:
            self.conn.close()
            self.conn = None


class WebhookSink:
    """Receives Gitea's webhook deliveries and remembers what each one was for."""

    def __init__(self, recorder):
        self.lock = threading.Lock()
        self.deliveries = []
        sink = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                length = int(self.headers.get("Content-Length") or 0)
                raw = self.rfile.read(length)
                try:
                    payload = json.loads(raw or b"{}")
                except json.JSONDecodeError:
                    payload = {}
                sink.record(self.headers.get("X-Gitea-Event"), self.headers.get("X-Gitea-Delivery"), payload,
                            time.monotonic() - recorder.began)
                self.send_response(200)
                self.end_headers()

            def log_message(self, *args):
                pass

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", SINK_PORT), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    def record(self, event, delivery, payload, t):
        repo = (payload.get("repository") or {}).get("full_name")
        if event == "push":
            key = ("push", repo, payload.get("after"))
        elif event == "issues":
            key = ("issues", repo, payload.get("action"), payload.get("number"))
        elif event == "issue_comment":
            key = ("issue_comment", repo, payload.get("action"), (payload.get("comment") or {}).get("id"))
        else:
            key = (event, repo)
        with self.lock:
            self.deliveries.append({"key": list(key), "delivery": delivery, "t": round(t, 3)})

    def keys(self):
        with self.lock:
            return {tuple(d["key"]) for d in self.deliveries}

    def start(self):
        self.thread.start()

    def stop(self):
        self.server.shutdown()


class ProbeWriter(threading.Thread):
    """Acknowledged writes straight to the server, to count what a crash loses.

    It bypasses the tap and Gitea so its commands do not enter the application's
    command mix. Each SET is acknowledged before the next is sent; the first
    failure, which the kill causes, ends the run.
    """

    def __init__(self, port, interval=0.002):
        super().__init__(daemon=True)
        self.port, self.interval = port, interval
        self.acked = []  # (sequence, monotonic time of the acknowledgement)
        self.halt = threading.Event()
        self.ended = None

    def run(self):
        try:
            client = pilot_lib.Client("127.0.0.1", self.port)
        except OSError as exc:
            self.ended = repr(exc)
            return
        seq = 0
        while not self.halt.is_set():
            seq += 1
            try:
                if client.call("SET", f"pilot:probe:{seq}", str(time.time_ns())) != b"OK":
                    self.ended = "unexpected reply"
                    break
            except (OSError, RuntimeError, ValueError) as exc:
                self.ended = repr(exc)
                break
            self.acked.append((seq, time.monotonic()))
            time.sleep(self.interval)
        client.close()

    def verify(self):
        """Return how many acknowledged writes survived the restart, and the RPO."""
        client = pilot_lib.Client("127.0.0.1", self.port)
        present = set()
        try:
            for start in range(0, len(self.acked), 500):
                batch = [seq for seq, _ in self.acked[start:start + 500]]
                values = client.call("MGET", *[f"pilot:probe:{s}" for s in batch])
                present.update(s for s, v in zip(batch, values) if v is not None)
        finally:
            client.close()
        lost = [seq for seq, _ in self.acked if seq not in present]
        rpo_ms = 0.0
        if lost:
            times = dict(self.acked)
            survivors = [times[s] for s in present]
            last_ack = self.acked[-1][1]
            rpo_ms = round((last_ack - (max(survivors) if survivors else self.acked[0][1])) * 1000, 1)
        return {"acknowledged": len(self.acked), "survived": len(present), "lost": len(lost),
                "first_lost": lost[:5], "rpo_ms": rpo_ms, "writer_ended": self.ended}


def gitea_cli(gitea, config, *args, capture=True):
    out = subprocess.run([gitea, *args, "--config", str(config)], capture_output=capture, text=True,
                         env=dict(os.environ, GITEA_WORK_DIR=str(config.parents[2])))
    if out.returncode != 0:
        raise RuntimeError(f"gitea {' '.join(args)} failed: {out.stdout}\n{out.stderr}")
    return out.stdout.strip()


def write_config(gitea, work):
    config = work / "custom/conf/app.ini"
    config.parent.mkdir(parents=True, exist_ok=True)
    secret = subprocess.run([gitea, "generate", "secret", "SECRET_KEY"], capture_output=True, text=True,
                            check=True).stdout.strip()
    token = subprocess.run([gitea, "generate", "secret", "INTERNAL_TOKEN"], capture_output=True, text=True,
                           check=True).stdout.strip()
    redis = f"redis://127.0.0.1:{pilot_lib.APP_PORT}/0"
    config.write_text(f"""APP_NAME = Keel application pilot
RUN_MODE = prod
WORK_PATH = {work}

[server]
HTTP_ADDR = 127.0.0.1
HTTP_PORT = {GITEA_PORT}
ROOT_URL = http://127.0.0.1:{GITEA_PORT}/
DISABLE_SSH = true
START_SSH_SERVER = false
LFS_START_SERVER = false
OFFLINE_MODE = true

[database]
DB_TYPE = sqlite3
PATH = {work}/data/gitea.db

[security]
INSTALL_LOCK = true
SECRET_KEY = {secret}
INTERNAL_TOKEN = {token}
ALLOWED_HOST_LIST = loopback

[service]
DISABLE_REGISTRATION = true

[repository]
ROOT = {work}/repositories
DEFAULT_BRANCH = main

[cache]
ADAPTER = redis
HOST = {redis}?pool_size=100&idle_timeout=180s
ITEM_TTL = 16h

[session]
PROVIDER = redis
PROVIDER_CONFIG = {redis}?prefix=pilot_session:
SESSION_LIFE_TIME = 86400

[queue]
TYPE = redis
CONN_STR = {redis}

; Gitea's Redis global lock is redsync, which runs Lua scripts, and its Redis
; websocket broker uses Pub/Sub. Keel has neither, so both stay in process.
[global_lock]
SERVICE_TYPE = memory

[websocket]
PUBSUB_TYPE = memory

[indexer]
ISSUE_INDEXER_TYPE = bleve
REPO_INDEXER_ENABLED = true
REPO_INDEXER_TYPE = bleve

[webhook]
DELIVER_TIMEOUT = 5

[mailer]
ENABLED = false

[oauth2]
ENABLED = false

[picture]
DISABLE_GRAVATAR = true

[cron.update_checker]
ENABLED = false

[log]
MODE = file
LEVEL = Info
ROOT_PATH = {work}/log
""")
    return config


def wait_healthy(browser, timeout):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        status, _, _ = browser.request("GET", "/api/healthz", "/api/healthz", ok=(200,))
        if status == 200:
            return True
        time.sleep(0.25)
    return False


class Workload:
    def __init__(self, args, recorder, sink, out):
        self.args, self.recorder, self.sink, self.out = args, recorder, sink, out
        self.users = [f"pilot{i}" for i in range(args.users)]
        self.browsers = {u: Browser(recorder, u, PASSWORD) for u in self.users}
        self.api = {u: Browser(recorder, u, PASSWORD) for u in self.users}
        self.repos = {u: [] for u in self.users}
        self.issues = {}  # (owner, repo) -> [issue numbers]
        self.expected = []  # webhook keys with the time their action was acknowledged
        self.lock = threading.Lock()
        self.git_root = out / "git"

    def expect(self, key):
        with self.lock:
            self.expected.append({"key": list(key), "t": round(time.monotonic() - self.recorder.began, 3),
                                  "phase": self.recorder.phase})

    def create_users(self):
        admin = Browser(self.recorder, ADMIN, PASSWORD)
        for user in self.users:
            admin.request("POST", "/api/v1/admin/users", "/api/v1/admin/users", basic=True, ok=(201,),
                          body={"username": user, "email": f"{user}@example.com", "password": PASSWORD,
                                "must_change_password": False, "visibility": "public"})

    def login(self, user):
        browser = self.browsers[user]
        browser.request("GET", "/user/login", "/user/login")
        browser.request("POST", "/user/login", "/user/login", form={"user_name": user, "password": PASSWORD},
                        ok=(303,))
        status, _, _ = browser.request("GET", "/user/settings", "/user/settings")
        return status == 200

    def create_repo(self, user, index):
        name = f"repo{index}"
        status, _, _ = self.api[user].request(
            "POST", "/api/v1/user/repos", "/api/v1/user/repos", basic=True, ok=(201,),
            body={"name": name, "auto_init": True, "default_branch": "main", "readme": "Default",
                  "description": "Keel application pilot repository"})
        if status != 201:
            return
        self.api[user].request(
            "POST", f"/api/v1/repos/{user}/{name}/hooks", "/api/v1/repos/{owner}/{repo}/hooks", basic=True,
            ok=(201,), body={"type": "gitea", "active": True, "events": ["push", "issues", "issue_comment"],
                             "config": {"url": f"http://127.0.0.1:{SINK_PORT}/{user}/{name}",
                                        "content_type": "json"}})
        with self.lock:
            self.repos[user].append(name)
            self.issues[(user, name)] = []

    def push(self, user, repo, round_):
        checkout = self.git_root / user / repo
        env = dict(os.environ, GIT_TERMINAL_PROMPT="0")
        url = f"http://{user}:{PASSWORD}@127.0.0.1:{GITEA_PORT}/{user}/{repo}.git"
        git = ["git", "-c", f"user.name={user}", "-c", f"user.email={user}@example.com"]
        began = time.monotonic()
        error = None
        try:
            if not checkout.exists():
                checkout.parent.mkdir(parents=True, exist_ok=True)
                subprocess.run(git + ["clone", "-q", url, str(checkout)], env=env, check=True,
                               capture_output=True, timeout=120)
            for n in range(3):
                path = checkout / f"notes/round-{round_}-{n}.md"
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(f"# Pilot note {round_}.{n}\n\npilot keel search marker {user} {repo}\n" * 20)
                subprocess.run(git + ["-C", str(checkout), "add", "-A"], env=env, check=True,
                               capture_output=True)
                subprocess.run(git + ["-C", str(checkout), "commit", "-q", "-m", f"Pilot round {round_}.{n}"],
                               env=env, check=True, capture_output=True)
            subprocess.run(git + ["-C", str(checkout), "push", "-q", "origin", "HEAD:main"], env=env, check=True,
                           capture_output=True, timeout=120)
            after = subprocess.run(["git", "-C", str(checkout), "rev-parse", "HEAD"], capture_output=True,
                                   text=True, check=True).stdout.strip()
            self.expect(("push", f"{user}/{repo}", after))
        except (subprocess.CalledProcessError, subprocess.TimeoutExpired) as exc:
            error = f"{exc!r} {getattr(exc, 'stderr', b'')!r}"[:500]
        self.recorder.add(endpoint="git push", status=None, seconds=time.monotonic() - began, ok=error is None,
                          error=error)

    def open_issue(self, user, repo, n):
        status, data, ok = self.api[user].request(
            "POST", f"/api/v1/repos/{user}/{repo}/issues", "/api/v1/repos/{owner}/{repo}/issues", basic=True,
            ok=(201,), body={"title": f"Pilot issue {n}", "body": f"pilot keel search marker issue {n}"})
        if ok:
            number = json.loads(data)["number"]
            with self.lock:
                self.issues[(user, repo)].append(number)
            self.expect(("issues", f"{user}/{repo}", "opened", number))

    def comment(self, user, owner, repo, number, n):
        status, data, ok = self.browsers[user].request(
            "POST", f"/{owner}/{repo}/issues/{number}/comments", "/{owner}/{repo}/issues/{n}/comments",
            form={"content": f"Pilot comment {n} from {user}"})
        if ok:
            match = re.search(rb"issuecomment-(\d+)", data)
            if match:
                self.expect(("issue_comment", f"{owner}/{repo}", "created", int(match[1])))

    def pages(self, user):
        """The pages one person reads in a round: their own and someone else's."""
        others = [u for u in self.users if u != user and self.repos[u]]
        targets = [(user, r) for r in self.repos[user][:1]]
        if others:
            other = others[hash((user, time.monotonic_ns())) % len(others)]
            targets.append((other, self.repos[other][0]))
        yield "/", "/"
        yield "/explore/repos", "/explore/repos"
        yield f"/{user}", "/{user}"
        yield "/user/settings", "/user/settings"
        for owner, repo in targets:
            base = f"/{owner}/{repo}"
            yield base, "/{owner}/{repo}"
            yield f"{base}/issues", "/{owner}/{repo}/issues"
            numbers = self.issues.get((owner, repo)) or []
            if numbers:
                yield f"{base}/issues/{numbers[0]}", "/{owner}/{repo}/issues/{n}"
            yield f"{base}/commits/branch/main", "/{owner}/{repo}/commits/branch/main"
            yield f"{base}/src/branch/main/README.md", "/{owner}/{repo}/src/branch/main/README.md"
            yield f"{base}/issues?q=marker", "/{owner}/{repo}/issues?q="
            yield f"{base}/search?q=marker", "/{owner}/{repo}/search?q="

    def browse(self, user, rounds):
        for _ in range(rounds):
            for path, endpoint in self.pages(user):
                self.browsers[user].request("GET", path, endpoint)

    def each_user(self, fn, *args):
        with concurrent.futures.ThreadPoolExecutor(len(self.users)) as pool:
            for future in [pool.submit(fn, u, *args) for u in self.users]:
                future.result()

    def populate(self, user, pushes):
        for index in range(self.args.repos_per_user):
            self.create_repo(user, index)
        for repo in list(self.repos[user]):
            for round_ in range(pushes):
                self.push(user, repo, round_)
            for n in range(self.args.issues_per_repo):
                self.open_issue(user, repo, n)
        for repo in list(self.repos[user]):
            for number in list(self.issues[(user, repo)])[:2]:
                self.comment(user, user, repo, number, 0)

    def mixed(self, user, halt, counter):
        """Browse continuously, writing an issue and a comment every few pages."""
        n = 100
        while not halt.is_set():
            for path, endpoint in self.pages(user):
                if halt.is_set():
                    return
                self.browsers[user].request("GET", path, endpoint)
            repo = self.repos[user][0]
            n += 1
            self.open_issue(user, repo, n)
            numbers = self.issues[(user, repo)]
            if numbers:
                self.comment(user, user, repo, numbers[-1], n)
            counter[user] = counter.get(user, 0) + 1


def run(args):
    out = Path(args.out).resolve()
    out.mkdir(parents=True, exist_ok=True)
    work = out / "gitea-work"
    recorder = Recorder()
    results = {"arm": args.arm, "config": {k: getattr(args, k) for k in
                                           ("users", "repos_per_user", "issues_per_repo", "rounds",
                                            "load_seconds", "outage_seconds", "maxmemory")},
               "gitea": {"tag": PINS["gitea_app"]["tag"], "commit": PINS["gitea_app"]["commit"],
                         "binary_sha256": pilot_lib.sha256(args.gitea_bin)},
               "tools": pilot_lib.tool_versions(), "phases": {}, "info": {}, "checks": {}}
    server = pilot_lib.CacheServer(args.arm, args.server_bin, out / "server", maxmemory=args.maxmemory)
    results["server"] = {"binary_sha256": pilot_lib.sha256(args.server_bin), "version": server.version(),
                         "command": server.command()}
    tap = pilot_lib.Tap(out / "wire.jsonl")
    sink = WebhookSink(recorder)
    gitea = None
    sampler = None
    gitea_log = (out / "gitea-stdout.log").open("ab")
    phase_began = {}

    def phase(name):
        now = time.monotonic()
        if recorder.phase in phase_began:
            results["phases"][recorder.phase] = round(now - phase_began[recorder.phase], 2)
        recorder.phase = name
        phase_began[name] = now
        if sampler:
            sampler.phase = name
        print(f"== phase {name} at {now - recorder.began:.1f}s", flush=True)

    def snapshot(label):
        try:
            text = server.info()
            (out / f"info-{label}.txt").write_text(text)
            results["info"][label] = pilot_lib.info_digest(text)
        except Exception as exc:
            results["info"][label] = {"error": repr(exc)}

    try:
        phase("setup")
        results["server"]["first_start_seconds"] = server.start()
        tap.start()
        sink.start()
        snapshot("start")
        results["gitea"]["version"] = subprocess.run([args.gitea_bin, "--version"], capture_output=True,
                                                     text=True).stdout.strip()
        config = write_config(args.gitea_bin, work)
        # The configuration is part of the evidence; its generated secrets are not.
        (out / "app.ini").write_text(re.sub(r"^(SECRET_KEY|INTERNAL_TOKEN) = .*$", r"\1 = <redacted>",
                                            config.read_text(), flags=re.M))
        gitea_cli(args.gitea_bin, config, "migrate")
        gitea_cli(args.gitea_bin, config, "admin", "user", "create", "--admin", "--username", ADMIN,
                  "--password", PASSWORD, "--email", "admin@example.com", "--must-change-password=false")
        gitea = subprocess.Popen([args.gitea_bin, "web", "--config", str(config)], stdout=gitea_log,
                                 stderr=gitea_log, stdin=subprocess.DEVNULL,
                                 env=dict(os.environ, GITEA_WORK_DIR=str(work)))
        sampler = pilot_lib.RssSampler(out / "rss.csv", {"server": lambda: server.pid,
                                                         "gitea": lambda: gitea.pid if gitea.poll() is None
                                                         else None})
        sampler.phase = recorder.phase
        sampler.start()
        if not wait_healthy(Browser(None), 180):
            raise RuntimeError("gitea did not become healthy")
        workload = Workload(args, recorder, sink, out)
        workload.create_users()
        logins = {}
        for user in workload.users:
            logins[user] = workload.login(user)
        results["logins"] = logins

        phase("populate")
        workload.each_user(workload.populate, 2)

        phase("browse")
        workload.each_user(workload.browse, args.rounds)
        snapshot("pre_kill")

        # Sessions to test after the crash: copies of each cookie jar taken
        # now, so a cookie Gitea issues during the outage cannot replace the
        # session whose survival is being checked.
        phase("load")
        saved = {u: dict(workload.browsers[u].cookies) for u in workload.users}
        halt = threading.Event()
        counter = {}
        probe = ProbeWriter(pilot_lib.SERVER_PORT)
        probe.start()
        pool = concurrent.futures.ThreadPoolExecutor(len(workload.users))
        futures = [pool.submit(workload.mixed, u, halt, counter) for u in workload.users]
        try:
            time.sleep(args.load_seconds)

            phase("outage")
            killed_at = time.monotonic()
            server.kill()
            time.sleep(args.outage_seconds)
            ready_seconds = server.start()
            ready_at = time.monotonic()

            phase("recovery")
            # The first signed-in page that works again, per person, from a
            # fresh connection carrying the cookies saved before the crash.
            # A redirect to the login form can also come from a read that
            # failed while connections were being re-established, so a
            # session counts as lost only after three redirects in a row.
            survived, recovered_at = {}, {}
            signed_out_streak = {u: 0 for u in workload.users}
            deadline = time.monotonic() + 120
            pending = set(workload.users)
            while pending and time.monotonic() < deadline:
                for user in sorted(pending):
                    check = Browser(recorder, user, PASSWORD)
                    check.cookies = dict(saved[user])
                    status, body, _ = check.request("GET", "/user/settings", "/user/settings (saved session)")
                    check.close()
                    if status == 200 and user.encode() in body:
                        survived[user] = True
                        recovered_at[user] = time.monotonic()
                        pending.discard(user)
                    elif status == 303:
                        signed_out_streak[user] += 1
                        if signed_out_streak[user] >= 3:
                            survived[user] = False
                            recovered_at[user] = time.monotonic()
                            pending.discard(user)
                    else:
                        signed_out_streak[user] = 0
                time.sleep(0.2)
        finally:
            halt.set()
            for future in futures:
                future.result()
            pool.shutdown()
        probe.halt.set()
        probe.join(5)
        results["restart"] = {
            "outage_requested_seconds": args.outage_seconds,
            "server_down_seconds": round(ready_at - killed_at, 3),
            "server_restart_to_ready_seconds": round(ready_seconds, 3),
            "aof_bytes_replayed": server.starts[-1]["aof_bytes"],
            "app_recovery_after_ready_seconds": {u: round(t - ready_at, 3) for u, t in recovered_at.items()},
            "sessions_checked": len(workload.users),
            "sessions_survived": sum(1 for v in survived.values() if v),
            "sessions_signed_out": sorted(u for u, v in survived.items() if not v),
            "sessions_undetermined": sorted(pending),
            "mixed_rounds_per_user": counter,
            "probe": probe.verify(),
        }
        snapshot("post_restart")

        phase("post")
        workload.each_user(lambda u: [workload.push(u, r, 9) for r in workload.repos[u]])
        workload.each_user(workload.browse, max(1, args.rounds // 2))

        phase("drain")
        expected = {tuple(e["key"]) for e in workload.expected}
        deadline = time.monotonic() + args.drain_seconds
        while time.monotonic() < deadline and not expected <= sink.keys():
            time.sleep(0.5)
        delivered = sink.keys()
        window = (killed_at - recorder.began - 2.0, ready_at - recorder.began + 5.0)
        missing = [e for e in workload.expected if tuple(e["key"]) not in delivered]
        results["webhooks"] = {
            "expected": len(workload.expected), "delivered_expected": len(workload.expected) - len(missing),
            "deliveries_received": len(sink.deliveries),
            "missing": missing,
            "missing_outside_restart_window": [e for e in missing if not window[0] <= e["t"] <= window[1]],
            "restart_window_seconds": [round(window[0], 2), round(window[1], 2)],
        }
        snapshot("end")
        phase("done")
    except Exception as exc:  # recorded, never discarded
        results["harness_error"] = repr(exc)
        print(f"harness error: {exc!r}", flush=True)
    finally:
        if gitea is not None and gitea.poll() is None:
            gitea.terminate()
            try:
                gitea.wait(timeout=30)
            except subprocess.TimeoutExpired:
                gitea.kill()
        if sampler:
            sampler.stop()
            results["rss"] = sampler.summary()
        sink.stop()
        tap.stop()
        results["server"]["exit_code"] = server.stop()
        results["server"]["starts"] = server.starts
        gitea_log.close()

    samples = recorder.samples
    results["requests"] = summarize_requests(samples)
    results["wire"] = pilot_lib.summarize_wire([out / "wire.jsonl"]) if (out / "wire.jsonl").exists() else {}
    log_path = work / "log/gitea.log"
    if log_path.exists():
        shutil.copy(log_path, out / "gitea.log")
        lines = log_path.read_text(errors="replace").splitlines()
        errors = [line for line in lines if " [E] " in line]
        results["gitea_log"] = {"lines": len(lines), "error_lines": len(errors), "first_errors": errors[:40]}
    (out / "requests.json").write_text(json.dumps(samples))
    results["checks"] = checks(results)
    results["passed"] = all(results["checks"].values())
    pilot_lib.write_json(out / "results.json", results)
    return 0 if results["passed"] else 1


def summarize_requests(samples):
    by_endpoint = {}
    for s in samples:
        by_endpoint.setdefault(s["endpoint"], []).append(s)
    steady = [s for s in samples if s["phase"] not in RESTART_PHASES]
    errors = [s for s in samples if not s["ok"]]
    phases = {}
    for s in samples:
        p = phases.setdefault(s["phase"], {"requests": 0, "errors": 0})
        p["requests"] += 1
        p["errors"] += 0 if s["ok"] else 1
    return {
        "total": len(samples), "errors": len(errors), "by_phase": phases,
        "errors_outside_restart": [s for s in errors if s["phase"] not in RESTART_PHASES][:50],
        "error_count_outside_restart": sum(1 for s in errors if s["phase"] not in RESTART_PHASES),
        "endpoints": {name: dict(pilot_lib.latency_summary([s["seconds"] for s in group
                                                            if s["ok"] and s["phase"] not in RESTART_PHASES]),
                                 requests=len(group), errors=sum(1 for s in group if not s["ok"]))
                      for name, group in sorted(by_endpoint.items())},
        "steady_all": pilot_lib.latency_summary([s["seconds"] for s in steady if s["ok"] and
                                                 s["endpoint"].startswith("GET ")]),
    }


def checks(results):
    restart = results.get("restart", {})
    webhooks = results.get("webhooks", {})
    wire = results.get("wire", {})
    return {
        "harness_completed": "harness_error" not in results,
        "all_logins_succeeded": bool(results.get("logins")) and all(results["logins"].values()),
        "no_request_errors_outside_restart": results["requests"]["error_count_outside_restart"] == 0,
        "all_sessions_survived_restart": bool(restart) and
        restart.get("sessions_survived") == restart.get("sessions_checked"),
        "no_unsupported_commands_beyond_client_probes": bool(wire) and not wire.get("unsupported_outside_handshake"),
        "webhooks_outside_restart_window_delivered": bool(webhooks) and
        not webhooks.get("missing_outside_restart_window"),
    }


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--arm", choices=pilot_lib.ARMS, required=True)
    ap.add_argument("--server-bin", required=True)
    ap.add_argument("--gitea-bin", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--maxmemory", default="256mb")
    ap.add_argument("--users", type=int, default=6)
    ap.add_argument("--repos-per-user", type=int, default=2)
    ap.add_argument("--issues-per-repo", type=int, default=3)
    ap.add_argument("--rounds", type=int, default=8, help="browse rounds per person before the crash")
    ap.add_argument("--load-seconds", type=float, default=20, help="mixed load before the kill")
    ap.add_argument("--outage-seconds", type=float, default=3, help="how long the server stays down")
    ap.add_argument("--drain-seconds", type=float, default=90, help="how long to wait for queued deliveries")
    return run(ap.parse_args())


if __name__ == "__main__":
    sys.exit(main())
