#!/usr/bin/env bash
# Build the standalone provider server and sign its embedded VZ runner on macOS.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
if [[ $# != 1 || $1 != /* ]]; then
  echo 'Usage: scripts/build-server.sh /absolute/output/pulse-macvz' >&2
  exit 2
fi
mkdir -p "$(dirname "$1")"
cd "$root"
source "$root/scripts/macos-app-identity.sh"
if [[ $(uname -s)/$(uname -m) == Darwin/arm64 ]]; then
  server_uuid=$(uuidgen | tr -d '-')
  CGO_ENABLED=0 go build -ldflags="-B 0x$server_uuid" -o "$1" ./cmd/pulse-macvz
  bash scripts/sign-macos.sh "$1" "$pulse_app_id"
else
  CGO_ENABLED=0 go build -o "$1" ./cmd/pulse-macvz
fi
