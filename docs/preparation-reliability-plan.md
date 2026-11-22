# Preparation and upgrade reliability

## Objective

A failed or interrupted image build must not require manual edits to guest receipts or VM metadata. Never replay an accepted job, remove another owner's VM, or discard the last ready base before its replacement is validated.

## Execution plan

1. **Attempt receipts.** Invalidate prior terminal receipts before a builder service attempt. Keep strict failure-first observation and test retries after both success and failure.
2. **Bounded recovery.** During explicit setup/upgrade, replace failed unused builders through the native lifecycle. Require matching owner/UID and image properties, reject job/pair/quarantine records, stop and observe termination, wait for delivery workers, and persist a two-replacement budget across process restarts. Retain diagnostic evidence. Never replay uncertain guest execution.
3. **Failure isolation.** Keep reconciling unrelated bases/templates while a failed branch settles. Report aggregated terminal errors after independent work completes, with a bounded maintenance deadline.
4. **Recoverable upgrades.** After accepted work drains, move configured ready bases into a durable owner-scoped checkpoint before rebuilding. Keep their disks and metadata until all configured capacity is ready. Resume interrupted upgrades; on terminal preparation failure restore previous bases. Cancellation retains the checkpoint and partial progress. This preserves bases, not live jobs or uninterrupted availability: upgrades still drain the pool and templates may need to be recreated.
5. **Verification.** Add fault tests for receipt retries, retry-budget persistence, ownership exclusions, independent preparation, and checkpoint interruption/rollback. Run relevant Go/race/shell tests and the full Go suite. Install the signed CLI and perform read-only live inspection; repeat setup only if the provider is stopped; do not upgrade or rebuild that healthy production pool merely to test failure injection.

## Boundaries

No JobDB terminal-job inference, accepted-job replay, protected-candidate changes, or publication. Disk/identity ambiguity fails closed and retains the checkpoint. Tests must distinguish proven recovery from paths that still require live qualification.

## Execution record

- [x] Attempt receipts: macOS base services write a durable attempt token, atomically publish terminal receipts and fence stale timeout/error handlers. Linux base retries remove old ready receipts; macOS template retries remove old failed receipts.
- [x] Maintenance recovery: at most two replacements per name, durable budget and private failure metadata; matching native spec/UID plus independent rejection of job/pair/quarantine properties. Stop and join delivery/observation before deletion. Progress shows the replacement count.
- [x] Failure isolation: registry failures stay scoped; independent bases/templates continue before aggregated terminal errors are returned. Missing failed companion sources cannot report ready.
- [x] Upgrade checkpoints: drain, stage configured ready bases, retire unused builders, prepare, validate, commit. Persisted staging/retirement/preparation/rollback states survive interruption. Preserve the provider property key across an empty inventory. Terminal build failure restores prior bases and warms capacity, while still reporting upgrade failure. New upgrades reset retry budgets only after retirement; resumptions retain them.
- [x] Fault coverage: stale receipts, superseded attempt, retry budget after deletion/restart, protected identities, deferred delivery/observer cleanup, source-failure isolation, cancellation, native exclusive moves, failure before/after rename, rollback, and property-key continuity.
- [x] Signed CLI built and installed; live read-only snapshots show all eight slots and images unchanged across installation. Four Linux workers are idle and four macOS templates are ready. The provider holds the owner lock, so no live setup/upgrade or server restart was attempted.
- [x] Final `CGO_ENABLED=0 go test ./...` passed, targeted recovery/checkpoint race tests passed, `go vet ./...` passed, three executable receipt tests passed, shell syntax and `git diff --check` passed. Signed installation passed `codesign --verify --deep --strict`. Evidence: `.local/preparation-reliability/` (`go-test.txt`, `race.txt`, `install.txt`, before/after/final status snapshots).

Live validation does not include a destructive upgrade/failure injection against the production pool. Checkpoint filesystem operations and interrupted lifecycle transitions are exercised with isolated test directories and synthetic guests.

Rollback restores saved images, not configuration. Resume with the same owner and options; if configuration changes make a previous image incompatible, readiness still fails explicitly and the restored image remains available for diagnosis.
