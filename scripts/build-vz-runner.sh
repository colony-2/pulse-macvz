#!/usr/bin/env bash
# Build and ad-hoc sign the standalone development runner; installs no service.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
if [[ $# != 1 || $1 != /* || $(uname -s)/$(uname -m) != Darwin/arm64 ]]; then
  echo 'Usage: scripts/build-vz-runner.sh /absolute/output/pulse-vz (Apple Silicon Mac)' >&2
  exit 2
fi
mkdir -p "$(dirname "$1")"
cd "$root"
CGO_ENABLED=0 go build -o "$1" ./cmd/pulse-vz
codesign --force --entitlements cmd/pulse-vz/vz.entitlements -s - "$1"
codesign --verify --strict "$1"
