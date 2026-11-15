# Run Tart placement diagnostics

The helper in [diagnosticfixture/fixture.go](diagnosticfixture/fixture.go) runs the real exported Tart runtime against loopback Orchard and SSH. It creates only temporary state and synthetic backend objects. It needs Go 1.26 and the dependencies already in this module. No Mac, provider credentials, installer download or capacity is required.

From this repository, run:

```sh
./scripts/test-tart-diagnostic-fixture.sh
go test -count=1 -run '^TestDiagnostic' ./internal/provider ./internal/orchard ./internal/tart ./internal/guestconn
go test -race -count=1 -run '^TestTartDiagnosticRuntime$' .
```

The script copies the helper to a temporary package named `providers`, compiles it, removes that copy, and runs the runtime cases with the race detector. The ordinary source uses only the exported Tart package, the shared provider API, standard library and SSH/WebSocket dependencies. It can also be imported directly by tests outside Pulse. It is test support, not a supported provider API.

## Embed in Pulse

Use the candidate commit SHA published and verified by the managed recipe for this change. The starting revision `cab64cd040976a3aa34b605a3d0abe0a35d8c49b` does **not** contain this fix. Do not use that SHA or the older Pulse baseline `f85d83f` as the diagnostic delivery.

After the recipe supplies the Tart candidate SHA, run these commands in Pulse's checkout. Set `TART_DIAGNOSTIC_REVISION` to that full SHA first:

```sh
: "${TART_DIAGNOSTIC_REVISION:?set the verified Tart candidate commit SHA}"
go get "github.com/colony-2/pulse-tart@${TART_DIAGNOSTIC_REVISION}"
tart_module_dir=$(go list -m -f '{{.Dir}}' github.com/colony-2/pulse-tart)
sed 's/^package diagnosticfixture$/package providers/' \
  "$tart_module_dir/tests/integration/diagnosticfixture/fixture.go" \
  > internal/providers/tart_diagnostic_fixture_test.go
go test -count=1 -run '^$' ./internal/providers
```

This uses Go's resolved module directory rather than a machine-specific cache path or replacement. Keep the helper as `_test.go` inside Pulse. Put the combined tests in `internal/providers/tart_diagnostics_test.go`, package `providers`, as agreed with Pulse's owner. The package-copy compilation and local runtime commands above have been executed here; the future `go get` and Pulse commands need the recipe's published candidate.

Construct `NewTartDiagnosticFixture(t, false)` for ordinary cases and `true` for Docker. It exposes `Runtime`, resolved `Config`, `Base`, `Capabilities`, `Scenario`, `Snapshot`, `Transcript`, `Ready`, `Mixed`, `Launch` and `Close`. `Launch` is a synthetic compatible example; adapt the controller's acquired demand and defaults to the same base, 4,000 millicores and 8 GiB ordinary / 7.5 GiB Docker memory, with supported capabilities. Leave scratch, timeout and start window unset.

Create a bounded context, call `Ready(ctx)`, and assert its error. Setup adopts stopped prepared bases and suspended templates through normal `Reconcile`, checking `Images`, `Slots` and Docker allocation phases. It never edits the journal. Select the fault only after setup, except the preparation case below. Compare counter deltas after setup, since constructor and readiness traffic are separate. `Transcript` contains method, object role and count only. `Close` releases the runtime before closing connections and endpoints; registered test cleanup also closes it.

| Case | Setup and scenario | Required comparison |
| --- | --- | --- |
| D01 inventory | `Ready(ctx)`, then `Scenario("inventory")` | Unavailable; exact `TartDiagnosticInventory`, including newline/quotes and 503, retained in Reason; zero `POST job`. |
| D01 mixed | `Ready(ctx)`, then `Mixed(ctx)` | No capacity; occupied reservation, source-referencing clone and invalid suspension/quarantine all retained; zero additional `POST job`. |
| D02 preparation | `Scenario("preparation")` before `Ready` or `Reconcile` | Real error includes `TartDiagnosticPreparation` and 503. Assert failure, then change `Scenario("")` before explicit recovery. |
| D02 uncertain create | `Ready(ctx)`, then `Scenario("create")` | Unknown; exact `TartDiagnosticCreate`; one `POST job`, with a synthetic created object retained server-side. |
| Accepted control | `Ready(ctx)`, then `Scenario("accepted")` | Accepted; empty Reason; one `POST job`; Close does not delete it. |

The real runtime tests also cover `websocket`, `handshake`, `session`, `readiness-error`, `readiness-marker`, `handoff-error`, `handoff-marker` and `docker-accepted`. SSH consumes synthetic scripts as stdin without executing or recording them. Failure replies use protocol errors; successful replies use fixed markers. Missing markers are reported by name without copying other guest output.

## D01–D02 assertions in Pulse

Wrap the real runtime with a recorder that delegates unchanged calls and stores the exact launches, submissions and method errors in test memory. Assign it to the real `macProvider` shim, then invoke the actual scheduler and controller. Embed the shim in a test-only wrapper overriding only `Maintain`: ordinary cases perform no maintenance. Avoiding `Runtime.Run` alone does not prevent `Controller.Once` from starting reconciliation. Keep real `SubmitWithDiagnostics` and normal closure.

For preparation, first assert the real `Reconcile` error. Have that test-only `Maintain` return the saved error to exercise the existing maintenance logger. Decode the JSON stream by `msg`; keep maintenance and launch records separate. This checks error capture, not the asynchronous maintenance loop.

D01 compares decoded `attempts[].original_results[].reason` against the exact runtime Reason, including raw status/body. Compare top-level Reason when it is the final refusal. Assert provider identity, existing correlation fields, effective allocation, informational severity, unknown structured capacity, zero native dispatch, lease release and the actual retry receipt/cooldown. Check single-service refusal and safe fallback separately: fallback acceptance keeps an empty top-level Reason, retains earlier refusal in attempts and retains the accepted lease.

D02 compares the uncertain runtime Reason through shim, scheduler and decoded controller record. Assert one create, uncertain admission, no fallback, no definite-non-start lease release and no retry deadline newly imposed by this attempt. Replay the recorder's **identical launch** directly through `Runtime.Submit` before reconciliation and compare counters: still unknown, no second POST. A second controller pass generates another launch ID and is not replay evidence. Accepted replay also keeps Reason empty and never dispatches twice. Keep separate method errors; authoritative per-item reasons can coexist with a nil Submit method error.

Discovery must add zero backend calls; compare snapshots. Diagnostics and fixture transcripts must exclude synthetic leases, encoded leases, complete launches, environment and scripts. Compare decoded strings, since JSON escaping preserves newlines and quotes.

After implementing the combined assertions, run in Pulse:

```sh
go test -race -count=1 -run '^TestTartDiagnosticD0[12]$' ./internal/providers
```

Local runtime tests cannot establish Pulse's fallback, lease-release, retry or controller-log assertions. Those D01–D02 results belong to Pulse after pinning this candidate.
