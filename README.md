# pulse-macvz

A Go library and Cobra CLI for running Pulse jobs in macOS and Linux ARM64 VMs
through Apple's Virtualization.framework. Requires Apple Silicon macOS; builds
with `CGO_ENABLED=0`. Tart and Orchard providers are no longer included.

## Install and prepare

```sh
make install
pulse-macvz setup --macos-base all
pulse-macvz server --collector pulse.example.com:8443 \
  --tls-ca ca.pem --tls-cert worker.pem --tls-key worker.key
```

`make install` builds, signs, and installs `pulse-macvz` in Go's normal binary
directory. On macOS it points to the executable inside `PulseProvider.app`,
preserving the established signing identity. `upgrade` drains and refreshes
prepared images. Stop the pool's existing controller before maintenance.
See [CLI configuration and embedding](docs/server-cli.md).

VMs live in `~/.pulse/vz`. Configuration defaults to
`~/.pulse/vz/provider.json`. If absent, the CLI reads `~/.pulse/provider.json`
and warns on stderr. Successful setup/upgrade writes the new path and leaves
the old file untouched. `--config` explicitly selects a file without fallback.

## Use the library

```go
import macvz "github.com/colony-2/pulse-macvz"

// Call macvz.RunnerMain() at the start of main, before other initialization.
cfg := macvz.Config{OwnerID: "mac-pool"}
runtime, err := macvz.NewVZ(cfg, macvz.VZConfig{})
if err != nil { return err }
defer runtime.Close()
// Pass runtime to Pulse as api.Provider and drive reconciliation:
return runtime.Run(ctx)
```

The host executable needs the virtualization entitlement and stable signing.
Alternatively, supply a separate signed executable in `VZConfig.Runner`.
The library implements `pulse-provider-api`; Pulse owns scheduling and leases.
Constructors validate configuration and lock the owner scope without creating VMs.
`Run` or `Reconcile` prepares capacity. `GetCapabilities` advertises the configured
catalog independently of readiness; `Slots` and `Images` report progress.

Public Cobra constructors share the standalone flags and functionality:

```go
import providercli "github.com/colony-2/pulse-macvz/cli"

vzCmd.AddCommand(providercli.NewSetupCommand(), providercli.NewUpgradeCommand())
```

## Capacity and images

Omitted configuration selects **two macOS job VMs and four Linux job VMs**, each
with **4 vCPU / 8 GiB**. The bases are Tahoe (`macos-26-arm64`), Sequoia
(`macos-15-arm64`) and Ubuntu 24.04 (`ubuntu-24.04-arm64`). Both macOS versions
share the same two-job limit. Larger or smaller VM sizes are currently rejected.

macOS workers use an **80 GiB sparse, copy-on-write disk** (larger source disks
are preserved). Base preparation expands the guest APFS container and requires
**at least 30 GiB free** on both the data volume and Nix store before publishing
the image. This is starting free space, not a reservation against subsequent job
writes. Clones share unchanged host blocks; 80 GiB is not allocated per worker.
After installing this version, `setup` prepares new bases and suspended templates
for this storage policy. Existing accepted jobs finish on their original disks;
their space does not increase in place. `upgrade` also refreshes installed tools.

Override any of the three job counts; omitted per-OS counts stay at 2 and 4,
and an omitted total becomes their sum:

```go
cfg.VMCounts = macvz.VMCounts{
    MacOS: macvz.VMCount(2), // maximum 2
    Linux: macvz.VMCount(4),
    Total: macvz.VMCount(3), // shared limit: at most three job VMs across both OSes
}
// macvz.VMCount(0) disables an OS; Total: macvz.VMCount(0) drains all job capacity.
// nil means default. ResolveConfig(cfg) returns the effective configuration.
```

These counts include warming/idle job VMs and unresolved launch reservations,
but **exclude Docker companions and image builders**. A macOS Docker job counts
as one macOS job and one total job, regardless of its Linux companion. A separate
physical limit counts every running/pending macOS VM, including builders and
foreign VMs on the worker, and never admits a third macOS instance. Suspended and
stopped source images do not count. Preparation waits for a free macOS position.

Lower limits let active jobs finish and retire unused warm capacity. With a
shared total smaller than the OS limits, a request can reclaim an idle worker
of another OS; Pulse retries while the requested pool warms. There is no lease
queue. Busy or warming slots return `no_capacity`.

Counts do not automatically scale to host resources. Six default job VMs use
24 vCPUs / 48 GiB before host overhead; two macOS Docker companions add another
8 vCPUs / 16 GiB. Choose lower counts for smaller workers.

Explicit `Bases` and `Slots` remain available for base selection. Without slots,
every configured base gets a group. The first macOS and Linux groups retain the
names `standard` and `linux`; later groups use `mac-<base>` or `linux-<base>`
(long names use a stable hash). The default Sequoia group is `mac-macos-15-arm64`.
An explicit slot group's zero `Count` drains that group; its zero CPU/memory
values resolve to 4 / 8192. Job limits apply across all groups. `C2JVersion` still
defaults to `"latest"`.

