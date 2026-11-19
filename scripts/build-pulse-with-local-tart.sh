#!/bin/bash
# Build Pulse with this checkout and its matching host health integration.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
source_repo=https://github.com/colony-2/pulse.git
ref=f9387b773994a5a4a151333c665b134660efd2b5
output="$root/dist/pulse"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --source) source_repo=$2; shift 2 ;;
    --ref) ref=$2; shift 2 ;;
    --output) output=$2; shift 2 ;;
    --help) echo 'Usage: build-pulse-with-local-tart.sh [--source CHECKOUT_OR_URL] [--ref COMMIT] [--output PATH]'; exit 0 ;;
    *) echo "Unknown argument: $1" >&2; exit 2 ;;
  esac
done
work=$(mktemp -d /tmp/pulse-local-build.XXXXXX)
trap 'rm -rf "$work"' EXIT
# An isolated clone leaves the supplied checkout and its local edits unchanged.
git clone --quiet --no-checkout "$source_repo" "$work/pulse"
git -C "$work/pulse" checkout --quiet --detach "$ref"
git -C "$work/pulse" apply --check "$root/integrations/pulse/maintenance-health.patch"
git -C "$work/pulse" apply "$root/integrations/pulse/maintenance-health.patch"
go -C "$work/pulse" mod edit "-replace=github.com/colony-2/pulse-tart=$root"
go -C "$work/pulse" mod tidy
go -C "$work/pulse" test ./internal/controller ./internal/httpapi
mkdir -p "$(dirname "$output")"
output=$(cd "$(dirname "$output")" && pwd)/$(basename "$output")
go -C "$work/pulse" build -o "$output" ./cmd/pulse
echo "Built Pulse with local Tart and maintenance health: $output"
echo 'Stop the old Pulse process and run this binary to use the changes.'
