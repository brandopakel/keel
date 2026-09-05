#!/usr/bin/env bash
# Build reviewable candidate archives. This script does not tag or publish.
set -euo pipefail
cd "$(dirname "$0")/.."
version=${1:-v0.1.0-alpha.3-dev}
case "$version" in *[!a-zA-Z0-9.+-]*|'') echo 'invalid version' >&2; exit 1;; esac
mkdir -p dist
for os in linux darwin; do
  for arch in amd64 arm64; do
    target="dist/keel_${version}_${os}_${arch}"
    mkdir -p "$target"
    GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/brandopakel/keel/internal/config.Version=$version" \
      -o "$target/keel" ./cmd/keel
    cp LICENSE THIRD_PARTY_NOTICES.md README.md "$target/"
    cp -R docs examples "$target/"
    mkdir -p "$target/bench"
    cp -R bench/external "$target/bench/"
    cp bench/run-tail.py "$target/bench/"
    tar --exclude='__pycache__' --exclude='*.pyc' -C "$target" -czf "$target.tar.gz" keel LICENSE THIRD_PARTY_NOTICES.md README.md docs examples bench
    shasum -a 256 "$target.tar.gz" > "$target.tar.gz.sha256"
  done
done
