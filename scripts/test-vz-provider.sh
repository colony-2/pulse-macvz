#!/usr/bin/env bash
# Real provider lifecycle checks on disposable copies of stopped cold bases.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
if [[ $# -lt 1 || $# -gt 2 || $(uname -s) != Darwin || $(uname -m) != arm64 ]]; then
  echo 'Usage: scripts/test-vz-provider.sh /absolute/linux-base [/absolute/macos-base]' >&2
  exit 2
fi
[[ $1 == /* && ${2:-/} == /* ]] || { echo 'Base paths must be absolute.' >&2; exit 2; }
mkdir -p test-results
out=$(mktemp -d "$root/test-results/vz-provider-run.XXXXXX")
chmod 700 "$out"
export CGO_ENABLED=0
export PULSE_VZ_RUNNER="$out/pulse-vz"
export PULSE_VZ_LINUX_BASE="$1"
export PULSE_VZ_MACOS_BASE="${2:-}"
export PULSE_VZ_RESULTS="$out"
bash scripts/build-vz-runner.sh "$PULSE_VZ_RUNNER"
printf 'Provider artifacts: %s\n' "$out"
go test -count=1 -timeout 30m -run '^TestLive(LinuxLifecycle|MacOSRestoreLifecycle|LinuxOCI)$' -v ./internal/vz 2>&1 | tee "$out/tests.log"
printf 'VZ_PROVIDER_PASS\n' > "$out/result.txt"
