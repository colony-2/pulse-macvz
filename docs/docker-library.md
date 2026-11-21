# Docker execution on VZ

All conventional macOS and Linux slots are Docker-eligible by default. A launch
must request the `docker` execution capability to activate Docker. Ordinary
Linux jobs do not start the daemon, and ordinary macOS jobs do not allocate a
Linux companion. `--macos-docker-capacity=-1` makes all macOS slots eligible.

Linux executes Docker locally. A macOS allocation pairs its suspended macOS
primary with an isolated Linux ARM64 engine. Each pair has independent engine
storage, credentials and identities. Guest forwarding provides Docker and
published ports to the macOS job. Shared bind mounts are unsupported.

The host endpoint runs as a launchd service using the signed executable's
`__pulse-vm bridge` child mode. No Python interpreter or separate bridge binary
is needed on the host. The bundle association and signing identity provide
stable macOS Local Network consent. The default reachable host address is
`192.168.64.1`, with ports 45000–45099. Use nonoverlapping port ranges for
independent owners on one machine.

```go
runtime, err := macvz.NewVZ(
    macvz.Config{OwnerID: "mac-pool"},
    macvz.VZConfig{DockerEndpoint: macvz.DockerEndpoint{
        Address: "192.168.64.1", PortMin: 45000, PortMax: 45099,
    }},
)
```

Preparation verifies the worker's access to Nix and Docker. Linux KVM access is
also verified when the host supports nested virtualization. Docker allocation
readiness is checked before admission. Private VM properties support recovery
without replaying native launches or admitting a second job to an occupied pair.
Cleanup waits for native stopped observations and validates identities before
removing either member.

Use `pulse-macvz setup` for missing capacity and `pulse-macvz upgrade` to drain
and refresh prepared images. Both share configuration with the server. Stop the
pool's current controller first; the owner lock rejects concurrent management.

Run `python3 scripts/test-vz-docker-verified.py` for the signed-network gates and
full Docker qualification, or `scripts/test-vz-docker-qualification.sh` with an
already verified runner. The suite checks concurrent jobs, controller and bridge
restarts, guest/engine loss, cleanup, isolation, and resource pressure. It uses
up to two macOS and two Linux VMs and a persistent prepared-source cache.
See [VZ qualification details](vz-testing.md) and [CLI options](server-cli.md).
