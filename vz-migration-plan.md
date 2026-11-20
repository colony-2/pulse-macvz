# Plan: add a cgo-free VZ provider to pulse-tart

Status: VZ provider implemented; native lifecycle, public API/JobDB, Linux OCI import/boot and Go launchd endpoint qualified
Date: 2026-11-19
Reviewed against: pulse-tart `1b5b9eb`; selected bindings `github.com/tmc/apple` `v0.6.19-0.20260907023617-4c51c1664813`; Cove `8dad6f9abe6a3c5de68b32f09ffb3875a3a6651e`

At the original review, nothing in this plan had been run on a Mac. API names were checked against the tagged library source; behavior claims needing the spike remain unqualified unless explicitly verified below.

Latest qualification: the cgo-free tmc/apple spike passed Linux EFI boot, headless macOS boot, two independent saved templates, and two rounds of concurrent clone restores outside Safehouse (`test-results/vz-spike.5Wt4Xa/result.txt`). After relaunching the agent from iTerm2 using the existing Safehouse profile and Codex `--no-daemon`, agent-originated Linux boot and Go SSH also passed. The shared lifecycle extraction and VZ provider implementation are in place. The actual Linux lifecycle passed under Safehouse, and the macOS suspend/restore lifecycle passed with Safehouse GPU access enabled. Public API/JobDB execution, a pinned Linux OCI import and boot, and the Go launchd endpoint lifecycle also passed. The full macOS Docker allocation fault/pressure suite has not been rerun for VZ. The investigation entries below record earlier failures; the latest results are detailed at the end.

## Decisions already made

- Add VZ as a third backend alongside local Tart and Orchard. Keep `NewTart` and `TartConfig`; removing Tart later is separate work.
- Provider selection and making VZ the default belong to Pulse, not this library. Do not add a default-provider constructor here.
- Do not enforce mutual exclusion between Tart and VZ or add shared host admission across them. Concurrent operation is allowed, but is not the intended deployment. Each backend retains its own capacity accounting.
- Tart pool migration, draining and upgrade policy are outside this work.
- License pulse-tart under Apache-2.0. Preserve third-party notices and licenses; this does not change Tart's own license.
- Build the VZ runner without cgo, using tmc/apple and purego. Selectively reuse Cove code under its MIT license; do not depend on its full CLI or private module replacements.
- Suspended slot templates and resume-from-memory job clones stay.
- The Orchard backend stays unchanged; it uses only Orchard's REST API.
- Use a focused native adapter compiled into our own binary, with tmc/apple/x/vzkit queue and run-loop helpers. Code-Hex/vz is no longer a dependency.

## Outcome

A VZ worker runs macOS and Linux jobs with only the Pulse binary installed. Add `NewVZ` and `VZConfig` alongside `NewTart`/`TartConfig` and `NewOrchard`/`OrchardConfig`. The provider contract, slot model, ownership records, SSH delivery and Orchard path do not change. VZ uses its own home and backend scope; it does not adopt Tart pools or fall back to Tart after a failure.

## What Tart does for us today

Every local VM operation goes through one seam, `Client.command(ctx, args...)` in `internal/tart/client.go`, plus the detached run wrapper in `start()`.

| Tart call | Used for |
| --- | --- |
| `--version` | `Info` health check |
| `list --source local --format json`, `get --format json` | state, hardware, MAC; also counts foreign Tart VMs |
| `clone <oci@digest> <name>` | first pull of a pinned base image |
| `clone <source> <name>` | job clones and slot templates |
| `set --cpu --memory [--random-mac --random-serial]` | sizing; fresh identity for cold clones |
| `run --no-graphics [--suspendable]` | detached VM process under `/bin/sh` with run lock and exit file |
| `suspend`, `stop`, `delete` | template suspend, Docker VM stop, cleanup |
| `ip --wait 5` | guest address for SSH delivery and the Docker endpoint |

VZ avoids two current local-backend dependencies. They remain available for Tart:

- `/usr/bin/python3` for `internal/tart/endpoint/bridge.py`. It is a stub on a stock Mac until the Xcode Command Line Tools are installed.
- `tart` in about 30 scripts under `scripts/`, including `prepare-*.sh`, `update-macos-images.sh`, `test-macos*.sh` and `probe-tart-restore.py`.

