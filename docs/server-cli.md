# Provider server CLI

`pulse-tart server` is a Cobra command using the published
`github.com/colony-2/pulse-provider-api/plugin` SDK. It defaults to **outbound**
mode and the **VZ** backend. The Go library remains available unchanged; this
command owns transport and reconciliation when used as the standalone host.

Long flags use `--full-name`; short aliases use one character, such as
`-m outbound`, `-b vz`, `-l 4`, or `-h`. Run `pulse-tart server --help` for all
options. All three commands share a versioned JSON configuration file; transport
uses the published SDK unchanged.

## Install and prepare

Build and install in Go's normal binary directory (`go env GOBIN`, or the
first `GOPATH` entry's `bin` directory):

```sh
make install
# Put the Go binary directory on PATH, then run from any directory:
pulse-tart setup --owner production --macos-capacity 2 --linux-capacity 4 \
  --collector pulse.example.com:8443 \
  --tls-ca /etc/pulse/ca.pem --tls-cert /etc/pulse/worker.pem \
  --tls-key /etc/pulse/worker.key
pulse-tart server
```

On Apple Silicon macOS, `make install` builds and signs `PulseProvider.app`
in that directory and installs `pulse-tart` as a symlink to its executable.
It uses the signing policy described below. Other platforms get the cgo-free
CLI executable directly. To select a destination, use
`GOBIN="$HOME/.local/bin" make install`. For a custom app location, use
`scripts/build-server-app.sh /absolute/path/PulseProvider.app` instead.

The installed app contains one cgo-free executable. That same executable runs
Cobra commands, VM children, and the Go Docker bridge through private child
modes. It embeds the guest installation assets; production VZ operation needs
neither a source checkout, host Python, nor a second signed helper. Python in
the repository is for development/qualification tools and the legacy Tart bridge.
Keep the app bundle intact even when invoking its executable through a symlink.
Rebuilding with `build-server-app.sh` stages and verifies the entire signed app
before atomically replacing the installed bundle. A build/signing failure leaves
the installed app intact. Keep one installed production copy; development
verification uses a separate identity and persistent installation.

The build uses `PULSE_CODESIGN_IDENTITY` or selects a single Developer ID or
Apple Development identity; otherwise it falls back to ad-hoc signing. Use a
stable release identity for persistent Local Network consent and apply your
release notarization policy before distribution. `setup`/`upgrade` register the
app with LaunchServices when macOS Docker companions are configured. Run that
installation step from an ordinary terminal; the server does not re-register on
every startup. Allow macOS's Local Network prompt for Pulse Provider when it
first connects. See [VZ signing and qualification](vz-testing.md).

`setup` checks the backend, prepares missing images/templates/allocations, and
saves settings after capacity is ready. It reuses prepared sources and does not
connect to Pulse, so collector credentials may be omitted until serving. Pass
collector/TLS flags to `setup` too if they should be saved. Downloads and guest
preparation can take minutes on first use.

VZ registry imports run in the background before a VM is created. An import
does not hold the VM lifecycle or reconciliation locks while downloading, so
other guest preparation and ready allocations can continue. Closing the
provider cancels pending imports; completed registry cache entries are reused
on restart. Import failures are reported by reconciliation and can be retried.

Setup prints the owner, VM home, shared concurrency limits, and every requested
slot, grouped under **Base images** and **Slots**. By default (`--progress auto`),
an interactive terminal refreshes changed lines in place as reconciliation polls.
Redirecting stderr to a file or pipe automatically selects linear output, which
prints changed rows on phase changes and roughly every ten seconds while polling,
with a heartbeat when nothing has changed. No cursor controls are written to logs.
Use `--progress linear` to select this mode in a terminal too:

```sh
pulse-tart setup --macos-base all --progress linear 2>&1 | tee setup.log
```

`--progress interactive` requests the in-place display when stderr is a capable
terminal. Redirected output, `TERM=dumb`, and terminals too small to show the
whole display fall back to linear output so every slot remains visible. Progress
mode applies only to the current command and is not saved in provider configuration.

