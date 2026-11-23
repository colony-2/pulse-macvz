# ARM64 nested-KVM GIC trap qualification

On 2026-11-23, the supplied virtio-disk reproducer passed three fresh nested boots
with the production `configure-linux-gic-traps.py` applied to a disposable copy
of the current prepared Ubuntu worker image. The three arguments are now standard
for ARM64 Linux worker preparation; there is no configuration flag.

## Environment and identity

- Host: Apple M4 Max; macOS 26.7.3, build 25G342.
- Worker: Ubuntu 24.04 ARM64, kernel `7.0.0-42-generic`, 4 vCPU / 8 GiB.
- Original prepared source: `pulse-server-id-ca1c953085473b708781-base`,
  UID `f7f990bb59b9463f4431fcf558bb7989`.
- Registry source: `ghcr.io/cirruslabs/ubuntu@sha256:439ba447488fa19f9b1ed091602546b8cb32442abdb60bac4943a4d6c3dd2ffa`.
- c2j: `unstable-3991db6`, revision `3991db6930ec62fb0bf85e4da824d6a8fc4fdfa7`.
- Test worker: `code-worker`, UID `53894350610f025224e880fb635fa926`,
  boot ID `d352cd11-64f7-49a5-bd6e-f3c540d74f9a`.
- Published canary image: `pulse-gic-canary-id-1b1839dcaedcd226fd61-base`,
  UID `9256b2da4f3794d4aeb36ec1354db509`.
- Local image directory:
  `~/.pulse/vz/canaries/gic-traps-20261123/vms/pulse-gic-canary-id-1b1839dcaedcd226fd61-base`.

The published image is a cold clone of the builder, before installing test tools.
The production source and accepted jobs were not changed. This validation reused
the prepared software; it did not repeat the full package/Nix build.

The booted worker's `/proc/cmdline` contained all three arguments exactly once:

```text
kvm-arm.vgic_v3_group0_trap=1 kvm-arm.vgic_v3_group1_trap=1 kvm-arm.vgic_v3_common_trap=1
```

The kernel confirmed `GICv3 sysreg trapping enabled ([G0G1C], reduced performance)`.
As `pulseworker` UID/GID 55050, opening `/dev/kvm` read/write returned API version
12 and `KVM_CREATE_VM` succeeded. All reproducer processes ran as that user.

## Reproduction

The unchanged [supplied archive](http://db.lab.c2.run:8081/api/projects/c2/jobs/3MWDMOhLIs4CmUu9gm6AEl6m6Mh/stored-document?ordinal=4&name=arm64-virtio-repro.tar.gz)
had SHA-256 `212adc89f1fe88fe3489469f8d6a3bedd9b0dd2e24e01c3ac58a5b487ffc80b6`.
Its `SHA256SUMS` checks passed. The official Cloud Hypervisor v55.0 static ARM64
binary had SHA-256 `446de7552dacc83bc86d1137e7ffa7c2d204cdda4850eac4f2d2666d63c0f697`,
matching the release asset digest.

Each invocation used the reproducer's normal PCI/MSI-X configuration, a new nested
VM and a fresh blank 64 MiB disk. No ITS-disable or MMIO workaround was added:

```sh
python3 run.py --guest-init ./init.aarch64 --mode cloud-hypervisor
```

| Run | complete | timeout | elapsed_s | exit | Last guest marker |
| --- | --- | --- | --- | --- | --- |
| 1 | true | false | 6.95 | 0 | BLOCK complete 1 3.879 |
| 2 | true | false | 6.79 | 0 | BLOCK complete 1 3.882 |
| 3 | true | false | 7.01 | 0 | BLOCK complete 1 4.058 |

These runs completed from 19:02:30 to 19:03:18 UTC. Reported `elapsed_s` measures
the nested VM run; the surrounding script also prepares/downloads test assets.
An earlier manually configured canary also passed 3/3 (6.05s, 6.16s, 5.99s).
This demonstrates the supplied disk-I/O case, not the original job's full Ubuntu
builder verification or a definitive diagnosis of the underlying virtualization bug.

## Provider lifecycle and handoff

The updated CLI ran ordinary `setup` in the isolated `gic-canary` owner/home with
one Linux slot and zero macOS capacity. It adopted the new cold source, retired
the previous unused allocation, and reached ready in 15 seconds. No opt-in
argument was supplied.

The resulting idle worker is
`pulse-4bf7ce42228a-92e1598cf0326ea9b13f8aced867ea70-primary`,
UID `45cd44306f6e49afd38342ed2bd5be75`, boot ID
`bb34a50c-4f27-42b4-8d2b-45cfe8337c42`. Its kernel arguments, kernel trap diagnostic,
and ordinary-user KVM VM creation were independently verified. No launch intent
or fingerprint was present. The pool is not connected to a collector.

The isolated pool config is
`~/.pulse/vz/canaries/gic-traps-20261123/provider.json`. The source remains available
for job `3MUMNt3TrDzumi5EOfs8s0PAFwL`; deployment and that job's full builder
verification are the next steps. Temporary test VMs were stopped. One isolated
idle worker remains available.

After deploying this version, ordinary `pulse-macvz setup` prepares the standard
new ARM64 source. Normal provider reconciliation does so as well. Existing
accepted jobs finish on their original allocation. See
[preparation behavior](linux-kvm.md#arm64-interrupt-controller-trapping).

## Evidence and regression checks

Local evidence archive: `test-results/gic-canary-results.tar.gz`.
`provenance.json` records the source and canary identities; `code-validation/`
contains each run, ordinary-user identity, boot arguments, kernel log, and test
outputs. `code-pool-setup.log`, `code-pool-status.json`, and
`code-pool-worker-verification.log` record the normal provider lifecycle.

Validation passed:

- `go test -race ./...` and `go vet ./...`.
- Python script tests (63 tests) and shell syntax checks.
- Regression coverage for old pool migration without disrupting accepted jobs,
  missing/conflicting kernel arguments blocking handoff, GRUB setting preservation,
  idempotence, interrupted regeneration, and refusal to mutate delivered jobs.

The [Linux kernel parameter reference](https://docs.kernel.org/admin-guide/kernel-parameters.html)
documents these GIC traps. The [independent nested-virtualization report](https://twistco.github.io/foldyard/nested-virt/#the-gic-traps-are-required)
motivated the workaround; this report records our own observed results.
