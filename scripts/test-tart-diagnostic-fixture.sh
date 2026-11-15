#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
fixture_dir=$(mktemp -d tests/integration/diagnostic-copy-XXXXXX)
trap 'rm -rf "$fixture_dir"' EXIT
sed 's/^package diagnosticfixture$/package providers/' tests/integration/diagnosticfixture/fixture.go > "$fixture_dir/fixture.go"
go test "./$fixture_dir"
go test -race -count=1 -run '^TestTartDiagnosticRuntime$' .
