#!/usr/bin/env bash
# Full production Docker allocation fault/pressure suite using a private VZ home.
set -euo pipefail
umask 077
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
if [[ $# -gt 1 || ${1:-/} != /* || $(uname -s)/$(uname -m) != Darwin/arm64 ]]; then
  echo 'Usage: scripts/test-vz-docker-qualification.sh [/absolute/prepared-macos-base]' >&2
  exit 2
fi
mkdir -p test-results
out=$(mktemp -d "$root/test-results/vz-docker.XXXXXX")
printf 'VZ Docker artifacts: %s\n' "$out"
printf 'Up to two macOS and two Linux VMs, 16 vCPUs / 32 GiB; allow 30–60 minutes plus downloads.\n'
python3 scripts/test-report.py snapshot "$out"
{ date -u; git rev-parse HEAD; go version; uname -sm; } > "$out/environment.log" 2>&1
cd "$out/source"
export CGO_ENABLED=0 GOWORK=off
export PULSE_VZ_HOME="$out/home" PULSE_VZ_RUNNER="$out/pulse-vz"
export PULSE_VZ_PREPARED_BASE="${1:-}" PULSE_TEST_OUTPUT_DIR="$out"
export PULSE_TEST_QUALIFICATION_BACKEND=vz
export PULSE_TEST_ENGINE_WORKER_ADDRESS=${PULSE_TEST_ENGINE_WORKER_ADDRESS:-192.168.64.1}
unset PULSE_TEST_BACKEND PULSE_TEST_DOCKER_LIBRARY PULSE_TEST_DOCKER_QUALIFICATION PULSE_QUALIFICATION_CONTROLLER
status=0
if ! python3 - "$out" > "$out/preflight.log" 2>&1 <<'PY'
import shutil, sys
assert shutil.disk_usage(sys.argv[1]).free >= 40*2**30, 'Need at least 40 GiB free for this suite.'
PY
then cat "$out/preflight.log" >&2; status=2; fi
if [[ $status == 0 ]]; then
  bash scripts/build-vz-runner.sh "$PULSE_VZ_RUNNER" > "$out/build.log" 2>&1 || status=2
fi
if [[ $status == 0 ]]; then
  printf 'Checking launchd before creating any VMs.\n'
  PULSE_TEST_VZ_DOCKER_ENDPOINT=1 go test ./internal/local -run '^TestLiveVZDockerEndpoint$' -v -count=1 -timeout 90s 2>&1 | tee "$out/endpoint.log" || status=2
fi
if [[ $status == 0 ]]; then
  PULSE_TEST_DOCKER_QUALIFICATION=1 go test . -run '^TestLiveDockerQualification$' -json -count=1 -timeout 100m 2>&1 | tee "$out/tests.jsonl" | python3 scripts/test-report.py stream "$out/tests.log" || status=2
fi
python3 scripts/docker-qualification/report.py "$out" "$status" vz || status=2
# Keep VM disks local. The archive contains the exact sources and test evidence.
tar --exclude="$(basename "$out")/home" --exclude="$(basename "$out")/pulse-vz" -czf "$out.tar.gz" -C "$(dirname "$out")" "$(basename "$out")"
printf 'Report: %s/report.json\nEvidence archive: %s.tar.gz\n' "$out" "$out"
exit "$status"
