# Linux/KVM allocation networking

The outer allocation VM is the isolation boundary. On KVM-capable Linux hosts,
all Linux jobs receive `/dev/kvm` access and `CAP_NET_ADMIN` inside their allocation.
This permits creating TAPs, bridges, routes, and firewall rules without sudo.
It does not confer privileges on the Mac host. Docker still starts only when the
launch requests the `docker` capability.

`/etc/pulse-macvz/network.json` describes the default nested network: bridge,
subnet, prefix/netmask, gateway, DNS, and usable guest address range. The provider
selects an unused private IPv4 /16 and preserves it across reboots. Each nested
NIC needs its own TAP and unique MAC address. Workloads assign unique addresses
from the descriptor; the provider does not impose a fixed TAP count or run DHCP.

For example, inside a normal job (which already has the capability):

```sh
cat "$PULSE_KVM_NETWORK"
ip tuntap add dev guest0 mode tap
ip link set guest0 master pulsebr0
ip link set guest0 up
# Start the VMM with guest0 as its TAP, a unique MAC, and network settings
# from network.json. Delete the TAP when the nested guest is finished.
ip link delete guest0
```

The bridge supports communication between nested guests and their outer worker,
and IPv4 forwarding/NAT permits general outbound access. DNS is forwarded from
the bridge gateway through the outer VM's resolver, over UDP or TCP. There are no
nested destination allowlists. Unsolicited connections through the Mac's upstream
NAT are not automatically exposed. Workloads may administer additional networks
inside their allocation if the supplied default does not fit.

The lingering `pulseworker` user manager is started during preparation. Job
processes receive:

```sh
XDG_RUNTIME_DIR=/run/user/55050
DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/55050/bus
PULSE_KVM_NETWORK=/etc/pulse-macvz/network.json
```

For independently supervised services, explicitly pass the environment and use
`pulse-kvm-run` to grant network administration to that service. User services do
not automatically inherit the launching process's capabilities or environment:

```sh
systemd-run --user --collect --unit=my-nested-vm \
  --setenv=XDG_RUNTIME_DIR=/run/user/55050 \
  --setenv=DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/55050/bus \
  --setenv=PULSE_KVM_NETWORK=/etc/pulse-macvz/network.json \
  /usr/local/bin/pulse-kvm-run /path/to/cloud-hypervisor ...
systemctl --user status my-nested-vm
journalctl --user -u my-nested-vm
```

The service survives the invoking session. Its lifetime is still bounded by the
outer allocation: finishing the job can shut down that allocation. The provider
does not supervise the nested workload's health or reboot policy.

## Preparation and rollout

Fresh Ubuntu bases and unused allocations use the same idempotent setup. Existing
prepared bases can be reused: a separate networking revision causes outdated idle
allocations to be configured before their next handoff. Accepted allocations are
excluded; the guest also refuses preparation if a durable launch receipt exists.
Restart the provider with the updated binary to repair unused allocations. An
upgrade is optional for refreshing reusable bases. Existing accepted jobs retain
their current environment until they finish.

Boot services reconstruct the bridge/forwarding and verify the current boot.
Handoff checks exercise KVM VM creation and TAP creation as UID/GID 55050, including
inside a transient user service. A successful old readiness receipt alone is not
sufficient. The implementation uses installed Ubuntu networking/systemd tools;
it copies `setpriv` to a root-owned, worker-executable file with only
`cap_net_admin=ep`, without changing distribution binaries or the Nix store.

## Qualification

The opt-in `TestLiveLinuxLifecycle` test with `PULSE_TEST_VZ_KVM=1` now exercises
ordinary-user KVM boot, twelve dynamically created TAPs, two nested guests, an
independent user service, peer/outer HTTP, DNS, and Internet HTTP. It operates on
a private clone and installs QEMU/test fixtures only in that disposable clone.
Set `PULSE_TEST_VZ_KVM_DOCKER=1` to repeat networking with Docker active,
including a container launched through the ordinary user's service manager.
The test also exercises the installed launcher's UID/capability transition and
checks that preparation refuses an accepted test launch.
Production base images and accepted jobs remain unchanged.

## ARM64 interrupt-controller trapping

ARM64 Ubuntu worker images enable these outer-kernel arguments by default:

```text
kvm-arm.vgic_v3_group0_trap=1
kvm-arm.vgic_v3_group1_trap=1
kvm-arm.vgic_v3_common_trap=1
```

These configure the worker's KVM implementation, not the command line of guests
launched by Cloud Hypervisor or QEMU. They enable emulation of nested GICv3 system
register accesses. The kernel reports `GICv3 sysreg trapping enabled ([G0G1C],
reduced performance)`; this trades some performance for reliable nested guest I/O.
The three-boot reproduction and exact environment are recorded in
[ARM64 GIC qualification](gic-traps-qualification.md).

There is no opt-in flag. Preparation writes a dedicated
`/etc/default/grub.d/99-pulse-kvm-gic-traps.cfg` and runs `update-grub`, preserving
other GRUB settings. The builder shuts down before it is used as a clone source.
Readiness verifies all three arguments in the booted worker's `/proc/cmdline`
before handoff, including launches that do not request `kvm`.

The prepared ARM64 Linux image name and allocation revision change with this
policy. After deploying the new binary, `pulse-macvz setup` prepares the new base
and replaces unused old workers. Normal provider reconciliation does this too.
An `upgrade` is only needed if you also want to refresh the software in your
other bases. Accepted jobs keep their original VM until completion; the provider
does not edit their boot settings or reboot them. macOS workers, x86 Linux workers,
and software-only Docker companion images retain their existing preparation.
