# Provider server CLI

`pulse-tart server` is a Cobra command using the published
`github.com/colony-2/pulse-provider-api/plugin` SDK. It defaults to **outbound**
mode and the **VZ** backend. The Go library remains available unchanged; this
command owns transport and reconciliation when used as the standalone host.

Long flags use `--full-name`; short aliases use one character, such as
`-m outbound`, `-b vz`, `-l 4`, or `-h`. Run `pulse-tart server --help` for all
options. There is no configuration file or new wire protocol.

## Build

```sh
./scripts/build-server.sh "$PWD/.local/bin/pulse-tart"
.local/bin/pulse-tart server --help
```

The build is cgo-free. On Apple Silicon macOS, the script signs the binary
with the virtualization entitlement and a fixed signing identifier. It uses
`PULSE_CODESIGN_IDENTITY` or automatically selects a single Developer ID or
Apple Development identity; without one, it falls back to ad-hoc signing.
Persistent Local Network consent also requires correct app identity metadata.
The live-qualified Docker configuration uses the bundled runner from
`scripts/build-vz-app.sh`, passed through `--runner`; the bare server executable
has not been qualified for launchd Local Network consent. See [VZ signing](vz-testing.md).
It includes the VM runner: no second binary
is required. Production packaging should sign with the application's release
identity and preserve its other entitlements. `--runner /absolute/pulse-vz` can
select a separate signed VZ runner instead. Linux builds can control Orchard;
local VZ requires Apple Silicon macOS, and local Tart requires its supported host.

## Outbound: provider connects to Pulse

```sh
.local/bin/pulse-tart server \
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
| `--macos-base` | `macos-26-arm64` | One macOS base; `macos-15-arm64` is also available |
| `--macos-capacity` | 2 | Total concurrent macOS job slots, limited to 0–2 |
| `--linux-capacity`, `-l` | 4 | Linux job VM slots; Docker runs inside each VM |
| `--macos-docker-capacity`, `-d` | 0 | Part of macOS capacity configured with Docker companions |
| `--total-capacity`, `-c` | macOS + Linux | Shared job limit; explicitly setting 0 drains all capacity |

For example, `--macos-capacity 2 --macos-docker-capacity 1 --linux-capacity 3`
configures one ordinary macOS slot, one Docker-capable macOS slot, and three
Linux/Docker slots. A Docker-capable macOS slot uses an additional private Linux
companion. Companions and preparation builders are not job slots. Each job VM
uses 4 vCPU / 8 GiB; a macOS Docker companion uses another 4 vCPU / 8 GiB.

The CLI selects one macOS base so its ordinary/Docker slot counts are global,
not multiplied across per-version pools. The library still supports multiple
bases with shared OS limits. Linux uses the library's pinned Ubuntu ARM64 base.
Set either OS capacity to 0 to disable it. Reduced capacity is reconciled without
terminating accepted work; it can take time to retire occupied allocations.

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
PATH as needed. Provider configuration comes from the CLI flags; `ConfigYAML`
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
it. VZ defaults to `~/.pulse-vz`, Tart to `~/.tart`; `--vm-home` overrides either.
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
PULSE_TEST_SERVER_BINARY="$PWD/.local/bin/pulse-tart" CGO_ENABLED=0 \
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
