#!/usr/bin/env bash
# Build the standalone provider server and sign its embedded VZ runner on macOS.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
if [[ $# != 1 || $1 != /* ]]; then
  echo 'Usage: scripts/build-server.sh /absolute/output/pulse-tart' >&2
  exit 2
fi
mkdir -p "$(dirname "$1")"
cd "$root"
if [[ $(uname -s)/$(uname -m) == Darwin/arm64 ]]; then
  server_uuid=$(uuidgen | tr -d '-')
  CGO_ENABLED=0 go build -ldflags="-B 0x$server_uuid" -o "$1" ./cmd/pulse-tart
  bash scripts/sign-macos.sh "$1" dev.pulse.provider
else
  CGO_ENABLED=0 go build -o "$1" ./cmd/pulse-tart
fi
