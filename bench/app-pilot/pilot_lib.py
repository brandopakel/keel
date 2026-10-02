"""Shared pieces of the application pilot: the cache server under test, the
wire tap in front of it, process memory sampling, and result summaries.

Both arms are configured here, side by side, so that a difference between them
has to be visible in one place: append-only persistence with a one-second
fsync, the same memory limit, and least-recently-used eviction over all keys,
which is the policy Keel applies when -maxmemory is reached.
"""
import collections
import csv
import hashlib
import json
import math
import os
import signal
import socket
import subprocess
import sys
import threading
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parents[1] / "bench/external/aws"))
from resp_client import Client  # noqa: E402

ARMS = ("keel", "redis")
# The application talks to the tap on Redis's well-known port, because some
# pinned test suites hard-code 127.0.0.1:6379; the server itself sits behind it.
APP_PORT = 6379
SERVER_PORT = 16379

# Requests a current go-redis client sends on its own, and survives being
# refused by design: it falls back to RESP2 when HELLO fails, ignores a refused
# CLIENT SETINFO, skips maintenance notifications without RESP3, and its Ring
# client works without COMMAND metadata. They are counted and reported, but
# they do not by themselves make a run incompatible.
HANDSHAKE_COMMANDS = {"HELLO", "CLIENT SETINFO", "CLIENT MAINT_NOTIFICATIONS", "COMMAND"}


def sha256(path):
    digest = hashlib.sha256()
    with Path(path).open("rb") as source:
        while chunk := source.read(1 << 20):
            digest.update(chunk)
    return digest.hexdigest()


def port_open(port, host="127.0.0.1"):
    try:
        socket.create_connection((host, port), 0.2).close()
        return True
    except OSError:
        return False


def wait_port(port, timeout, process=None):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if process is not None and process.poll() is not None:
            raise RuntimeError(f"process exited with {process.returncode} before port {port} opened")
        if port_open(port):
            return
        time.sleep(0.02)
    raise TimeoutError(f"port {port} did not open within {timeout}s")


class CacheServer:
    """Keel or Redis with identical durability and memory settings."""

    def __init__(self, arm, binary, directory, *, port=SERVER_PORT, maxmemory="256mb"):
        if arm not in ARMS:
            raise ValueError(f"unknown arm {arm!r}")
        self.arm, self.binary, self.port, self.maxmemory = arm, str(Path(binary).resolve()), port, maxmemory
        self.directory = Path(directory).resolve()
        self.directory.mkdir(parents=True, exist_ok=True)
        self.process = None
        self.log = None
        self.starts = []

    def command(self):
        if self.arm == "keel":
            return [self.binary, "-host", "127.0.0.1", "-port", str(self.port),
                    "-appendonly", "-appendfilename", str(self.directory / "keel.aof"),
                    "-appendfsync", "everysec", "-maxmemory", self.maxmemory, "-evict", "lru"]
        # RDB snapshots are off so that the append-only file is the only
        # persistence on both arms, and a restart replays the same kind of log.
        return [self.binary, "--bind", "127.0.0.1", "--port", str(self.port),
                "--dir", str(self.directory), "--appendonly", "yes",
                "--appendfsync", "everysec", "--save", "", "--maxmemory", self.maxmemory,
                "--maxmemory-policy", "allkeys-lru", "--daemonize", "no"]

    def start(self, timeout=60):
        """Start the server and return the seconds until it answered PING."""
        self.log = (self.directory / "server.log").open("ab")
        began = time.monotonic()
        self.process = subprocess.Popen(self.command(), stdout=self.log, stderr=self.log,
                                        stdin=subprocess.DEVNULL)
        deadline = began + timeout
        while time.monotonic() < deadline:
            if self.process.poll() is not None:
                raise RuntimeError(f"{self.arm} exited with {self.process.returncode} during startup; "
                                   f"see {self.directory}/server.log")
            try:
                client = Client("127.0.0.1", self.port)
                try:
                    if client.call("PING") == b"PONG":
                        ready = time.monotonic() - began
                        self.starts.append({"pid": self.process.pid, "ready_seconds": ready,
                                            "aof_bytes": self.aof_bytes()})
                        return ready
                finally:
                    client.close()
            except (OSError, RuntimeError, ValueError):
                pass
            time.sleep(0.005)
        raise TimeoutError(f"{self.arm} did not answer PING within {timeout}s")

    def kill(self):
        """Crash the process without letting it flush or sync anything."""
        self.process.send_signal(signal.SIGKILL)
        self.process.wait()
        self.log.close()

    def stop(self, timeout=60):
        if self.process is None or self.process.poll() is not None:
            return self.process.returncode if self.process else None
        self.process.terminate()
        try:
            code = self.process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            self.process.kill()
            code = self.process.wait()
        self.log.close()
        return code

    @property
    def pid(self):
        if self.process is not None and self.process.poll() is None:
            return self.process.pid
        return None

    def aof_bytes(self):
        paths = [self.directory / "keel.aof"] if self.arm == "keel" else \
            list((self.directory / "appendonlydir").glob("*"))
        return sum(p.stat().st_size for p in paths if p.is_file())

    def info(self):
        client = Client("127.0.0.1", self.port)
        try:
            raw = client.call("INFO")
        finally:
            client.close()
        return raw.decode("utf-8", "replace") if isinstance(raw, bytes) else str(raw)

    def version(self):
        flag = "-version" if self.arm == "keel" else "--version"
        out = subprocess.run([self.binary, flag], capture_output=True, text=True)
        lines = (out.stdout.strip() or out.stderr.strip()).splitlines()
        return lines[0] if lines else ""


