#!/usr/bin/env bash
# Public provider API + real JobDB contract. Uses a private VZ home.
# Optional prepared base is read-only; integration tools are installed in a clone.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
if [[ $# -gt 1 || $(uname -s) != Darwin || $(uname -m) != arm64 ]]; then
  echo 'Usage: scripts/test-vz-public.sh [/absolute/prepared-macos-base]' >&2
  exit 2
fi
[[ ${1:-/} == /* ]] || { echo 'Base path must be absolute.' >&2; exit 2; }
mkdir -p test-results
out=$(mktemp -d "$root/test-results/vz-public.XXXXXX")
chmod 700 "$out"
export CGO_ENABLED=0
export PULSE_TEST_BACKEND=vz
export PULSE_VZ_HOME="$out/home"
export PULSE_VZ_RUNNER="$out/pulse-vz"
export PULSE_VZ_PREPARED_BASE="${1:-}"
export PULSE_TEST_OUTPUT_DIR="$out"
bash scripts/build-vz-runner.sh "$PULSE_VZ_RUNNER"
printf 'Public API artifacts: %s\n' "$out"
go test . -run '^TestLive$' -count=1 -v -timeout 50m 2>&1 | tee "$out/tests.log"
printf 'VZ_PUBLIC_PASS\n' > "$out/result.txt"
