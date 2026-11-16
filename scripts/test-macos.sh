#!/bin/bash
# Run every test, including real VM/JobDB execution, and retain shareable results.
# Parse the whole driver before running it so edits cannot change an active run.
main() {
set -euo pipefail
umask 077
cd "$(dirname "$0")/.."
backend=${1:-${PULSE_TEST_BACKEND:-tart}}
if [[ $# -gt 1 || ( $backend != tart && $backend != orchard ) ]]; then
  echo 'Usage: ./scripts/test-macos.sh [tart|orchard]' >&2
  exit 2
fi
for command in go python3 git; do command -v "$command" >/dev/null || { echo "Missing command: $command" >&2; exit 2; }; done
out="$PWD/test-results/$(date -u +%Y%m%dT%H%M%SZ)-$backend-$$"
mkdir -p "$out"
# Always retain a shareable archive, including failures before Go starts.
finish() {
  status=$?
  trap - EXIT
  if [[ ! -f "$out/summary.json" ]]; then
    python3 - "$out/summary.json" "$backend" "$status" <<'PY'
import json, pathlib, sys
pathlib.Path(sys.argv[1]).write_text(json.dumps({
    'result': 'failed', 'backend': sys.argv[2], 'command_exit_code': int(sys.argv[3]),
    'reason': 'Stopped before live coverage could be verified; see logs.'
}, indent=2) + '\n')
PY
  fi
  tar -czf "$out.tar.gz" -C "$(dirname "$out")" "$(basename "$out")" || exit 2
  echo "Send: $out.tar.gz"
  exit "$status"
}
trap finish EXIT
export PULSE_TEST_BACKEND="$backend"
export PULSE_TEST_OUTPUT_DIR="$out"
# Other opt-in suites provision their own VMs and must be run separately.
unset PULSE_TEST_DOCKER_LIBRARY PULSE_TEST_DOCKER_ENDPOINT PULSE_TEST_DOCKER_QUALIFICATION PULSE_TEST_QUALIFICATION_BACKEND
export PULSE_TEST_TART_HOME=${PULSE_TEST_TART_HOME:-${TART_HOME:-$HOME/.tart}}
echo "Results: $out"
{
  date -u
  echo "Backend: $backend"
  if [[ $backend == tart ]]; then echo "Tart home: $PULSE_TEST_TART_HOME"; fi
  if [[ $(uname -s) == Darwin ]]; then sw_vers; uname -m; sysctl hw.model hw.ncpu hw.memsize; else uname -sm; fi
  if command -v tart >/dev/null; then tart --version; fi
  go version
  git rev-parse HEAD
  git diff --stat
} 2>&1 | tee "$out/environment.log"
if [[ $backend == tart && ( $(uname -s) != Darwin || $(uname -m) != arm64 ) ]]; then
  echo 'Direct Tart live tests require an Apple Silicon Mac.' | tee "$out/preflight-error.log"
  exit 2
fi
if [[ $backend == tart ]]; then
  command -v tart >/dev/null || { echo 'Missing command: tart' | tee "$out/preflight-error.log"; exit 2; }
  [[ $PULSE_TEST_TART_HOME == /* ]] || { echo 'PULSE_TEST_TART_HOME must be absolute.' | tee "$out/preflight-error.log"; exit 2; }
  echo 'Run this suite alone, with Pulse paused: it uses both macOS VM slots.'
fi
if [[ $backend == orchard && ( -z ${PULSE_TEST_ORCHARD_URL:-} || -z ${PULSE_TEST_ORCHARD_WORKER:-} ) ]]; then
  echo 'Set PULSE_TEST_ORCHARD_URL and PULSE_TEST_ORCHARD_WORKER.' | tee "$out/preflight-error.log"
  exit 2
fi
python3 scripts/test-report.py snapshot "$out"
# Compile embedded assets and read comparison files from the same frozen tree.
cd "$out/source"
export GOWORK=off
echo 'Checking script regressions; log: python.log'
if ! python3 -m unittest discover -s scripts -p 'test_*test.py' > "$out/python.log" 2>&1; then
  cat "$out/python.log"
  exit 1
fi
# Do not add -run here: the full suite and the JobDB assertions must execute.
echo 'Running the full race suite and live VM/JobDB recovery checks.'
set +e
go test -json -race -v -count=1 -timeout=50m ./... 2> "$out/go-stderr.log" \
  | tee "$out/tests.jsonl" \
  | python3 scripts/test-report.py stream "$out/tests.log"
statuses=("${PIPESTATUS[@]}")
set -e
status=0
for code in "${statuses[@]}"; do if [[ $code != 0 ]]; then status=$code; break; fi; done
cat "$out/go-stderr.log"
set +e
python3 scripts/test-report.py summarize "$out" "$backend" "$status"
status=$?
set -e
# The EXIT trap archives only this output directory. Private VM properties and
# auxiliary fixture intents stay outside it.
exit "$status"
}
main "$@"
