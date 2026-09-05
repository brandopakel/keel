#!/usr/bin/env bash
# Start an already registered, sandboxed Bencher runner on a controlled host.
# Runner/Spec registration and hardware selection belong to the owner's account.
set -euo pipefail
: "${BENCHER_HOST:?set the Bencher API endpoint}"
: "${BENCHER_RUNNER:?set the registered runner UUID or slug}"
: "${BENCHER_RUNNER_KEY:?inject the runner key from the host secret store}"
if [[ $(uname -s) != Linux || ! -r /dev/kvm || ! -w /dev/kvm ]]; then
  echo 'This runner profile requires Linux with readable/writable /dev/kvm for Firecracker.' >&2
  exit 1
fi
runner --version
# Keep a benchmark series on the installed, verified runner build. Upgrade it
# deliberately between series and record the new version and executable hash.
exec runner up --no-auto-update