def parse_info(text):
    fields = {}
    for line in text.splitlines():
        if ":" in line and not line.startswith("#"):
            key, value = line.split(":", 1)
            fields[key.strip()] = value.strip()
    return fields


# INFO fields worth carrying into a summary; each server names them its own way.
INFO_FIELDS = ("used_memory", "used_memory_rss", "used_memory_peak", "keel_version", "redis_version",
               "aof_enabled", "aof_current_size", "aof_base_size", "aof_rewrites",
               "evicted_keys", "expired_keys", "connected_clients", "db0")


def info_digest(text):
    fields = parse_info(text)
    return {key: fields[key] for key in INFO_FIELDS if key in fields}


class Tap:
    """The resp_tap.py process, owned by the harness."""

    def __init__(self, log_path, *, listen=APP_PORT, upstream=SERVER_PORT):
        self.log_path, self.listen, self.upstream = Path(log_path), listen, upstream
        self.process = None

    def start(self):
        if port_open(self.listen):
            raise RuntimeError(f"port {self.listen} is already in use; the tap must own it")
        stderr = self.log_path.with_suffix(".stderr").open("ab")
        self.process = subprocess.Popen(
            [sys.executable, str(HERE / "resp_tap.py"), "--listen", str(self.listen),
             "--upstream", str(self.upstream), "--log", str(self.log_path)],
            stdout=stderr, stderr=stderr, stdin=subprocess.DEVNULL)
        wait_port(self.listen, 10, self.process)
        return self

    def stop(self):
        if self.process is not None and self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait()


def read_rss_kib(pid):
    try:
        with open(f"/proc/{pid}/status") as status:
            for line in status:
                if line.startswith("VmRSS:"):
                    return int(line.split()[1])
    except (FileNotFoundError, ProcessLookupError, PermissionError):
        return None
    return None