The bridge is this repository's small TCP forwarder, introduced in commit `a74d3cf` with Docker allocations. For macOS jobs using a Linux Docker companion, it forwards a worker-host port to the companion's allocation-authenticated SSH service on port 2222. It does not expose an unauthenticated Docker API or terminate SSH. launchd keeps it alive independently of `Runtime.Close`, and reconciliation removes it with the allocation. VZ implements the same function in Go to avoid requiring host Python; replacing Tart's helper is not required for VZ.

## Design

### Layers

```
Pulse binary  (darwin/arm64, cgo-free, signed with the virtualization entitlement)
└─ pulse-tart
   ├─ internal/provider        unchanged
   ├─ internal/orchard         unchanged
   └─ internal/local           shared local lifecycle; pure Go
      ├─ tart                  retained Tart CLI adapter
      └─ vz
         ├─ store              VM directories, clone, identity, state, IP lookup
         ├─ image              OCI pull of base images
         └─ runner             darwin/arm64 only; tmc/apple and purego
```

`internal/local` keeps everything that is not hypervisor-specific: `pulse.json` records, the pool lock, volume binding, delivery and recovery, endpoint allocation. Backend identity, home and command/runner configuration remain distinct; existing Tart ownership scopes and paths must not change during extraction.

Build tags contain the native adapter to `darwin && arm64`; other platforms receive a clear unsupported-runner stub. All providers build with `CGO_ENABLED=0`. Tart and Orchard retain their existing platform support. Signing the executable that creates VMs remains mandatory. Qualify cross-compilation separately from live macOS execution.

### Process model

A VM lives only as long as the process that created it, and accepted execution must survive `Close`. So each VM still gets its own detached process, exactly as today. Only the command changes:

- Today: `/bin/sh` wrapper runs `tart run --no-graphics --suspendable <name>`.
- New: the same wrapper runs `<pulse binary> __pulse-vm run <vm-dir>`.

Pulse calls one hook at the top of `main`:

```go
func main() {
    pulsetart.RunnerMain() // runs the VM and exits when invoked as a runner; otherwise returns
    ...
}
```

The wrapper, run lock, `run.log` and exit file are kept as they are, because they still report an exit code when the runner is killed.

`VZConfig.Runner` can point at a separately built runner binary. That is for tests and for any host that prefers not to sign the main binary.

### Public API

```go
type VZConfig struct {
    Home           string        // default ~/.pulse-vz
    Runner         string        // default os.Executable()
    CommandTimeout time.Duration // default 2m
    SSHTimeout     time.Duration // default 15s
    DockerEndpoint DockerEndpoint
}

func NewVZ(c Config, options VZConfig) (*Runtime, error)
func RunnerMain()
```

`VZConfig` does not need `Binary` or `PythonBinary`; both remain in `TartConfig`. `Config`, `Runtime` and its methods are unchanged. Use backend kind `vz` for VZ ownership scope and diagnostics.

### VM directory

`<Home>/vms/<name>/` holds:

| File | Content |
| --- | --- |
| `config.json` | OS, arch, CPU, memory, MAC, macOS hardware/machine identity, layout version, required-restore flag and snapshot compatibility |
| `disk.img` | raw disk |
| `nvram.bin` | macOS auxiliary storage, or the EFI variable store for Linux |
| `state.vzvmsave` | saved machine state, present only while suspended |
| `pulse.json`, `pulse.run.lock`, `pulse.run.log`, `pulse.exit` | existing Pulse artifacts |
| `vz.lock`, `vz.started`, `vz-control.json` | native-run lock, durable start fence, private control socket/token metadata |

Power state needs no subprocess: running means the run lock is held, suspended means `state.vzvmsave` exists, otherwise stopped.

### Operation mapping

| Today | New implementation | Layer |
| --- | --- | --- |
| `--version` | `__pulse-vm probe`: entitlement present, macOS 14+, virtualization supported | runner |
| `list`, `get` | read `config.json` plus lock and state file | store |
| `clone <oci>` | pull into `<Home>/cache/<digest>/`, then clone | image |
| `clone <src> <dst>` | `unix.Clonefile` per file into a temp directory, then rename | store |
| `set --cpu --memory` | edit `config.json` | store |
| `--random-mac` | generate a locally administered MAC in Go | store |
| `--random-serial` | signed runner generates a new VZ machine identifier; store persists it in `config.json` for a cold clone | store + runner |
| `run` | restore when metadata requires it; reject missing/incompatible state without cold fallback; otherwise cold boot | runner |
| `suspend` | authenticated socket request: persist required-restore metadata, pause, save to a temp name, fsync, rename, exit 0 | runner |
| `stop` | authenticated socket request: native `Stop` with a bounded completion wait | runner |
| `delete` | refuse while the lock is held; remove the directory | store |
| `ip --wait` | match the MAC in `/var/db/dhcpd_leases`, polling | store |
| `bridge.py` | VZ uses `__pulse-vm bridge` under launchd; Tart keeps its helper | runner binary, pure Go code |

