# VZ qualification

Build and unit checks do not qualify live virtualization. The core native,
provider lifecycle, public API/JobDB, registry import, and Go endpoint checks
have passed as recorded below.

## Setup and script map

VZ is a library provider. There is no VZ install daemon or separate provider
setup command. The host calls `NewVZ`, then drives `Run` or `Reconcile`. Guest
setup archives and image preparation run through that same lifecycle. Production
hosts can embed `RunnerMain` in their signed executable; alternatively, build a
standalone development runner and pass its absolute path in `VZConfig.Runner`:

```sh
./scripts/build-vz-runner.sh "$PWD/.local/bin/pulse-vz"
```

This builds with cgo disabled and ad-hoc signs with
`cmd/pulse-vz/vz.entitlements`. It installs no services and changes no sandbox
permissions. Production signing belongs to the host application's release build.
Test drivers build their own helper automatically; this build step is not a
prerequisite for running them.

| Script | Purpose | Creates VMs? |
| --- | --- | --- |
| `scripts/build-vz-runner.sh OUTPUT` | Build/sign a development helper at an absolute path | No |
| `scripts/test-vz-endpoint.sh` | Diagnose launchd bridge startup, forwarding, restart, removal | No |
| `scripts/test-vz-provider.sh LINUX_BASE [MACOS_BASE]` | Native backend lifecycle, suspend/restore, recovery; optional OCI check | Yes, disposable copies |
| `scripts/test-vz-public.sh [MACOS_BASE]` | Public API, capacity, recovery, real JobDB leases | Yes, private VZ home |
| `scripts/test-vz-docker-qualification.sh [MACOS_BASE]` | Full paired Docker allocation fault/pressure matrix | Yes, private VZ home |
| `scripts/test-vz-spike.sh MACOS_BASE LINUX_BASE` | Original native API feasibility experiment | Yes; normally use the provider test instead |

Base arguments must be absolute paths to stopped, cold image directories. See
each section below for prerequisites. All test drivers retain logs under
`test-results/`; the Docker driver also freezes its sources and writes a report
and evidence archive. No driver changes Pulse's provider default or migrates the
Tart inventory.

The ignored `.local/vz-diagnostics/` directory contains this workstation's
investigation scripts and launchers, not required product setup. Its
`resume-provider-agent-v2.sh` and `control-sockets-v2.sb` are the current local
Safehouse launcher/profile. Older launchers and `control-sockets.sb` are obsolete;
do not use them. Network/GPU collectors and probe binaries are historical
diagnostics. Use the supported `scripts/test-vz-endpoint.sh` instead of the old
`.local/vz-diagnostics/test-endpoint.sh`. Local launchers contain workstation and
conversation-specific paths; they are not portable installation instructions.

## Automated checks

```sh
CGO_ENABLED=0 go test ./...
go test -race ./internal/local ./internal/vz/...
CGO_ENABLED=0 go vet ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./...
```

The Linux cross-build includes an explicit unsupported native-runner stub.
Clone and snapshot compatibility tests run on macOS with APFS. Unit tests cover
private clone contents, source/run locking, rollback, fresh cold identities,
preserved snapshot identity, missing state, changed host/layout/configuration,
dispatch fencing, authenticated control, and verified OCI decoding.

## Live provider checks

Use stopped, cold macOS and Linux base directories with SSH `admin` / `admin`.
These credentials are confined to opt-in tests. The tests hold a shared lock on
the source disk and clone only configuration, disk, and NVRAM. They do not modify
the source images or import their Pulse ownership records.

```sh
./scripts/test-vz-provider.sh /absolute/linux-base /absolute/macos-base
```

Omit the macOS argument for the Linux lifecycle test only. The script builds and
ad-hoc signs its own cgo-free helper. Logs and disposable VM directories remain
under `test-results/vz-provider-run.*` for diagnosis.

The Linux test exercises the actual local backend and VZ adapter: source adoption,
cold cloning, native boot, guest-script delivery, client closure and reopening,
recovery without replay, management SSH, authenticated stop, and deletion.
This test passed under Safehouse in `test-results/vz-provider-run.4OxqDC`
(7.34 seconds) after correcting the control-socket grant and launcher.
The macOS test prepares two independent snapshots, restores both concurrently
for two rounds, reopens the client while they run, and verifies SSH and cleanup.
It also corrupts a disposable snapshot and requires restore failure without
cold boot. Live tests stop their own VMs on exit and retain source snapshots.

The smaller feasibility driver remains available:

```sh
./scripts/test-vz-spike.sh /absolute/macos-base /absolute/linux-base
```

It qualifies native API/device behavior but does not exercise provider recovery,
OCI downloads, Docker allocations, or JobDB execution. Those are separate gates.

## Public API and JobDB

```sh
./scripts/test-vz-public.sh /absolute/prepared-macos-base
```

The optional input is a stopped, prepared macOS base. Its raw files are cloned;
the test installs the integration profile, including its JobDB fixture service,
inside a disposable builder. Without an input, normal preparation pulls the
configured macOS base from the registry. The test exercises public admission,
capacity, slot reuse, recovery without the old state directory, and valid,
replayed, and expired JobDB leases. It uses a private VZ home and test owner.
This passed in `test-results/vz-public.2XqEHY` (334.53 seconds), including cleanup.
The valid job executed Nix `hello`, renewed its lease six times, and retrieved
its synthetic secret once. Completed and expired leases did not reacquire work.

