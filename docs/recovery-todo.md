# Provider recovery TODO

Incident: 2026-11-21. Restore the server pool before further feature work.

## Restore service

- [x] Verify JobDB job `3MP8NgqQcE0kajyOrq7853x7Dq6` is cancelled and stop only its stale Linux VM (UID `f73fdde175df4dd176ebb3979b59fd78`). Runner exited 0.
- [x] Confirm upgrade drained the stale allocation and completed both macOS bases and three templates.
- [x] Recover Ubuntu base `pulse-server-id-ca1c953085473b708781-base`: added apt lock wait and restarted the failed build service in place. Verified ready receipt, recorded readiness through the new observer, and shut down cleanly.
- [x] Recover macOS 26 template `standard-0001`: retired only verified unused preparing VM UID `07542cbb8e2aa8a6f46e569ecf695c5c`; recreated from the existing ready base and suspended successfully. Other ready templates preserved.
- [x] Complete setup and verify four idle Linux allocations and four ready suspended macOS templates. All Linux guests independently answered SSH, reported c2j `unstable-5b70b0a` and helper `0.2.0`, allowed pulseworker read/write access to `/dev/kvm`, and had Docker stopped.
- [ ] Restart/verify the reverse connection. Saved `provider.json` has no collector or authentication settings; requested the existing launch command/config path.

## Correctness and diagnostics

- [x] Wait up to 300 seconds for the dpkg lock during Ubuntu worker and Docker companion package installation.
- [x] Observe the Ubuntu builder's systemd failure/ready receipt so setup reports a failed build instead of indefinitely waiting on uncertain delivery. Added recovery tests and live verified ready recovery.
- [x] Extend failure/receipt recovery to macOS templates and companion builders. Template receipt recovery validates the current boot UUID and configured MAC, persists the verified receipt, and leaves suspension to normal reconciliation. Companion builders persist a ready marker; older builders can be observed through their service journal. Neither path replays startup or accepted work. Tests cover failure, expiry, readiness, and stale/mismatched receipts.
- [ ] Establish the root cause of the old macOS template's lost connectivity. Evidence establishes an alive runner, SSH refusal, no complete host receipt, and successful replacement from the same ready base. It does not establish why the old guest stopped answering. Do not conflate this with the confirmed Ubuntu apt failure.
- [x] Treat temporary deletion blocked by a delivery worker as deferred cleanup. Only a verified stopped VM's delivery is cancelled; reconciliation retains its reservation and retries without failing the reverse session. Tests cover cancellation, unrelated cleanup, and paired slot/port retention.
- [x] Include VM identity in all local deletion errors and launch identity in job/allocation cleanup errors.
- [x] Distinguish uncertain handoff from an assignment in `status`; explain that VM liveness is not JobDB execution status.
- [ ] Automate recovery of orphan assignments whose JobDB jobs are terminal. The pinned provider API exposes capabilities, submit, list, and logging, but no cancellation operation or terminal-job feed. This needs an explicit controller/worker contract or a separately authorized administrative recovery operation; no age-based reaping or JobDB polling was added.

## Pending feature work

- [x] Review and commit the implemented/tested launch history and log CLI commands alongside the recovery fixes.
- [x] Full `CGO_ENABLED=0 go test ./...`, targeted recovery race tests, `go vet ./...`, shell/Python syntax checks, and live setup/readiness checks passed. Installed signed CLI. A second setup completed immediately with all eight slot VM identities unchanged. Evidence: `.local/pool-recovery/` (`all-recovery-tests.txt`, `new-recovery-race.txt`, `recovery-setup.txt`, before/after status snapshots). New template/companion lost-connection paths have regression coverage; no fresh full VM qualification was run in the ready pool.
- [x] Commit recovery work at the user's request. No publication/push is authorized.

Do not rebuild completed images unnecessarily, change accepted jobs, or modify the protected candidate `8187fb785dc8b076f32059929c7dbb7a6651429b` / excluded job `Uv7dTrifueEo48PogJxlTtjRwpt`.