### Runner configuration

The selected tmc/apple version exposes all of these native APIs. The concrete calls are exercised by `experimental/vz-spike/native_darwin_arm64.go`; configuration validation alone does not qualify execution.

**macOS guest**

- `VZMacOSBootLoader`, `VZMacPlatformConfiguration`, `VZMacAuxiliaryStorage`, and hardware model/machine identifier initialized from persisted data.
- `VZMacGraphicsDeviceConfiguration` with one display. No window is opened.
- `VZDiskImageStorageDeviceAttachment` with uncached I/O and fsync behind a virtio block device.
- `VZNATNetworkDeviceAttachment` with the MAC from `config.json`.
- `VZMacKeyboardConfiguration` and `VZMacTrackpadConfiguration`.
- For `suspendable` VMs: call `ValidateSaveRestoreSupportWithError()` before start and fail fast. The exact device set that passes is a spike output; expect to drop audio and entropy.

**Linux guest**

- `VZEFIBootLoader` with a variable store, generic platform, virtio network, block and entropy.
- Cold boot only, as today.

### Saved-state rules

These encode what `docs/suspended-templates.md` already established.

- Cloning a suspended source keeps its MAC and machine identifier and clones `state.vzvmsave`. Cloning a stopped source gets a fresh MAC and identifier.
- Each slot template has a unique identity; at most one clone of a template runs at a time. The provider already enforces this.
- The runner deletes `state.vzvmsave` after a successful restore. A failed restore leaves it and exits non-zero; job semantics stay "terminal, no cold fallback".
- The runner stamps a layout version next to the saved state. A runner with a different device layout refuses the restore with a specific error, and the provider rebuilds the template. A host macOS upgrade is handled the same way.

### Base images

**Phase A: keep the pinned images, pull them natively.** `bases.json` and `DefaultLinuxBase` reference `ghcr.io/cirruslabs/...@sha256:...` in the Tart OCI layout: one config blob, one NVRAM blob and LZ4 disk chunks, each chunk annotated with its uncompressed size and digest.

- Registry access: `github.com/google/go-containerregistry` (Apache 2.0).
- Decompression: `github.com/pierrec/lz4/v4` (BSD-3) plus a small deframer for Apple's LZ4 framing. cove's `internal/ociimage` (MIT) implements the same format and is a usable reference.
- Verify each chunk digest, write sparse, and reject unknown layer types with a clear error.
- Keep this code pure Go so it is tested on Linux CI against a fake registry.

**Phase B: own the images.** Publishing prepared bases to our own registry removes the dependency on an external image publisher. Use the same layout so there is one VZ puller. Building from Apple restore images is possible with `vz.NewMacOSInstaller`; unattended first-boot setup is the hard part and is an open decision. Phase B is not on the critical path.

## Implementation sequence

The refactor comes before the new code so that every step is verifiable against today's behavior.

1. **Introduce a hypervisor interface inside the local backend.** Move the existing Tart CLI calls behind it with no behavior change. Existing unit tests keep passing.

   ```go
   type hypervisor interface {
       Probe(ctx context.Context) error
       List(ctx context.Context) ([]hardware, error)
       Inspect(ctx context.Context, name string) (hardware, error)
       Pull(ctx context.Context, ref, name string) error
       Clone(ctx context.Context, src, dst string, keepIdentity bool) error
       Configure(ctx context.Context, name string, cpu, memoryMiB int64) error
       Start(name string, suspendable bool) (pid int, err error)
       Suspend(ctx context.Context, name string) error
       Stop(ctx context.Context, name string) error
       Delete(ctx context.Context, name string) error
       IP(ctx context.Context, name string, wait time.Duration) (string, error)
   }
   ```

2. **Add the vz implementation** beside the Tart one.
3. **Run the live suite against both** in separate runs on the same worker and compare. Keep Orchard regression coverage.
4. **Expose `NewVZ` and qualify it for release.** Add VZ paths to tooling and retain explicit Tart/Orchard paths. Pulse changes its own default after VZ qualifies; that selection change and any later Tart removal are outside this library plan.

## Milestones

Estimates are rough and assume one engineer with a dedicated Apple Silicon worker.

