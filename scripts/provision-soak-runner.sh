#!/usr/bin/env bash
# Turn a fresh Ubuntu machine into the self-hosted runner the true-uptime soak
# needs: one GitHub Actions runner, labelled `soak`, running as a service under
# an unprivileged user. Written for an Oracle Cloud Always Free Ampere A1 VM,
# which is the machine this project can afford; any Ubuntu 22.04+ on arm64 or
# x86_64 works the same. See docs/long-run-gate.md for the steps around it.
#
#   sudo ./provision-soak-runner.sh --repo brandopakel/keel --token <registration token>
#
# The registration token comes from
#   gh api -X POST repos/brandopakel/keel/actions/runners/registration-token -q .token
# and is valid for one hour; nothing long-lived is left on the machine. The
# runner is not ephemeral: a persistent runner is what lets a weekly schedule
# find it. What keeps a persistent runner on a public repository safe is the
# repository's Actions setting requiring approval for every outside
# collaborator's workflow, which is set, and the read-only default token.
set -euo pipefail

REPO=""
TOKEN=""
LABELS="soak"
RUNNER_USER="runner"
RUNNER_HOME="/opt/actions-runner"
VERSION=""

usage() { sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --repo) REPO="$2"; shift 2 ;;
    --token) TOKEN="$2"; shift 2 ;;
    --labels) LABELS="$2"; shift 2 ;;
    --version) VERSION="$2"; shift 2 ;;
    -h|--help) usage 0 ;;
    *) echo "unknown argument: $1" >&2; usage 1 ;;
  esac
done
[ -n "$REPO" ] && [ -n "$TOKEN" ] || { echo "--repo and --token are required" >&2; usage 1; }
[ "$(id -u)" -eq 0 ] || { echo "run as root: it installs packages and a service" >&2; exit 1; }

case "$(uname -m)" in
  aarch64|arm64) ARCH=arm64 ;;
  x86_64|amd64) ARCH=x64 ;;
  *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

# What the soak workflow needs on the machine: the runner's own dependencies,
# python3 for the harness, and git for checkout. Go arrives through setup-go.
export DEBIAN_FRONTEND=noninteractive
apt-get update -q
apt-get install -y -q curl tar git jq python3 ca-certificates libicu-dev
# The runner's dependency script knows the rest (libssl, krb5, ...).

# The Always Free x86 shape has 1 GB. The Go build peaks past that, and a
# runner job that dies in the compiler is a wasted week. Two gigabytes of swap
# cost nothing but boot-volume space and let the build finish, slowly.
if [ "$(awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo)" -lt 2048 ] && [ ! -f /swapfile ]; then
  fallocate -l 2G /swapfile
  chmod 600 /swapfile
  mkswap /swapfile >/dev/null
  swapon /swapfile
  echo '/swapfile none swap sw 0 0' >> /etc/fstab
fi

# Ubuntu's unattended-upgrades runs daily and lets needrestart restart any
# service that maps an upgraded library. That includes the runner, and a
# restarted runner cancels the job it is running - which is how the first
# 48-hour attempt ended at nine hours, healthy, on 2026-09-18. Security
# updates stay on; the runner is exempt, and the machine never reboots itself.
mkdir -p /etc/needrestart/conf.d
cat > /etc/needrestart/conf.d/actions-runner.conf <<'NR'
# The GitHub Actions runner hosts multi-day jobs; a restart cancels them.
$nrconf{override_rc}{qr(^actions\.runner)} = 0;
NR
echo 'Unattended-Upgrade::Automatic-Reboot "false";' > /etc/apt/apt.conf.d/52soak-runner-no-reboot

id "$RUNNER_USER" >/dev/null 2>&1 || useradd --system --create-home --shell /bin/bash "$RUNNER_USER"
mkdir -p "$RUNNER_HOME"
chown "$RUNNER_USER:$RUNNER_USER" "$RUNNER_HOME"

# Latest runner release unless pinned, verified against the checksum GitHub
# publishes in the release notes.
api="https://api.github.com/repos/actions/runner/releases"
if [ -z "$VERSION" ]; then
  VERSION="$(curl -fsSL "$api/latest" | jq -r .tag_name | sed 's/^v//')"
fi
release_json="$(curl -fsSL "$api/tags/v$VERSION")"
asset="actions-runner-linux-$ARCH-$VERSION.tar.gz"
expected="$(printf '%s' "$release_json" | jq -r .body | sed -n "s/.*BEGIN SHA linux-$ARCH -->\([0-9a-f]\{64\}\)<!-- END SHA linux-$ARCH.*/\1/p" | head -1)"
[ -n "$expected" ] || { echo "no published checksum for $asset in release v$VERSION" >&2; exit 1; }
tmp="$(mktemp -d)"
curl -fsSL -o "$tmp/$asset" "https://github.com/actions/runner/releases/download/v$VERSION/$asset"
echo "$expected  $tmp/$asset" | sha256sum -c -
tar -xzf "$tmp/$asset" -C "$RUNNER_HOME"
rm -rf "$tmp"
chown -R "$RUNNER_USER:$RUNNER_USER" "$RUNNER_HOME"
"$RUNNER_HOME/bin/installdependencies.sh"

# Register, then install and start the service. --replace lets this script be
# rerun on the same machine; --unattended never prompts.
cd "$RUNNER_HOME"
sudo -u "$RUNNER_USER" ./config.sh --unattended --replace \
  --url "https://github.com/$REPO" --token "$TOKEN" \
  --name "soak-$(hostname)" --labels "$LABELS" --work _work
./svc.sh install "$RUNNER_USER"
./svc.sh start
./svc.sh status | head -5

cat <<MSG

Runner registered to $REPO with labels [$LABELS], running as a service.

Next, once:   gh variable set SOAK_RUNNER --repo $REPO --body soak
Then the weekly true-uptime soak in scheduled-soak.yml runs here on its own.
A short check that this runner works:
  gh workflow run scheduled-soak.yml --repo $REPO -f runner=soak -f seconds=600 -f timeout_minutes=60
The full 48 hours, now:
  gh workflow run scheduled-soak.yml --repo $REPO -f uptime=true -f runner=soak
MSG
