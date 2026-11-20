#!/usr/bin/env bash
# Build and sign the standalone development runner; installs no service.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
if [[ $# != 1 || $1 != /* || $(uname -s)/$(uname -m) != Darwin/arm64 ]]; then
  echo 'Usage: scripts/build-vz-runner.sh /absolute/output/pulse-vz (Apple Silicon Mac)' >&2
  exit 2
fi
mkdir -p "$(dirname "$1")"
cd "$root"
# Signing changes do not change Go's default Mach-O UUID. Never reuse that
# UUID between ad-hoc and certificate-signed variants (Apple TN3178/TN3179).
runner_uuid=$(uuidgen | tr -d '-')
CGO_ENABLED=0 go build -ldflags="-B 0x$runner_uuid" -o "$1" ./cmd/pulse-vz
bash scripts/sign-macos.sh "$1" dev.pulse.vz.runner