That run also exposed and verified a fix in shared delivery tracking: once a
retry reaches guest execution, an earlier SSH setup error is cleared. The
execution fence remains durable, and any new post-dispatch failure still blocks
replay. A regression test and the full race suite cover this transition.

## Go Docker endpoint

The standalone diagnostic builds its own runner:

```sh
./scripts/test-vz-endpoint.sh
```

This VM-free test passed in `test-results/vz-endpoint.WgtkR2` (2.50 seconds):
launchd startup, forwarding, client closure/reopening, same-port process restart,
and service removal. Safehouse denies `job-creation` in the current agent policy,
so the successful run used an ordinary terminal. The Go bridge requires the same
launchd job-registration ability as Tart's existing Python bridge.

## Full Docker qualification

```sh
./scripts/test-vz-docker-qualification.sh /absolute/prepared-macos-base
```

The optional base is copied read-only and prepared with the Docker integration
profile. Without one, preparation uses the registry. The Linux companion comes
from the configured pinned registry image. This is the same production allocator
suite used for the other providers: concurrent JobDB jobs, controller/relay/
endpoint/daemon process loss, CPU/memory and private-disk pressure, prune
isolation, primary/engine VM loss, fresh identities, and cleanup. The controller
subprocess reopens the VZ provider with the same home and runner.

Allow 30–60 minutes plus downloads, at least 40 GiB free, and capacity for two
macOS and two Linux guests (16 vCPUs / 32 GiB). The default worker address is
`192.168.64.1`; override `PULSE_TEST_ENGINE_WORKER_ADDRESS` if the guests reach
this host through a different address. The test uses ports 45200–45201.

The driver checks the launchd endpoint **before creating any VMs**. It requires
launchd job creation and the virtualization/network permissions below. With the
current Safehouse policy, run it from an ordinary terminal. A preflight failure
does not qualify Docker: `report.json` must say `passed`, with evidence for every
required stage. The agent attempt in `test-results/vz-docker.RpflDb` stopped at
this preflight (`bootstrap` exit 5); the full matrix is still pending.

The actual pinned Ubuntu import also passed in `test-results/vz-oci.s0wuxQ`:
manifest/layer verification, Apple LZ4 decoding, cache publication and clone,
followed by the Linux lifecycle test against the imported image (5.75 seconds).
To include a fresh registry import in `scripts/test-vz-provider.sh`, set
`PULSE_VZ_OCI_IMAGE` to a digest-pinned Linux reference. This opt-in gate also
clones from the published cache a second time before booting the imported image.

## Running under Safehouse

Virtualization requires the host's existing virtualization sandbox profile,
read access to `/var/db/dhcpd_leases`, and access to the configured VZ home and
test output directory. A signed binary alone does not grant those permissions.
For macOS guests, also pass `--enable=gpu` to Safehouse. Even the headless macOS
configuration uses a virtual display and needs GPU/IOSurface access.

The provider controls detached VMs through a private, token-authenticated UNIX
socket in `/tmp/pulse-vz-<numeric uid>/`. Safehouse allows TCP/IP by default but
requires a separate UNIX-socket grant. Substitute your numeric `id -u` value for
`UID` in this additional profile:

```scheme
;; VZ runner control only: private, authenticated sockets for this user.
;; macOS SBPL treats {32} literally. Concatenate four eight-digit groups instead.
(define pulse-vz-hex8 "[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]")
(define pulse-vz-control-path
    (string-append "^(/private)?/tmp/pulse-vz-UID/"
        pulse-vz-hex8 pulse-vz-hex8 pulse-vz-hex8 pulse-vz-hex8 "[.]sock$"))
(allow network-bind network-inbound
    (local unix-socket (path-regex pulse-vz-control-path)))
(allow network-outbound
    (remote unix-socket (path-regex pulse-vz-control-path)))
```

Append it using a second `--append-profile=...` flag. This allows only the
per-user VZ control socket names; it does not grant access to Docker or SSH-agent
sockets. Do not change a running agent's sandbox policy from inside that agent.

Keep `codex` as Safehouse's direct command. Safehouse selects its agent profile
from the command name; wrapping it in `bash -c` omits the Codex profile and its
Keychain dependency. If a launcher performs a socket preflight, run that check
in a separate Safehouse invocation before launching Codex directly.

macOS Local Network privacy is separate. In the recorded investigation, both
Terminal and iTerm2 could reach a guest through the exact Safehouse profile.
An older agent launch lost terminal responsibility and reported
`local_network_denied=yes`. A fresh terminal-owned `codex --no-daemon` launch
restored access without adding a TCP rule. The exact earlier ancestry transition
is unknown, and `--no-daemon` alone is not a guaranteed remedy for every launch.

The earlier macOS SIGTRAP under Safehouse is resolved by its GPU profile.
`test-results/macos-sandbox.DTC9gr` compares identical provider tests and policies
with only `--enable=gpu` added: the baseline failed after 7.22 seconds with an
`IOSurfaceRootUserClient` denial; the GPU-enabled test passed in 65.25 seconds.
The separate Metal probe failed with the baseline and passed with GPU access.
The passing provider test includes two independently prepared snapshots, two
rounds of concurrent restores, client recovery, stop/delete, and a corrupt
snapshot that must fail without cold boot. These core lifecycle checks do not
by themselves qualify registry import or JobDB execution; those separate passing
checks are recorded above. The complete macOS Docker allocation fault/pressure
suite has not been rerun with VZ; shared orchestration tests and the new Go
endpoint's launchd lifecycle passed.