The display shows base preparation, guest SSH/command stages, snapshot saving,
and Docker allocation readiness. Registry imports show
compressed bytes read versus the manifest's total, average throughput, and a
rough transfer ETA. This is not the virtual disk size and the ETA excludes later
guest installation and snapshot work. Unknown totals, stalled transfers, cache
hits, and phases with no meaningful ETA are labeled explicitly.

For local macOS base builders, a separate SSH observer reports the last guest
observation, preparation log size, and observed restarts. Log size indicates
activity, not download completion. After a lost preparation connection, the
observer checks the guest's durable readiness marker, expected OS version, and
installed tools before recording readiness and requesting shutdown. It never
replays a job command. Three observed guest restarts before readiness cause setup
to fail explicitly; the preparation deadline also remains anchored to VM creation
across controller restarts. A failed base does not block reconciliation of other
capacity.

VZ cold boots of macOS images expose a minimal guest compatibility console. Some
Cirrus images contain a Tart guest agent that signals `launchd` to reboot if
that console is missing. This does not require the Tart executable or enable
Tart RPC/clipboard services. The device choice is persisted with the VM;
existing suspended templates retain their original device graph when restored.
Linux uses cached virtio disk I/O with full synchronization, matching
[Tart's workaround for Linux filesystem corruption](https://github.com/cirruslabs/tart/pull/675).

On hosts supporting nested virtualization (macOS 15+ on M3 or later), VZ enables
it for every Linux ARM VM and automatically advertises the `kvm` execution
capability for Linux ARM bases. No extra flag or configured capability is needed.
A launch may request `kvm`, or both `docker` and `kvm`; jobs without either
requirement can still access KVM. macOS bases and unsupported hosts do not
advertise `kvm`. It accelerates ARM guests, not x86 guests.

The Linux kernel must expose `/dev/kvm`. Before admission, the provider verifies
that `pulseworker` can open it and create a KVM VM without elevated privileges.
Existing prepared bases are reused; unused Linux allocations created without
this capability are recycled, while accepted jobs finish normally. Update both
the provider library and any separately configured VZ runner, then run `setup`
with the server stopped. Containers launched by a job still need explicit device
access, for example `docker run --device=/dev/kvm ...`.

`upgrade` waits for accepted jobs to finish, retires this owner's allocations,
removes its inactive prepared images, and prepares fresh capacity using the
selected c2j version. For example:

```sh
# Stop the provider server first; accepted VM jobs survive server shutdown.
pulse-tart upgrade --c2j-version latest --timeout 2h
pulse-tart server
```

Maintenance uses the normal ownership lock and refuses to race another server.
A timeout or cancellation does not cancel accepted jobs. There is downtime for
rebuilding capacity; once old sources have been removed, a preparation failure
requires retrying. `upgrade` refreshes guest images/tools, not the host executable:
install a newer signed app bundle separately, then run it. No background service
is installed by these commands.

All commands read `~/.pulse/provider.json`, or `--config/-f PATH`. Explicit flags
win over saved settings. `setup` and successful `upgrade` save effective settings
atomically with mode 0600; `server` does not rewrite them. The format is:

```json
{"version": 1, "flags": {"backend": "vz", "owner": "production", "macos-capacity": "2", "linux-capacity": "4"}}
```

Flag values are strings, using the same syntax as CLI arguments. Unknown options
and versions fail validation. Relative paths in the file resolve against its
directory; paths supplied on the command line resolve against the current
directory. Generated configurations save absolute paths. Credentials remain in
their certificate/key/token files; configuration stores those paths. A missing
default config is allowed; an explicitly named missing file is allowed only for
`setup`. An omitted `total-capacity` continues to follow macOS + Linux capacity.

For a bare executable, use `scripts/build-server.sh OUTPUT`; Linux builds can
control Orchard. Local VZ requires Apple Silicon macOS; local Tart requires its
supported host. `--runner /absolute/runner` remains available for embedding or
specialized installations, but is unnecessary with the bundled CLI.

## Outbound: provider connects to Pulse

```sh
pulse-tart server \
  --collector pulse.example.com:8443 \
  --name mac-worker-01 \
  --tls-ca /etc/pulse/collector-ca.pem \
  --tls-cert /etc/pulse/mac-worker-01.pem \
  --tls-key /etc/pulse/mac-worker-01.key \
  --owner production \
  --macos-capacity 2 \
  --linux-capacity 4
```

The Pulse side must run the SDK's collector and authorize the client certificate
as `mac-worker-01`. `--name` defaults to `--owner`. The collector address is
`host:port`, not an HTTP URL. `--tls-server-name` defaults to the collector host;
set it explicitly when dialing an IP address for a DNS certificate.

The connection requires verified mutual TLS 1.3 and the SDK's reverse ALPN.
The provider opens no public listener. Use direct TCP or TCP passthrough; a normal
HTTP reverse proxy is not compatible. No token flag or insecure TLS mode is
provided. Certificates are loaded at startup; restart the server after replacing
its certificate files.

The default reconnect policy allows five transient replacement attempts, starting
at one second, doubling up to 30 seconds with 20% jitter. One healthy minute
resets the budget. Set `--reconnect-attempts 0` to disable reconnects; the delay
and reset settings have corresponding flags. Authentication, identity, protocol,
and initialization rejection are terminal SDK errors. Reconnect never resends a
submission, and starts a fresh runtime only after the previous session closes.

## Capacity

| Flag | Default | Meaning |
| --- | --- | --- |
| `--macos-base` | `all` | Both `macos-26-arm64` and `macos-15-arm64`; accepts one name or a comma-separated selection |
| `--macos-capacity` | 2 | Total concurrent macOS job slots, limited to 0–2 |
| `--linux-capacity`, `-l` | 4 | Linux job VM slots; Docker starts inside the VM only when requested |
| `--macos-docker-capacity`, `-d` | -1 (all) | Slots per macOS version eligible for Docker; 0 disables it, a positive count limits eligibility |
| `--total-capacity`, `-c` | macOS + Linux | Shared job limit; explicitly setting 0 drains all capacity |

All macOS slots support ordinary and Docker-capable launches by default.
Only a launch carrying the `docker` capability starts a private Linux companion.
Linux workers warm with Docker software installed but its daemon and socket
stopped. A Docker request starts the daemon before admission; an ordinary
request receives no Docker environment variables. Preparation retains no lease:
the controller retries while the requested Docker environment becomes ready.

`--macos-docker-capacity 1` limits Docker eligibility to one template per version;
that template can also run an ordinary job. Companions and preparation builders
are not job slots. Each job VM uses 4 vCPU / 8 GiB; a macOS Docker companion uses
another 4 vCPU / 8 GiB while allocated.

Each selected macOS version has its own prepared base and suspended templates.
The running-job limits remain shared: the default creates two templates for
macOS 26 and two for macOS 15, but runs at most two macOS jobs in total. Setup
waits for every selected version's templates and the warm Linux workers,
without starting Docker. macOS templates remain suspended until requested. Pending template preparation may retire an idle
macOS Docker worker when both physical VM positions are occupied; accepted jobs
are preserved. Linux uses the library's pinned Ubuntu ARM64 base.
Set either OS capacity to 0 to disable it. Reduced capacity is reconciled without
terminating accepted work; it can take time to retire occupied allocations.

Existing saved configurations keep explicit settings, including the old
`--macos-docker-capacity 0`. To enable Docker eligibility for every macOS slot,
run setup/upgrade with `--macos-docker-capacity=-1`. Existing saved configurations
keep their explicit base selection. To add both
versions to an existing single-version setup, stop its controller and run:

```sh
pulse-tart setup --macos-base all
```

Keep the same configuration file, owner and VM home as the serving controller.
When Pulse embeds the library, configure matching bases/slots in Pulse too;
Pulse does not automatically read the CLI's saved configuration. Base selectors
use advertised names (`macos-15*`, for example), not macOS point-version aliases.

macOS Docker requires a worker address reachable by its guests. The defaults are
`--docker-worker-address 192.168.64.1`, `--docker-port-min 45000`, and
`--docker-port-max 45099`. Reserve a separate port range for each owner using
companions on the same worker. Linux-only Docker needs no host bridge. For
Orchard, set the worker address to that remote worker's reachable IPv4 address.

## Inbound: Pulse launches a local plugin process

```sh
pulse-tart server --mode inbound --backend orchard \
  --orchard-url https://orchard.example.com \
  --orchard-worker mac-worker-01 \
  --orchard-username provider \
  --orchard-token-file /etc/pulse/orchard-token \
  --owner production
```

These are the executable arguments for the host's `plugin.Start` configuration,
not a standalone public TCP listener. The host creates the authenticated local
transport and parent-loss pipe. Invoke it through the SDK; running the command
alone lacks the required handshake. Do not pass outbound `--collector`, `--name`,
or `--tls-*` options in inbound mode. The host supplies its initialization name,
an absolute base directory, and an explicit child environment, including HOME and
PATH as needed. Provider configuration comes from the saved settings and CLI flags; `ConfigYAML`
must be empty or `{}`. Executor-environment overrides are rejected explicitly.

**Current upstream restriction:** the pinned SDK supports inbound `Start`/`Serve`
only on Linux. On macOS it returns `plugin: process safeguards unavailable on this
platform`, before constructing a provider. Thus inbound can currently run on a
Linux host controlling Orchard, but cannot yet run a local macOS VZ/Tart provider.
Use outbound on macOS. The requested Darwin safeguards work is tracked in the
`pulse-provider-api` cell as job `3MMBDIgfqQlxtJzTCnyTaSfHGrh`.

## Ownership, lifecycle, and diagnostics

`--owner` defaults to `server`; keep it stable across restarts. The default state
directory is `~/.pulse-tart/BACKEND/OWNER`, with `--state-dir` available to override
it. VZ defaults to `~/.pulse/vz`, Tart to `~/.tart`; `--vm-home` overrides either.
Existing `~/.pulse-vz` and Tart data are not moved or deleted. To keep using an
old VZ home, explicitly pass `--vm-home "$HOME/.pulse-vz"` to `setup`.
Paths accept relative paths or `~/` and are resolved before serving. Orchard
requires `--orchard-url` and `--orchard-worker`; its token is read from a file,
not passed as the token value in argv. `--orchard-allow-http` is an explicit lab
option. Never run two controllers for the same backend/home/owner scope.

Provider construction and `Check` occur after SDK authentication. Reconciliation
prepares missing images and maintains allocations in the SDK session lifetime;
a reconciliation failure stops the server with a nonzero exit status. RPCs do
not own accepted VM jobs. Session loss or SIGINT/SIGTERM cancels maintenance and
closes the runtime, preserving accepted jobs for recovery after restart. Inbound
process transport cleanup is managed by the SDK and the dedicated CLI process.

`--rpc-concurrency` defaults to 4, `--max-message-bytes` to 16 MiB,
`--startup-timeout` to 30 seconds, and `--shutdown-timeout` to 5 seconds.
`--poll-interval` defaults to 2 seconds and `--bootstrap-timeout` to 45 minutes.
Image preparation happens in reconciliation, not inside the startup handshake.
Status/error output uses stderr; inbound stdout is reserved for the SDK handshake.

For sandbox permissions and VM/endpoint diagnostics, use [VZ testing](vz-testing.md).
Building/signing the CLI does not change sandbox permissions. macOS Docker
companions still require launchd job creation in addition to VZ/network access.

## Verification

```sh
go test -race ./internal/servercli
PULSE_TEST_SERVER_BINARY="$HOME/Applications/PulseProvider.app/Contents/MacOS/pulse-tart" CGO_ENABLED=0 \
  go test ./internal/servercli -run '^TestLiveVZServer$' -v -count=1
```

The ordinary suite tests capacity/flag validation, lifecycle cleanup, and a real
SDK collector with temporary mutual-TLS certificates, provider API calls, and
forced transport loss/reconnection. The inbound subprocess test runs on Linux
and explicitly skips elsewhere due to the upstream restriction. The opt-in VZ
smoke test runs the signed CLI with a private home and zero capacity, verifies
registration/discovery/list and SIGTERM, and checks that no VMs were created.
It passed on this Apple Silicon host. This is not a replacement for the existing
VM lifecycle, JobDB, or Docker qualification suites.
