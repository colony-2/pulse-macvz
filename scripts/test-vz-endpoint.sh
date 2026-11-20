#!/usr/bin/env bash
# VM-free launchd supervision check, including forwarding/restart/removal.
set -euo pipefail
umask 077
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
[[ $# == 0 ]] || { echo 'Usage: scripts/test-vz-endpoint.sh' >&2; exit 2; }
mkdir -p test-results
out=$(mktemp -d "$root/test-results/vz-endpoint.XXXXXX")
export CGO_ENABLED=0 PULSE_VZ_RUNNER="$out/pulse-vz" PULSE_TEST_OUTPUT_DIR="$out"
bash scripts/build-vz-runner.sh "$PULSE_VZ_RUNNER"
printf 'Endpoint artifacts: %s\n' "$out"
status=0
PULSE_TEST_VZ_DOCKER_ENDPOINT=1 go test ./internal/local -run '^TestLiveVZDockerEndpoint$' -v -count=1 -timeout 90s 2>&1 | tee "$out/tests.log" || status=$?
printf 'endpoint exit=%s\n' "$status" > "$out/result.txt"
exit "$status"
