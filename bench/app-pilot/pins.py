#!/usr/bin/env python3
"""Read the pilot's pinned versions and fetch pinned source trees.

Every outside project the pilot runs is named once, in pins.json, by exact
commit. Fetching by commit rather than branch or tag means a rerun builds the
same code, and a tag that is later moved cannot change what was measured.
"""
import argparse
import json
import shutil
import subprocess
import sys
from pathlib import Path

PINS = json.loads((Path(__file__).resolve().parent / "pins.json").read_text())


def lookup(dotted):
    value = PINS
    for part in dotted.split("."):
        value = value[part]
    return value


def fetch(repo, commit, dest, tag=None):
    """Check out exactly `commit` into `dest`, verifying the tag if one is named."""
    dest = Path(dest)
    if (dest / ".git").exists():
        head = subprocess.run(["git", "-C", str(dest), "rev-parse", "--verify", "-q", "HEAD"],
                              capture_output=True, text=True).stdout.strip()
        if head == commit:
            return
        if head:
            raise SystemExit(f"{dest} is at {head}, not the pinned {commit}")
        # An earlier fetch was interrupted after `git init`: nothing was checked
        # out, so start again rather than fail on every rerun.
        shutil.rmtree(dest)
    dest.mkdir(parents=True, exist_ok=True)
    git = ["git", "-C", str(dest)]
    subprocess.run(git + ["init", "-q"], check=True)
    subprocess.run(git + ["remote", "add", "origin", repo], check=True)
    subprocess.run(git + ["fetch", "-q", "--depth", "1", "origin", commit], check=True)
    subprocess.run(git + ["checkout", "-q", "--detach", "FETCH_HEAD"], check=True)
    if tag:
        # The tag is fetched separately and must still name the pinned commit;
        # build tooling such as Gitea's version string reads it.
        subprocess.run(git + ["fetch", "-q", "--depth", "1", "origin", f"refs/tags/{tag}:refs/tags/{tag}"],
                       check=True)
        tagged = subprocess.run(git + ["rev-parse", f"{tag}^{{commit}}"],
                                capture_output=True, text=True, check=True).stdout.strip()
        if tagged != commit:
            raise SystemExit(f"tag {tag} now names {tagged}, not the pinned {commit}")


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = ap.add_subparsers(dest="action", required=True)
    get = sub.add_parser("get", help="print one pinned value, e.g. redis.sha256")
    get.add_argument("key")
    one = sub.add_parser("fetch", help="fetch one checkout, or gitea_app, into a directory")
    one.add_argument("name")
    one.add_argument("dest")
    every = sub.add_parser("fetch-checkouts", help="fetch every regression checkout under a directory")
    every.add_argument("root")
    args = ap.parse_args()
    if args.action == "get":
        print(lookup(args.key))
    elif args.action == "fetch":
        pin = PINS["gitea_app"] if args.name == "gitea_app" else PINS["checkouts"][args.name]
        fetch(pin["repo"], pin["commit"], args.dest, pin.get("tag"))
    else:
        for name, pin in PINS["checkouts"].items():
            fetch(pin["repo"], pin["commit"], Path(args.root) / name)
    return 0


if __name__ == "__main__":
    sys.exit(main())