class RssSampler(threading.Thread):
    """Samples resident memory of named processes into a CSV until stopped.

    The callables return the current pid, so a server restarted under a new pid
    keeps being followed. The phase is set by the workload and recorded with
    each sample, so memory can be read against what the application was doing.
    """

    def __init__(self, path, processes, interval=0.5):
        super().__init__(daemon=True)
        self.path, self.processes, self.interval = Path(path), processes, interval
        self.phase = "start"
        self.halt = threading.Event()
        self.samples = []
        self.began = time.monotonic()

    def run(self):
        with self.path.open("w", newline="") as out:
            writer = csv.writer(out)
            writer.writerow(["elapsed_s", "phase", "process", "pid", "rss_kib"])
            while not self.halt.is_set():
                elapsed = round(time.monotonic() - self.began, 3)
                for name, pid_of in self.processes.items():
                    pid = pid_of()
                    rss = read_rss_kib(pid) if pid else None
                    if rss is not None:
                        writer.writerow([elapsed, self.phase, name, pid, rss])
                        self.samples.append((elapsed, self.phase, name, rss))
                out.flush()
                self.halt.wait(self.interval)

    def stop(self):
        self.halt.set()
        self.join(timeout=5)

    def summary(self):
        by_name = collections.defaultdict(list)
        by_phase = collections.defaultdict(lambda: collections.defaultdict(list))
        for _, phase, name, rss in self.samples:
            by_name[name].append(rss)
            by_phase[name][phase].append(rss)
        return {name: {"samples": len(v), "max_kib": max(v), "mean_kib": round(sum(v) / len(v)),
                       "last_kib": v[-1],
                       "max_kib_by_phase": {phase: max(r) for phase, r in by_phase[name].items()}}
                for name, v in by_name.items()}


def percentile(values, q):
    """Nearest-rank percentile; exact on small samples, no interpolation."""
    if not values:
        return None
    ordered = sorted(values)
    rank = max(1, math.ceil(q / 100 * len(ordered)))
    return ordered[rank - 1]


def latency_summary(seconds):
    return {"count": len(seconds),
            "p50_ms": round(percentile(seconds, 50) * 1000, 2) if seconds else None,
            "p95_ms": round(percentile(seconds, 95) * 1000, 2) if seconds else None,
            "p99_ms": round(percentile(seconds, 99) * 1000, 2) if seconds else None,
            "max_ms": round(max(seconds) * 1000, 2) if seconds else None}


def command_key(rec):
    key = rec["cmd"]
    if "sub" in rec:
        key += " " + rec["sub"]
    return key


def summarize_wire(paths):
    """Command mix, options and every error reply from tap logs."""
    counts = collections.Counter()
    options = collections.defaultdict(set)
    errors = collections.Counter()
    connections = 0
    unavailable = 0
    for path in paths:
        with Path(path).open() as log:
            for line in log:
                rec = json.loads(line)
                event = rec.get("event")
                if event == "open":
                    connections += 1
                elif event == "upstream_unavailable":
                    unavailable += 1
                if "cmd" not in rec:
                    continue
                key = command_key(rec)
                if rec["cmd"] == "HELLO":
                    key = "HELLO"
                counts[key] += 1
                options[key].update(rec.get("opts", ()))
                if "error" in rec:
                    errors[(key, rec["error"])] += 1
    unsupported = sorted({key for (key, error) in errors
                          if "unknown command" in error or "unknown subcommand" in error})
    return {
        "connections": connections,
        "connections_refused_while_server_down": unavailable,
        "commands": {key: {"count": counts[key], "options": sorted(options[key])} for key in sorted(counts)},
        "errors": [{"command": key, "error": error, "count": n} for (key, error), n in sorted(errors.items())],
        "unsupported": unsupported,
        "unsupported_outside_handshake": [key for key in unsupported if key not in HANDSHAKE_COMMANDS],
    }


def tool_versions():
    def run(*cmd):
        try:
            out = subprocess.run(cmd, capture_output=True, text=True, timeout=30)
            return (out.stdout or out.stderr).strip().splitlines()[0] if (out.stdout or out.stderr) else ""
        except (OSError, subprocess.TimeoutExpired):
            return None
    return {"go": run("go", "version"), "git": run("git", "--version"),
            "python": sys.version.split()[0], "kernel": run("uname", "-sr"),
            "runner_image": os.environ.get("ImageOS", "") + " " + os.environ.get("ImageVersion", "")}


def write_json(path, data):
    Path(path).write_text(json.dumps(data, indent=2, sort_keys=True) + "\n")
