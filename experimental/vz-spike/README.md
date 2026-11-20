# VZ feasibility spike

This standalone command tests `tmc/apple` with `CGO_ENABLED=0` before the local
backend refactor. It is not a production provider. It supports Apple Silicon
macOS with the virtualization entitlement. Source images use the existing Tart disk/config/NVRAM
format; no Tart implementation source is imported or used.

Run from a normal terminal on the worker:

```sh
scripts/test-vz-spike.sh /absolute/path/to/stopped-macos-base /absolute/path/to/stopped-linux-base
```

Use **stopped cold bases**, never an active VM or suspended Tart template. The
script clones only config, disk and NVRAM into a private directory under
`test-results`. It assigns fresh MAC and machine identities, boots Linux, prepares
two macOS snapshots sequentially, then restores two independent clones together
for two rounds. It checks SSH as `admin` / `admin` (the test base credentials).
Existing source files and Pulse records are not modified. The driver stops its
own child processes on exit and retains disposable images and logs for diagnosis.

The probe uses a temporary diskless EFI VM and stops it after one second. It
checks that VM startup works in the execution environment before cloning any
images. Framework configuration validation alone does not prove that VM startup
is allowed. A sandboxed parent can prevent startup even with valid entitlements.

A successful full run prints `VZ_SPIKE_PASS`. This proves the basic boot and
restore path, not provider recovery, image downloads, Docker, or soak qualification.
The runner is deliberately isolated here until that gate passes.

The native adapter uses `tmc/apple/x/vzkit` for the VM dispatch queue and
CoreFoundation run loop. It creates no AppKit window. Save and restore errors
are terminal; a failed restore never falls back to a cold boot. Native
configuration validation and unit tests pass. The full live gate passed outside
Safehouse: Linux boot, headless macOS, two independent snapshots, and two rounds
of concurrent restores (`test-results/vz-spike.5Wt4Xa`). Linux boot and Go SSH
also passed from the agent under Safehouse after a fresh terminal-owned launch.
An earlier launch lost terminal attribution and macOS denied Local Network
access; no additional Safehouse network rule was needed. Earlier macOS runner
traps under Safehouse still need retesting in the working launch context.
See `vz-migration-plan.md` for the evidence and qualification limits.

For short connectivity diagnostics, `vz-spike ssh -timeout 30s VM_DIRECTORY`
prints connection errors as they change and returns the last error on timeout.
The full driver retains the three-minute guest readiness deadline.
