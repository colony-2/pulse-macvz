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

## Upgrade recovery, 2026-11-22 UTC

- [x] Diagnose macOS 15 base failure: initial guest time was October 3, causing the Nix installer download to reject a not-yet-valid TLS certificate. A later launchd attempt finished successfully on November 22 (exit 0 and ready receipt), but retained the first attempt's failed receipt. The observer correctly rejected contradictory receipts.
- [x] Make each macOS builder service attempt invalidate prior ready/failed receipts before preparation. Regression tests execute the service path with isolated fake guest paths, covering successful retry after failure and failed retry after success. TLS verification remains enabled.
- [x] Verify current macOS 15 tools, OS, ready receipt, completed service and absence of accepted job state; preserve the stale failed receipt under a diagnostic name, then recover readiness through normal setup.
- [x] Retire only the two stopped, unused builders whose delivery expired before any exec: companion UID `e57e8f590fe2cc6ba5ce9ef520757acf` and macOS 26 UID `a0f80fa6229f20762f99aefd994d8bb8`. Validate owner, UID, image phase, absence of job/pair, no possible execution and native stopped state under the controller lock. Preserve metadata locally. Keep completed bases and recovered Linux allocations.
- [x] Install the signed receipt fix and restart normal setup. Linux's four idle allocations, both macOS 15 suspended templates and the companion base recovered.
- [x] Finish macOS 26 preparation/templates and verify repeat setup preserves all prepared VM identities. Recovery setup took about 5m17s; repeat setup completed immediately. All eight slots report c2j `unstable-98fcda4`; direct checks of all four Linux guests confirm KVM read/write access and Docker stopped. Receipt regression tests, local/provider/servercli Go tests, vet and shell syntax checks passed. Evidence: `.local/pool-recovery-1121b/`.

## Preparation reliability follow-through, 2026-11-22 UTC

- [x] Implement the [reliability plan](preparation-reliability-plan.md): fenced attempt receipts, bounded maintenance-only builder replacement, independent preparation, durable upgrade checkpoints, interruption recovery and rollback.
- [x] Test full Go suite, targeted race/fault tests, shell receipt execution, vet and signed installation. Record evidence in `.local/preparation-reliability/`.
- [x] Verify the running pool without interruption: four idle Linux workers and four ready macOS templates; slot/image snapshots unchanged across installation. The provider holds the pool lock; no setup, live destructive qualification, server restart, or accepted-job mutation performed.