Default pools have eight configured positions: two per macOS version and four
for Linux. These are alternative per-base capacity, with **at most six job VMs
and two macOS instances** overall. Either macOS version can use both macOS
positions, or one job can run on each version.

`DefaultBases()` supplies these digest-pinned macOS images, without Xcode.
`DefaultLinuxBase()` supplies Ubuntu 24.04 ARM64. The default SSH login is
`admin`/`admin`; override credentials with a private image.

| Concrete base | Verified guest OS | Preference |
| --- | --- | --- |
| `macos-26-arm64` | Tahoe 26.6.2 | First |
| `macos-15-arm64` | Sequoia 15.7.7 | Second |

`GetCapabilities` advertises all configured bases in order, independent of current capacity. Selectors such as `macos-26*` or `macos-*arm64` match concrete names; `*` matches any sequence, other characters are literal, and matching is case-sensitive. All requested capabilities must match. Admission chooses the first configured base with an available slot. With omitted slots, adding a base automatically creates its group; the two-macOS limit applies across all versions.


Only ARM64 guests are supported. The provider pulls compatible OCI VM images
itself; no Tart executable is required. Existing VZ homes, owner scopes, prepared
images, and guest paths/services retain their identities across the rename.
Guest artifacts named `pulse-tart` are a compatibility format, not a Tart dependency.

Image names contain no c2j version. Existing bases are reused. Changing
`C2JVersion` only affects newly prepared bases; run `pulse-macvz upgrade` to
refresh existing ones. Preparation installs c2j and tools using Nix in the guest.

## Execution and recovery

macOS slots use suspended prepared templates; Linux uses cold clones.
`Close` leaves VMs intact. Restarting with the same VZ home and owner recovers
private `pulse.json` properties beside each VM. No native start is replayed.
The provider sends the opaque lease through stdin to c2j, which retrieves secrets.

`Drain` retires capacity after accepted jobs finish; `PurgeImages` removes only
known prepared sources once the pool has drained. UID and ownership checks
protect unrelated VMs. Shared host volumes remain unsupported.

Default slots support Docker when a launch requests `docker`. Linux starts its
own daemon on demand; macOS uses a private Linux companion and the signed Go
bridge child mode. No host Python is needed for serving. On supported hardware,
Linux guests expose KVM and advertise `kvm`. Jobs can create TAPs dynamically,
use a shared outbound network, and run independent user services; see
[Linux/KVM networking](docs/linux-kvm.md) and [VZ tests](docs/vz-testing.md).

## Tests

```sh
make test vet build
python3 -B -m unittest discover -s scripts -p '*test.py'
# Live tests use disposable VMs; reserve host capacity before running:
./scripts/test-vz-public.sh
python3 scripts/test-vz-docker-verified.py
```

See [VZ setup and qualification](docs/vz-testing.md) and
[Docker lifecycle](docs/docker-library.md). Live VM tests are opt-in.

### Per-launch logs

The provider API supports optional `launch` progress and `combined` worker-output
listeners, plus `ProviderWithLaunchLogs.GetLaunchLog` for retained output.
Logging does not change admission or cancel jobs. Guest capture and
host archival continue independently of listeners, so a restarted provider can
resume copying retained output from surviving VMs.

Host archives default to `~/.pulse/vz/logs`, partitioned by owner scope and hashed
LaunchID. Inspect copied output without opening a second controller:

```sh
pulse-macvz launches
pulse-macvz launches show LAUNCH_ID
pulse-macvz logs LAUNCH_ID --follow
```

Use `Config.Logging.Directory` when embedding, or `--log-directory` on the CLI.
`--log-segment-mib`, `--log-backups`, `--log-compress`, `--log-max-bytes`, and
`--log-max-age` control rotation and retention. Defaults are 16 MiB guest segments,
three compressed backups, a 1 GiB host budget per owner scope, and seven days for
completed archives. Output can be partial after retention, storage failure, or VM
loss; the API and local reader report this explicitly.

`GetLaunchLog` reads the host archive without contacting VMs. It returns up to
100 records per page with checkpoints that survive provider restarts, and reports
finalization and incomplete output separately from job status. It works through
both inbound and reverse plugins without a live listener. Poll the returned
checkpoint with a delay when caught up but not finalized.

Existing archives can be retrieved after updating the provider binary. New guest
capture helpers also preserve record numbers and capture timestamps; run `upgrade`
to install them in prepared images. Older captured output remains readable with
stable offset-based sequence numbers, zero (unknown) timestamps, and
`Incomplete=true`. Images predating capture report logs unavailable until upgraded.
Live callbacks do not reconnect automatically; retained retrieval is independent.
See [logging design and limits](docs/provider-logging-proposal.md).
