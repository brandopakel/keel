#!/bin/sh
# api-compat.sh BASE_DIR HEAD_DIR OUT_DIR: compare the public package keel's
# API in two checkouts with apidiff, pinned, and write the report to OUT_DIR.
# Exit status: 0 when the head's API is compatible with the base's, or the
# base has no package keel yet; 3 when it is not.
#
# Pre-1.0, a minor release may break the API, with migration notes
# (docs/public-api.md); a pull request that means to carries the label
# api-break, which the workflow passes as ALLOW_BREAK=1.
set -eu
base=$1 head=$2 out=$3
mkdir -p "$out"
apidiff=${APIDIFF:-apidiff}
(cd "$head" && "$apidiff" -w "$out/head.api" github.com/brandopakel/keel)
if ! grep -qs '^package keel$' "$base"/*.go; then
    echo "The base has no package keel: its whole API is new." | tee "$out/report.txt"
    exit 0
fi
(cd "$base" && "$apidiff" -w "$out/base.api" github.com/brandopakel/keel)
"$apidiff" "$out/base.api" "$out/head.api" > "$out/report.txt"
"$apidiff" -incompatible "$out/base.api" "$out/head.api" > "$out/incompatible.txt"
cat "$out/report.txt"
if [ -s "$out/incompatible.txt" ]; then
    if [ "${ALLOW_BREAK:-0}" = 1 ]; then
        echo "Incompatible changes, allowed by the label api-break."
        exit 0
    fi
    echo "Incompatible changes to package keel; label the pull request api-break and add migration notes if they are meant."
    exit 3
fi
echo "Compatible."
