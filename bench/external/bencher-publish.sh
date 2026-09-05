#!/usr/bin/env bash
set -euo pipefail
: "${BENCHER_PROJECT:?set the existing project slug}"
: "${BENCHER_API_KEY:?set the token in the environment}"
: "${BENCHER_TESTBED:?set a stable hardware/configuration testbed name}"
: "${BENCHER_BRANCH:?set the source branch}"
: "${BENCHER_HASH:?set the exact source commit}"
: "${1:?pass the exported BMF JSON path}"
exec bencher run --error-on-alert --adapter json --project "$BENCHER_PROJECT" \
  --testbed "$BENCHER_TESTBED" --branch "$BENCHER_BRANCH" --hash "$BENCHER_HASH" --file "$1"
