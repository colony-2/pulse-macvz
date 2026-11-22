# Linux/KVM networking and user supervision

## Decision and isolation boundary

The outer Linux VM allocated by pulse-macvz is the protection boundary. Nested VMs do not need additional network isolation from the job account or from one another. Nested guests should have general outbound connectivity through the outer VM, subject to the same upstream reachability as that allocation. Do not impose destination allowlists or a fixed TAP pool.

This plan replaces the earlier suggestion of pre-reserving a bounded set of TAPs. Each nested virtual NIC uses its own TAP. Workloads create and remove TAPs as needed and can attach them to a common bridge.

## Implementation

1. **Enable ordinary-user network administration inside the allocation.** Ensure pulseworker (UID/GID 55050/55050) can use `/dev/net/tun`. Supply `CAP_NET_ADMIN` to Linux/KVM job execution and independently supervised user services. Verify actual capability inheritance through the installed launcher and user service manager, rather than relying on a device permission or declared setting. This deliberately permits administering interfaces, bridges, addresses, routes and firewall rules inside that disposable outer VM. Do not grant host privileges, general sudo, or unrelated capabilities such as `CAP_SYS_ADMIN`.

2. **Provide a usable default network.** Prepare one managed Linux bridge, a collision-checked private IPv4 subnet, a gateway address, IPv4 forwarding and outbound masquerading through the allocation's uplink. Enable return traffic and communication between attached nested guests; apply no destination restriction beyond the allocation's existing upstream network. Preserve unrelated configuration during preparation and integrate correctly with Docker's forwarding rules. This is default outbound access, not automatic unsolicited inbound exposure through the Mac's NAT.

3. **Make DNS and configuration usable.** Supply reachable DNS servers, not the allocation's loopback resolver address. If needed, provide DNS forwarding for nested guests. Publish the bridge, subnet/prefix, gateway, DNS and attachment instructions in a stable, readable guest configuration file. Cloud Hypervisor/workload code creates TAPs and assigns unique guest addresses within that subnet, then fills its existing network entries. No new Pulse provider protocol fields are needed. There is no provider-imposed nested-VM count; the address space and allocation resources still have finite capacity.

4. **Enable independent user supervision.** Enable lingering for pulseworker and start `user@55050.service`. Verify the owned runtime directory and user bus. Set Linux launch defaults `XDG_RUNTIME_DIR=/run/user/55050` and `DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/55050/bus`, and explicitly propagate required environment and network-admin capability to user services. Use user services without interactive system-service authorization. Services remain bounded by the lifetime of the outer allocation.

5. **Integrate with normal preparation and handoff.** Add shared, idempotent capability setup for fresh Ubuntu bases and unused Linux allocations. Install the boot-time service/configuration needed to reconstruct the managed bridge and forwarding on subsequent boots; TAPs themselves remain workload-created. Record a separate versioned readiness result, including boot identity, independent of Nix setup. Gate KVM handoff on successful ordinary-user operations. Reconcile outdated unused workers before their next assignment. If an older guest launcher must be updated for capability propagation, update it only while the allocation is unused.

6. **Qualify behavior.** In disposable Linux/KVM allocations, verify UID/GID, KVM VM creation, dynamic TAP creation/attachment/deletion, bridge/address configuration and repeated idempotent setup. Create more TAPs than the previously suggested fixed pool to demonstrate that no such count limit exists. Verify ordinary-user services survive the launching session, retain their environment/capabilities, and can create TAPs themselves. Demonstrate nested-guest DNS and general outbound connectivity, guest-to-outer-VM and guest-to-guest communication, including Docker-enabled and Docker-disabled allocations. Include handoff/readiness regression tests and check that accepted allocations are never selected for preparation.

## Rollout and exclusions

- No live repair of the previously supplied allocation or boot ID. No mutation, replacement, restart or replay of accepted work.
- Apply through fresh-image preparation and the existing unused-allocation lifecycle. Existing accepted jobs finish unchanged.
- Do not change Mac networking, physical-parent configuration or host permissions.
- No new instance provisioning/resizing workflow or public provider API. Use installed tools and the existing image/qualification paths; identify any missing prerequisite explicitly.
- Cloud Hypervisor retains its guest assets, fixture/helper deployment, independent observations and eleven LIVE assertions. Separately coordinated reboot/fault tests remain separate.

## Status

Implemented and qualified on disposable Ubuntu ARM64 clones. Fresh bases and
unused workers share setup; a separate persisted revision schedules idle repair.
The installed launcher passes only NET_ADMIN through a root-owned setpriv copy,
and user services explicitly use the same wrapper. Boot services restore and
verify the network. Saved MAC/boot intent makes interrupted bridge creation
recoverable without adopting an unrelated interface.

Validation passed: all Go tests, provider race tests, go vet, 60 Python tests,
and live lifecycle qualification with Docker off and on. Live checks covered
KVM creation/boot as UID 55050, twelve dynamic TAPs, two nested guests, independent
user services, peer/outer/Internet HTTP, UDP and TCP DNS, Docker container access,
repeated setup preserving unrelated resources, reboot/cold boot, actual launcher
handoff, and refusal to prepare an accepted synthetic test launch. Evidence is in
`test-results/kvm-networking/`. All disposable VMs are stopped.

No production allocation was mutated. Install the new build and restart the
separate provider process to apply it to unused allocations; existing accepted
work stays unchanged. An upgrade is optional for refreshing the reusable bases.
Cloud Hypervisor's own eleven assertions remain separate.