| # | Milestone | Exit criterion | Estimate |
| --- | --- | --- | --- |
| 0 | Spike | A standalone signed Go program boots a macOS base headless, suspends it, clones it with state and identity, and restores two clones from two templates at the same time. SSH answers on the expected address. Restore time is recorded against today's 8.2 s. A Linux EFI guest boots. | 3 to 5 days |
| 1 | Seam refactor | Hypervisor interface in place; Tart behind it; all tests green; no behavior change | 3 to 4 days |
| 2 | Runner and store | Unit tests on the store; runner contract tests pass on the macOS runner; the scenarios from `probe-tart-restore.py` pass | 1.5 to 2 weeks |
| 3 | Image pull | Both pinned macOS bases and the Linux base pull by digest and boot | about 1 week |
| 4 | Integration | `NewVZ` passes `scripts/test-macos.sh`, including restart recovery, cleanup and JobDB execution; Docker suites pass with the Go bridge | about 1 week |
| 5 | Tooling and CI | VZ tooling runs without Tart or Python; explicit Tart/Orchard paths retained; darwin VZ CI builds with cgo disabled and signs; cross-platform builds verified | about 1 week |
| 6 | Release qualification | VZ soak passes; three backends remain available; Apache-2.0 license in place; README, `SPEC.md`, `PURPOSE.md`, `NOTICE`, `compatibility.json` updated | 3 to 5 days |
| 7 | Own base images | Bases served from our registry; no `ghcr.io/cirruslabs` reference left | separate track |

Milestone 0 is the go/no-go. It needs no pulse-tart changes: an existing Tart-pulled base directory on a dev Mac is enough input.

## Build, signing and CI

- Build with `CGO_ENABLED=0`. The selected bindings use purego to call Apple frameworks dynamically; no Objective-C compilation step is required. Build and signing are separate checks.
- Require macOS 14 or later for save/restore, and validate host API availability before constructing the VM. Pin the bindings and test native callback lifetimes and dispatch/run-loop behavior on supported hosts.
- The binary must carry `com.apple.security.virtualization`. Ad hoc signing is sufficient: `codesign --entitlements vz.entitlements -s - <binary>`. Add the entitlements file to the repo and the signing step to the release job.
- No special Apple approval is needed for the planned Virtualization.framework/NAT setup. Add the virtualization entitlement to Pulse's existing release signing process, preserving its other entitlements and normal distribution signing/notarization. Apple's [entitlement instructions](https://developer.apple.com/documentation/virtualization/adding-the-virtualization-entitlement-to-your-project) describe adding the boolean key. Its [NAT documentation](https://developer.apple.com/documentation/virtualization/vznatnetworkdeviceattachment) explicitly says the restricted `com.apple.vm.networking` entitlement is not required. Ad hoc signing above is for local development and test runners.
- Ordinary `go test` builds do not carry the required virtualization entitlement. Live tests therefore use a small test-only runner (`internal/cmd/pulse-vm`), built and signed by `make`, and passed through `VZConfig.Runner`. The same tool serves the VZ paths in scripts. It is not shipped with Pulse under the recommended embedded-runner design.
- CI: add a darwin cgo-disabled build-and-sign job on the self-hosted macOS runners; keep the Ubuntu job and add `GOOS=linux go build ./...` as a guard on the build tags.

## Testing

- **Unit:** a fake hypervisor, following the existing fake-command tests.
- **Runner contract:** boot, stop, suspend, restore, kill during save, restore with a mismatched layout version, missing entitlement.
- **Restore matrix:** the cases in `probe-tart-restore.py`, including identity collisions.
- **Live suite:** `scripts/test-macos.sh` and the Docker suites gain an explicit VZ selection, unchanged in what they assert. Retain Tart and Orchard coverage for the shared lifecycle refactor.
- **Soak before release qualification:** a few hundred template-clone-restore-delete cycles, a host reboot mid-run, and a full disk during save.

## Behavior changes

- VZ requires macOS 14 for suspended templates; existing backend requirements are unchanged.
- A binary hosting the VZ runner must be signed with the virtualization entitlement.
- VZ uses a new home directory and prepares its own pools. Tart pool migration and rollout policy are outside scope.
- VZ observes its own inventory. Tart/Orchard and other tools' VMs are not included in VZ admission accounting; combined use can fail at VM startup. Do not add cross-backend capacity coordination or prevent concurrent operation. Deployments are expected to avoid concurrent use.

## Risks

| Risk | Mitigation |
| --- | --- |
| Headless macOS guest may need an application run loop. The library's macOS example opens a window; its Linux example does not. | First spike item. Fallback: run the application loop with no window. |
| Suspended-clone restore is identity- and configuration-sensitive | Spike proves it before any refactor; layout version stamp; restore matrix in CI |
| Native framework or binding failure takes down the process | One process per VM, so a crash loses one VM; the wrapper still records the exit |
| Tart OCI layout changes or the images disappear | Images are pinned by digest; Phase B removes the dependency |
| Leaning on Tart's implementation while adding VZ | Work from Apple's documentation, the tmc/apple bindings, and MIT-licensed references (Lume, Cove). Do not copy from Tart's source. |
| Binding or Go version drift | Pin tmc/apple and purego; the module uses Go 1.26. Re-run native qualification when either changes. |

## Open decisions

1. Runner inside the Pulse binary (recommended) or a separately shipped signed helper? Either supports a cgo-free controller; signing and runner version compatibility must be checked.
2. Keep pulling the cirruslabs images initially (recommended), or make Phase B a prerequisite for VZ release qualification?
3. How to do first-boot setup for our own macOS images: one manual pass per macOS release, or an unattended tool at build time?
4. Rename the module and repo separately? The current name describes only one of three backends; renaming is not needed to add VZ.
5. VZ on Intel Macs: support Linux guests there, or qualify VZ on Apple Silicon only initially? Existing Tart support is unchanged.

## Cove evaluation — 2026-11-19

Reviewed `github.com/tmc/cove` at [`8dad6f9abe6a3c5de68b32f09ffb3875a3a6651e`](https://github.com/tmc/cove/tree/8dad6f9abe6a3c5de68b32f09ffb3875a3a6651e), not the older `tmc/vz-macos` repository. The user selected the focused-runner option after this review. It is not yet an implemented provider or a passed live qualification.

### Findings

- **Cgo-free is real.** Cove uses `github.com/tmc/apple` and `github.com/ebitengine/purego`, and its release configuration specifies `CGO_ENABLED=0`. A separate probe built with cgo disabled against Cove's pinned `tmc/apple` version, `v0.6.19-0.20260907023617-4c51c1664813`, successfully created a Virtualization.framework configuration and round-tripped CPU/memory values. This verifies native calls, not VM boot or restore. Signing remains required for the VM process.
- **Suspend/resume already exists.** `cmd/cove/macos.go` implements pause, save, restore and resume. It saves on normal SIGINT/SIGTERM cleanup. No need to implement the basic framework calls from scratch.
- **Suspended-template cloning does not meet our contract.** [`clone.go`](https://github.com/tmc/cove/blob/8dad6f9abe6a3c5de68b32f09ffb3875a3a6651e/cmd/cove/clone.go) discards saved state. [`fork.go`](https://github.com/tmc/cove/blob/8dad6f9abe6a3c5de68b32f09ffb3875a3a6651e/cmd/cove/fork.go) explicitly documents that snapshot forks rotate machine identity, expect restore rejection and rely on cold boot. We need a mode that clones a consistent suspended disk/state/config/identity set, preserves machine ID and MAC, and leaves template sources untouched.
- **Restore failures currently cold-boot.** [`startConfiguredVM`](https://github.com/tmc/cove/blob/8dad6f9abe6a3c5de68b32f09ffb3875a3a6651e/cmd/cove/macos.go) moves state aside and falls through to cold boot on configuration mismatch or restore failure. Pulse needs explicit resume-required semantics that fail nonzero, preserve diagnostics and never boot fresh after failed restore. A missing state file must also fail when resume is required.
- **Suspend needs a machine-facing contract.** Graceful exit currently tries to save and falls back to hard stop if saving fails. Pulse needs distinct suspend and stop operations with observable completion and failure. State/config publication must be crash-consistent, and compatibility must cover the runner device layout and host OS version.
- **OCI reuse is valuable but not complete.** Cove has Tart manifest parsing, Apple LZ4 decoding, sparse disk writes and digest checks. Its [`tart_pull.go`](https://github.com/tmc/cove/blob/8dad6f9abe6a3c5de68b32f09ffb3875a3a6651e/cmd/cove/tart_pull.go) projects only CPU and memory; OS, architecture, hardware model and machine identity are preserved in raw JSON but not converted there. Linux also needs its own disk/NVRAM layout. Our exact pinned macOS and Linux images still need import-and-boot qualification.
- **Headless is not yet proven for our worker.** The macOS headless path creates an ordered-out VM window, an AppKit application and a status item. Test it in the actual service/session environment before adopting it; a CLI flag alone does not establish service compatibility.
- **Current packaging blocks a clean full build.** `CGO_ENABLED=0 GOWORK=off go build ./cmd/cove` failed because `go.mod` replaces `cove-marina` and `cove-ocispec` with absent sibling directories. Neither sibling could be fetched anonymously from GitHub in this evaluation. Cove's installation docs also describe an unreleased Apple dependency overlay; that documented issue has not been independently reached because the sibling modules block the build first.
- **Library extraction is substantial.** VM execution and lifecycle remain in `cmd/cove` (`package main`); `internal/vmrun` is configuration/planning, not an importable engine. Using the whole project means an external CLI or a maintained extraction/fork. Cove's MIT license permits reuse with its notices retained.

### Options and recommendation

1. **Extend Cove as a signed external helper.** Add identity-preserving suspended clones, resume-required failure semantics, reliable suspend/stop results, complete image translation, and a reproducible pinned build. Pulse invokes it through the local hypervisor seam. This reuses more existing CLI/store/image code, but adds a separate binary and a broader dependency/fork to maintain. Do not use runtime ad hoc re-signing for production distribution; pre-sign the helper with the release entitlements.
2. **Use Cove's cgo-free foundation in our focused runner.** Replace Code-Hex/vz with `tmc/apple`/`purego` (and selected `x/vzkit` helpers) while keeping the planned detached runner, Pulse lifecycle and narrow device configuration. This preserves the single-Pulse-binary design and avoids cgo without depending on Cove's CLI, fleet components or private module replacements. OCI format code can be reused under its MIT terms if extracted with a complete dependency/license audit.

Recommendation: qualify option 2 first if the goal is avoiding cgo while retaining the proposed deployment model. Option 1 is viable if reusing and maintaining Cove as a separate product dependency is preferred, but it is not a drop-in replacement today. Either option must pass the same two-template concurrent restore spike before the provider refactor.

### Verification recorded

- Original pulse-tart `go test ./...`: passed before the prototype changes.
- Code-Hex/vz signed configuration validation: passed; tool-sandbox VM start denied; user Terminal EFI start confirmed.
- Cove's pinned Apple bindings: cgo-disabled build and native configuration round trip passed; no live VM test of those bindings yet.
- Cove `internal/vmrun`, `internal/vmidentity`, `internal/vmstate` tests: passed with cgo disabled.
- Cove `internal/checkpoint` unit tests (`-run '^Test'`): passed with cgo disabled. Its example tests fail here because they hard-code creation of `~/tmp`, which is outside the tool's allowed filesystem access. Setting `TMPDIR` does not change that hard-coded path.
- Full Cove executable build: blocked by the missing sibling module replacements above. No claim of a working Cove VM or production-ready integration.


### Implementation checkpoint

- Ported the isolated spike to tmc/apple with cgo disabled: Linux EFI and lean macOS configuration, distinct cold identities, state-preserving suspended clones, save/restore, native queue and CoreFoundation event loop. No AppKit presentation is created.
- Added a Go TCP bridge with half-close and shutdown tests. It forwards the companion SSH stream; it is not yet connected to launchd/provider allocations.
- Added a digest-pinned OCI importer with sparse output, compressed and decoded digest validation, declared-size limits and cleanup of failed imports. Copied only Cove's Apple LZ4 codec/tests, retained its MIT license in `internal/vz/image/LICENSE.cove`, and fixed dictionary history across short blocks. Real registry image import-and-boot remains unqualified.
- Changed the project license to Apache-2.0 and retained third-party notices.
- `scripts/test-vz-spike.sh` builds/signs the cgo-free spike and records Linux boot, two sequential independent macOS templates, and two rounds of concurrent restores. It must run outside the tool sandbox, which rejects VM startup. The script retains logs under `test-results/vz-spike.*`.
- The live feasibility gate, shared lifecycle refactor, production runner/store, `NewVZ`, tooling integration and release qualification remain outstanding. Existing Tart and Orchard execution paths have not changed.

Validation at this checkpoint: `CGO_ENABLED=0 go test ./...`, `go test -race ./internal/vz/...`, `CGO_ENABLED=0 go vet ./...`, `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./...`, and spike shell syntax checks all passed. These are build/unit checks, not live VM qualification.


### Safehouse retry

After the user added the Virtualization service, task-name and virtualization
extension rules and restarted the session, the cgo-free firmware probe started
and stopped successfully. A disposable Linux guest started, received a DHCP
lease and answered the system SSH client. The Go SSH probe failed before login
with `connect: no route to host`; Local Network privacy is suspected, not proven.
A separate disposable macOS start failed with `Failed to issue sandbox extension
for Metal shader directory`. Logs are in `test-results/vz-spike.nCukxd`.
Both test runners exited and no concurrent suspend/restore gate has passed.
The earlier statement that the sandbox prevents all VM startup is superseded by
these narrower results. Additional Safehouse/OS permissions remain to diagnose.


A further retry after the Metal-cache profile change (`test-results/vz-spike.DB1kCI`)
passed the firmware probe and started Linux with a DHCP lease. Go connections
still returned `no route to host`. macOS progressed beyond the Metal error but
failed issuing a sandbox extension for the disposable VM's `nvram.bin`; the VM
artifact directory also needs scoped file-extension issuance. No macOS boot or
suspend/restore pass is claimed. `TMUX` was unset but `TERM_PROGRAM=tmux` remained;
those environment values alone do not establish the process ancestry.


The absolute VM-artifact extension rule subsequently allowed both Linux and
macOS startup (`linux-absolute-permission.log`, `mac-smoke-absolute-permission.log`
under `test-results/vz-spike.XxjAhX`). The Go SSH client still fails with
`no route to host`, with no matching user-observed Seatbelt or Local Network log
entries. The runner has an LC_UUID and the virtualization entitlement. The user
launches from iTerm2 through Safehouse with Codex `--no-daemon`; no missing
Safehouse network flag has been established.

A native macOS save smoke test then exited with signal 5 before publishing a
snapshot (`mac-save-smoke.log`). The crash report is needed; reading
`~/Library/Logs/DiagnosticReports` was denied by the sandbox. This is a separate
unresolved issue, and save/restore remains unqualified. The SSH probe now accepts
`-timeout` and prints connection error changes promptly; its unit tests pass
with cgo disabled.


### Controlled comparisons

The user ran the identical `.local/vz-diagnostics/network.sh` from iTerm2:
through Safehouse it failed with `no route to host`; directly from iTerm2 it
reached Linux guest SSH after 6.36 seconds, verifying OS, architecture and the
expected MAC. This establishes Linux boot and Go SSH functionality outside
Safehouse and rules out an agent-only launch issue. No precise sandbox or
privacy subsystem cause has yet been established; no broad network rule or
system privacy change was applied.

Crash reports became readable after the user granted read-only diagnostic-report
access. Additional stage logs showed macOS exits with SIGTRAP about six seconds
after successful Start, before any pause/save request. The earlier description
of a save-operation crash was premature. The same early exit occurred with:
(1) tmc/apple with cgo disabled, (2) the same tmc/apple adapter with cgo enabled,
(3) a temporary AppKit/no-window control, and (4) the earlier Code-Hex/vz runner
using the same disposable VM and device layout. Controls are confined to
`.local/vz-spike`; Code-Hex is not a project dependency. A full macOS live spike
outside Safehouse is the next gate. Native Start success alone is not guest
boot or save/restore qualification.

### Completed feasibility gate and agent networking

The full cgo-free run in `test-results/vz-spike.5Wt4Xa` completed with
`VZ_SPIKE_PASS`: Linux EFI, headless macOS, two independently prepared snapshots,
and two rounds restoring both clones concurrently. Native restore/resume took
5.48–7.76 seconds. Every guest reached SSH, and the driver stopped its VMs.
This qualifies the feasibility gate, not the production provider lifecycle.

Network.framework subsequently identified the agent's network failure as
`local_network_denied=yes`. The failing probe was its own responsible process.
Controlled collectors from Terminal (`local-network.dMQKrp`) and iTerm2
(`local-network.XTzjyn`) both passed all five launch variants: plain, Safehouse's
environment marker alone, unrestricted Seatbelt, default Safehouse, and the exact
custom Safehouse profile. In each successful run, the terminal remained the
responsible process. These results supersede the earlier inference that the
Safehouse configuration itself explains the network failure.

Resuming this conversation from the ordinary iTerm2 shell through
`.local/vz-diagnostics/resume-agent.sh` restored terminal responsibility while
retaining the existing Safehouse grants. In the relaunched agent, the native
probe reported `local_network_denied=no` and the cgo-free Go SSH probe reached
the disposable Linux guest in 5.34 seconds, verifying OS, architecture and MAC.
The diagnostic stopped its VM. No new network rule or system privacy setting
was needed. The precise earlier process transition that lost terminal
responsibility remains unknown; the user had already reported `--no-daemon`,
so a missing flag is not established as the cause. The earlier macOS SIGTRAP
under Safehouse still needs retesting in this working launch context.

### Provider implementation checkpoint

Implemented the typed `internal/local.Hypervisor` seam, moved local lifecycle
and its tests to `internal/local`, and retained a thin `internal/tart` constructor.
`NewVZ` / `VZConfig` use a separate home and ownership scope. `RunnerMain` and
`cmd/pulse-vz` provide a signed cgo-free helper; VZ's launchd endpoint uses Go.
The store implements verified OCI cache publication, APFS cloning, fresh cold
identities, preserved snapshot identities, DHCP lookup, native/run locks, and
an exclusive durable start fence. Snapshot metadata binds saved state to the
host build, device layout and configuration. Missing/incompatible state fails
without cold fallback. Detached runner control uses a private authenticated UNIX
socket instead of saved-PID signalling.

Implementation refinements to the proposed layout: macOS hardware and machine
identity remain embedded in `config.json`, and both OSes use `nvram.bin`, matching
the qualified native spike. The config carries `vzVersion`, `restoreRequired`
and snapshot compatibility metadata. Existing shared artifacts remain named
`pulse.run.lock`, `pulse.run.log`, `pulse.exit` and `pulse.json`. Additional
runner files are `vz.lock`, `vz.started` and private `vz-control.json`.

Checks passed: full `CGO_ENABLED=0 go test ./...`; subsequent targeted tests
for the new public constructor, shared lifecycle, provider and VZ packages;
`go test -race ./internal/local ./internal/vz/...`; `CGO_ENABLED=0 go vet ./...`;
cgo-free Linux amd64 and Darwin amd64 cross-builds. The signed standalone
helper passed its native support/entitlement/protocol probe and identity creation.

Core provider lifecycle tests now pass. Linux boot, delivery, client reopening,
recovery, SSH, stop and deletion passed in 7.34 seconds under Safehouse
(`test-results/vz-provider-run.4OxqDC`). The controlled macOS comparison
(`test-results/macos-sandbox.DTC9gr`) showed that adding only Safehouse's
`--enable=gpu` changes the result from an early SIGTRAP with an IOSurface denial
to a full 65.25-second pass. That test includes two independent snapshots, two
rounds of concurrent restores, client reopening/recovery, SSH, cleanup and
corrupt-state rejection without cold fallback. No extra GUI profile was needed.

Additional integration checks passed:
- `test-results/vz-oci.s0wuxQ`: real pinned Ubuntu manifest/layer verification,
  Apple LZ4 decoding, cache publication/clone, then Linux boot/recovery/cleanup.
- `test-results/vz-endpoint.WgtkR2`: Go endpoint launchd startup, forwarding,
  Close/reopen, same-port restart, and removal (2.50 seconds).
- `test-results/vz-public.2XqEHY`: public admission/capacity, slot reuse, recovery
  without the old StateDir, and real JobDB valid/replayed/expired leases
  (334.53 seconds). The valid job ran Nix hello, renewed six times and fetched
  its synthetic secret once. Final test VM locks and endpoint services were
  confirmed inactive/absent.

The public test found a shared lifecycle bug: a prior SSH setup timeout was
still reported after a retry entered guest execution. The dispatch transition
now clears that stale cause while preserving the durable execution fence and
post-dispatch failure behavior. Regression, full unit/race, vet and cgo-free
cross-build checks passed. CI now explicitly checks cgo-disabled tests/builds
and signed standalone-helper construction.

The complete macOS Docker allocation fault/pressure suite was not rerun with
VZ; shared orchestration tests and the new endpoint lifecycle passed. No default
selection changed. The terminal launcher includes the tested GPU feature and
checks socket access and Metal before starting Codex directly. Safehouse's
current policy denies launchd job creation, so endpoint qualification used a
normal terminal without changing the agent's policy.

### Control socket sandbox correction

The first scoped grant still denied bind after restart
(`test-results/vz-provider-run.0z3XNW`), before native startup. A read-only
comparison using the installed macOS sandbox compiler established that `{32}`
is literal text in its regex dialect: `a{32}` and `a[{]32[}]` compile identically,
while 32 repeated `a` characters do not. The corrected profile concatenates
four groups of eight hex-character classes, retaining the original exact scope.
A replacement terminal launcher checks bind/listen/connect/data exchange under
Safehouse before resuming Codex. It requires a fresh launch; the active profile
was not modified. The Linux recovery assertion now decodes the stored
`execution_possible` JSON key explicitly.
