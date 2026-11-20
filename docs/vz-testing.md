# VZ qualification

Build and unit checks do not qualify live virtualization. The core native,
provider lifecycle, public API/JobDB, registry import, Go endpoint, and full
paired Docker qualification checks have passed as recorded below.

## Setup and script map

VZ is available through the library and the [standalone server CLI](server-cli.md).
The CLI provides `setup` and `upgrade` for installed operation; see its documentation
for persistent settings and draining behavior. An embedding host calls `NewVZ`, then drives `Run` or `Reconcile`. Guest
setup archives and image preparation run through that same lifecycle. Production
hosts can embed `RunnerMain` in their signed executable; alternatively, build a
standalone development runner and pass its absolute path in `VZConfig.Runner`:

```sh
./scripts/build-vz-runner.sh "$PWD/.local/bin/pulse-vz"
```

This builds with cgo disabled and signs with `cmd/pulse-vz/vz.entitlements`.
`scripts/sign-macos.sh` uses `PULSE_CODESIGN_IDENTITY` when set; otherwise it
selects a single available Developer ID Application identity, then a single
Apple Development identity. Multiple identities of the preferred type require
explicit selection. With no certificate, it falls back to ad-hoc signing;
`PULSE_CODESIGN_IDENTITY=-` also explicitly selects ad-hoc signing.
The runner and server use fixed signing identifiers so an Apple-issued signature
can retain Local Network consent across builds. macOS can still ask for initial
approval when the launchd bridge first connects to a VM. Ad-hoc signing cannot
reliably retain that consent across rebuilds. See
[Apple's Local Network guidance](https://developer.apple.com/documentation/technotes/tn3179-understanding-local-network-privacy).
Local builds omit notarization timestamps; distribution builds must apply the
host application's release signing/notarization policy.
Building installs no services and changes no sandbox permissions.
Mac builds also pass a fresh Mach-O UUID to Go's internal linker (`-B`), retaining
`CGO_ENABLED=0`. Re-signing an unchanged Go executable alone does not change its
default UUID, which can incorrectly share a networking identity between ad-hoc
and certificate-signed variants. See [Apple TN3178](https://developer.apple.com/documentation/technotes/tn3178-checking-for-and-resolving-build-uuid-problems).

For a standalone runner used as a launch agent, `scripts/build-vz-app.sh` can
build an app bundle containing the fixed bundle identifier and
`NSLocalNetworkUsageDescription`. Its runner path is
`OUTPUT.app/Contents/MacOS/pulse-vz`. The VZ endpoint supervisor derives
`AssociatedBundleIdentifiers` from the runner's own app bundle, including when
the configured runner path is a symlink. An embedding application should provide
its own usage description and bundle identity. This packaging passed launchd
Local Network connectivity, process restart, and same-path rebuild checks in
`test-results/vz-verified.bnosi8f8.tar.gz`, with only the initial prompt approved.
This qualifies the tested identity/path on this host, not arbitrary installations
or bare executables. The bare executable's signature alone is not evidence that
permission attribution is correct.
Test drivers build their own helper automatically; this build step is not a
prerequisite for running them.

| Script | Purpose | Creates VMs? |
| --- | --- | --- |
| `scripts/build-server-app.sh OUTPUT.app` | Package/sign the main server, VM children, and Go bridge in one app | No |
| `scripts/build-server.sh OUTPUT` | Build/sign the standalone CLI with embedded VM runner | No |
| `scripts/build-vz-runner.sh OUTPUT` | Build/sign a development helper at an absolute path | No |
| `scripts/build-vz-app.sh OUTPUT.app` | Package/sign a runner with app identity and Local Network usage metadata | No |
| `scripts/test-vz-endpoint.sh` | Diagnose launchd bridge startup, forwarding, restart, removal | No |
| `scripts/test-vz-provider.sh LINUX_BASE [MACOS_BASE]` | Native backend lifecycle, suspend/restore, recovery; optional OCI check | Yes, disposable copies |
| `scripts/test-vz-public.sh [MACOS_BASE]` | Public API, capacity, recovery, real JobDB leases | Yes, private VZ home |
| `scripts/test-vz-docker-qualification.sh [MACOS_BASE]` | Full paired Docker allocation fault/pressure matrix | Yes, private VZ home |
| `python3 scripts/test-vz-docker-verified.py MACOS_BASE` | Bundled runner consent/rebuild gates, then the full Docker matrix | Yes, disposable Linux preflight followed by private test VMs |
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

For the current Local Network/signing fix, use the combined verification driver
from an ordinary terminal outside Safehouse:

```sh
python3 scripts/test-vz-docker-verified.py /absolute/prepared-macos-base
```

It freezes the sources, builds and registers a signed runner app, checks launchd
lifecycle, then starts a disposable Linux VM from the prepared-source cache and
requires guest SSH through the actual launchd bridge. It checks service restart,
stops the VM, rebuilds the app at the same path with a distinct UUID and matching
signing requirement, and repeats the guest-network check. Approve the initial
Pulse Provider Verification Local Network prompt if shown; during the explicitly announced rebuild
check, do not approve another prompt, because that would mask failed consent
persistence. A failure or skipped gate stops before the Docker matrix.

Only after both gates pass does it invoke the full qualification driver with
the verified app runner, leaving it at the same registered path. Cleanup stops
the preflight VM and removes the test services, retaining the single registered
verification app for the next run. The final
`Verification: passed` requires all gates and the full Docker report to pass.
The `test-results/vz-verified.*.tar.gz` archive contains the verification results,
frozen sources, stage logs, and nested Docker evidence, excluding VM disks and
compiled binaries. Set `PULSE_VZ_NETWORK_BASE` to a cold prepared VZ Linux image
directory if no Linux source exists in the qualification cache. An Apple-issued
signing identity is required for the rebuild-consent gate.

The main server verification uses one persistent app at
`.local/vz-verification/PulseProvider.app` with the test-only bundle ID
`dev.pulse.provider.qualification`. It stays registered between runs; reports
contain evidence, not additional installed copies. Production keeps
`dev.pulse.provider`. Apple documents unexpected privacy behavior with multiple
copies of an app ([TN3179](https://developer.apple.com/documentation/technotes/tn3179-understanding-local-network-privacy)). The driver records LaunchServices resolution and
rejects competing registered copies of its test identity. Its rebuild gate also
rejects any bridge forwarding failures, even if subsequent retries succeed
after a second approval. On failure it automatically captures relevant identity
logs. Once the failed test ends, a lingering Local Network prompt for the
verification app can be allowed; this does not turn the saved failure into a pass.

`vz-verified.me0hx329` passed initial connectivity/restarts but failed after the
main app rebuild, before the Docker suite. The two builds had matching signing
requirements and distinct UUIDs. The old packaging rewrote a registered app
in place, briefly invalidating its signature, and test builds reused the
production identity across separate app copies. The packaging now signs a
staged bundle and installs it with an atomic directory swap; verification uses
the isolated persistent installation described above. These fixes address
observed packaging/test defects. The subsequent `vz-verified.qw_kgw2e` run passed
initial and rebuilt network gates, including the strict forwarding-log check.
The earlier archive alone does not establish which defect caused its second
prompt.

`vz-verified._lkfct1m` then verified a single registered test app and passed
loopback forwarding and service restart, but failed during repeated endpoint
cleanup: `print` still found the service while `bootout` returned exit 3,
“No such process.” No VM or consent check had started. Endpoint removal now
waits for confirmed service absence after bootout, including this race, before
releasing its files or reusing its label. Permission errors and unconfirmed
removal still fail and retain recovery state. Regression tests cover delayed
removal, repeated removal, timeout, and denied observation; the live pass marker
is emitted only after deferred cleanup succeeds.


The underlying Docker-only driver remains available:

```sh
./scripts/test-vz-docker-qualification.sh /absolute/prepared-macos-base
```

`PULSE_VZ_RUNNER` may supply an already built and signed executable; in that case
the driver verifies and records its signature and hash rather than rebuilding it.

The optional base is copied read-only and prepared with the Docker integration
profile. Without one, preparation uses the registry. The Linux companion comes
from a reusable Docker base prepared from the configured pinned registry image.
This is the same production allocator
suite used for the other providers: concurrent JobDB jobs, controller/relay/
endpoint/daemon process loss, CPU/memory and private-disk pressure, prune
isolation, primary/engine VM loss, fresh identities, and cleanup. The controller
subprocess reopens the VZ provider with the same home and runner.

By default, the driver keeps prepared cold bases and suspended slot templates
in `.local/vz-qualification-cache`. The first run prepares them; subsequent runs
clone their raw VM files into a new private test home. Ownership records, job
state, allocation disks, and allocation credentials are never cached. Sources are cached
as soon as both pairs are ready, so a later fault-test failure does not discard
the preparation work. A cache lock prevents concurrent runs from restoring the
same template identities.

The cache key covers guest preparation and native VZ inputs from the frozen
source manifest, pool configuration, host macOS build, c2j selector, and supplied
base path/file sizes/modification times. Changes to those inputs start a new
entry; ordinary test-reporting changes do not. Set
`PULSE_VZ_TEST_CACHE=/absolute/cache` to choose another location,
or `PULSE_VZ_TEST_CACHE=` to exercise completely cold preparation. Remove unused
cache entries when no test is running to reclaim disk space. Normal cleanup
still removes every test allocation and the private copies of prepared sources.

The `two production Docker allocations` timer includes pool preparation on a
cache miss. On a hit it measures fresh pair preparation from cached sources;
job handoff is a later stage. In the isolated VZ check
`test-results/vz-engine-base2`, software-base preparation took 29 seconds and
two successive Linux companions each reached readiness in about 7 seconds.
Those numbers exclude macOS restore, bridge startup, and job handoff.
Cached allocation readiness has a five-minute deadline, versus 35 minutes when
sources must be prepared. Waiting logs include pair phases and save
`allocation-wait.json` and `endpoint-wait.json` in the output directory.

The VM-free endpoint preflight checks launchd lifecycle and loopback forwarding;
it cannot establish permission to reach a guest. During allocation, VZ now also
requires the Linux engine's SSH banner through the launchd bridge before it
restores macOS. A listening host port alone is insufficient: Local Network
privacy can allow incoming connections while denying the bridge's outgoing
connection. Forwarding errors are retained in the endpoint's `service.log`.

Allow 30–60 minutes plus downloads, at least 40 GiB free, and capacity for two
macOS and two Linux guests (16 vCPUs / 32 GiB). The default worker address is
`192.168.64.1`; override `PULSE_TEST_ENGINE_WORKER_ADDRESS` if the guests reach
this host through a different address. The test uses ports 45200–45201.

The driver checks the launchd endpoint **before creating any VMs**. It requires
launchd job creation and the virtualization/network permissions below. With the
current Safehouse policy, run it from an ordinary terminal. A preflight failure
does not qualify Docker: `report.json` must say `passed`, with evidence for every
required stage. The agent attempt in `test-results/vz-docker.RpflDb` stopped at
this preflight (`bootstrap` exit 5).

The outside-terminal run in `test-results/vz-docker.n5ksdl` passed the endpoint
preflight but failed while preparing its first Linux Docker companion: no engine
ready marker arrived before the 15-minute allocation deadline. Its diagnostics
did not include delivery status or the preparation service journal, so the cause
inside the guest remains unknown. Those diagnostics and allocation phases are
now captured on failure, together with partial qualification progress.

An isolated reproduction using that run's cached Ubuntu image and the unchanged
production startup script passed in 33 seconds (`test-results/vz-engine-repro`).
This does not qualify the complete paired Docker matrix. To run that narrower
preparation check without launchd or macOS guests:

```sh
mkdir -p test-results/vz-engine
bash scripts/build-vz-runner.sh "$PWD/test-results/vz-engine/pulse-vz"
PULSE_VZ_LINUX_BASE=/absolute/stopped-linux-base \
PULSE_VZ_RUNNER="$PWD/test-results/vz-engine/pulse-vz" \
PULSE_VZ_RESULTS="$PWD/test-results/vz-engine" CGO_ENABLED=0 \
  go test ./internal/provider -run '^TestLiveVZDockerEngine$' -v -count=1 -timeout=7m
```

It creates a private APFS clone, runs the real Docker preparation script, records
delivery/service diagnostics, and stops the disposable VM before returning.
Set `PULSE_VZ_ENGINE_PRESSURE=1` to also check Docker daemon restart, CPU/memory
pressure, container OOM, private-disk ENOSPC, and subsequent container execution.
Use an unallocated Linux base, not a disk from a previously used Docker companion.
This narrow check does not exercise the macOS relay or peer isolation.

### Nested Linux KVM

On a host supporting nested virtualization, use a stopped **prepared Linux worker
base** (with `/opt/pulse-tart/bin/c2j`), not a Docker-engine-only companion base:

```sh
mkdir -p "$HOME/.pulse/vz/diagnostics/kvm"
PULSE_TEST_VZ_KVM=1 \
PULSE_VZ_LINUX_BASE=/absolute/prepared-linux-worker-base \
PULSE_VZ_RUNNER="$HOME/go/bin/pulse-tart" \
PULSE_VZ_RESULTS="$HOME/.pulse/vz/diagnostics/kvm" CGO_ENABLED=0 \
  go test ./internal/vz -run '^TestLiveLinuxLifecycle$' -v -count=1 -timeout=12m
```

The test uses a private clone, runs the production worker-permission setup, and
creates a KVM VM as `pulseworker`. It installs QEMU and a minimal initramfs inside
that disposable clone, then boots its installed ARM kernel using `-accel kvm` as
the same unprivileged account. A successful API call alone is not a pass: the
nested kernel must execute init and shut down. Software emulation is forbidden.
The outer VM is stopped on success or failure. Allow downloads for test packages;
QEMU is not added to production base images by this test.

The next full run, `test-results/vz-docker.RcRbUS`, passed concurrent jobs and
controller, guest-relay, host-endpoint, and Docker-daemon kill/recovery checks,
with two macOS guests observed. It failed in the CPU/memory pressure script;
the peer remained responsive through seven probes. The management SSH API
discarded the failed command's output, so the exact assertion is unknown.
Qualification now captures combined output and an explicit exit status, and the
pressure script reports each measurement before asserting it. The same pressure
script passed on an isolated VZ Linux engine in `test-results/vz-pressure-repro`.
The extended isolated test in `test-results/vz-pressure-fresh` also passed after
Docker daemon kill/restart, including CPU/memory pressure, OOM, ENOSPC, and
subsequent container execution (74 seconds including engine preparation).
That run did not qualify the complete matrix; the later passing run is recorded below.

The later run, `test-results/vz-docker.OC6W7b`, completed cold preparation
(1m42s for the fixture base, then 4m42s to two ready pairs) but failed before
jobs started: cache publication tried to copy slot templates twice because
both `Images()` and `Slots()` list them. Publication now deduplicates that
inventory, with a regression test. Early failure reports also accept an empty
identity list instead of losing the stage report to a Python exception.

The preparation-only gate `TestLiveVZDockerPreparationCache` subsequently
published four sources in `test-results/vz-cache-prepare.ue8gT0`. It uses real
base/template reconciliation but disables host endpoint creation, so it does
not qualify paired allocations. A fresh private home then restored all four
sources without rebuilding in `test-results/vz-cache-reuse.deAehh`; the gate took
0.19 seconds. This measures source reuse, not VM startup or job handoff.
The opt-in gate requires `PULSE_TEST_VZ_DOCKER_PREPARE=1`, a signed VZ runner,
`PULSE_VZ_TEST_CACHE`, and a frozen source manifest in `PULSE_TEST_OUTPUT_DIR`,
using the same configuration as the full qualification driver.

`test-results/vz-docker.Wh1yKV` hit the cache but could not configure the first
pair. The Linux startup script completed in 6.7 seconds and macOS restored and
received its startup script within 20 seconds. The host launchd bridge accepted
connections but could not connect to Linux. A separate bridge in
`test-results/vz-bridge-check.tocp_jev` reported `no route to host` and prompted
for Local Network access; after user approval, the same diagnostic build passed
in `test-results/vz-bridge-check.6k8_psj1`. This isolates that run's block to host
Local Network permission. The original allocation eventually timed out without
starting jobs. The signing and forwarded-SSH readiness fixes described above
address this failure; the later passing fault-matrix run is recorded below.
Host-only bridge code is now excluded from the guest preparation cache key.
Existing sources were checked against identical remaining preparation inputs
and reused successfully in `test-results/vz-cache-signing.yI4CGz` (0.23 seconds).

Later verification disproved the claim that signing alone had fixed consent:
`test-results/vz-signed-bridge.3rtap3ij` still received `no route to host` after
approval; `test-results/vz-consent-restart.pa77c0w0` reproduced it in three fresh
processes, while each direct guest connection succeeded. No network recovery
was observed. The old ad-hoc and newly certificate-signed binaries had different
signing identifiers but the same Mach-O UUID. They also lacked usage metadata
and an associated app bundle. Those are concrete identity defects documented
by Apple; the evidence does not establish which caused the retained denial.
`test-results/vz-identity-audit.wbzlNQ/identity-audit.json` records the collision
and verifies distinct UUIDs, matching certificate requirements, cgo-free builds,
and the new bundle metadata. It explicitly records live networking as unverified.
The old workstation verification script is disabled to prevent further tests
with that invalid comparison.

The actual pinned Ubuntu import also passed in `test-results/vz-oci.s0wuxQ`:
manifest/layer verification, Apple LZ4 decoding, cache publication and clone,
followed by the Linux lifecycle test against the imported image (5.75 seconds).
To include a fresh registry import in `scripts/test-vz-provider.sh`, set
`PULSE_VZ_OCI_IMAGE` to a digest-pinned Linux reference. This opt-in gate also
clones from the published cache a second time before booting the imported image.

### Latest bundled verification result

`test-results/vz-verified.qw_kgw2e.tar.gz` passed with the packaged main server
executable, atomic app replacement, isolated persistent verification identity,
parallel Docker preparation, and confirmed endpoint removal. Both initial and
rebuilt launchd-to-guest connectivity/restart gates passed; the rebuilt bridge
log contained no forwarding failures. LaunchServices resolved exactly the
verification app. All twelve Docker qualification checks passed, four real
JobDB jobs completed, and the peak running macOS count remained two.

The four prepared bases/templates were cache hits. Two complete Docker
allocations became ready in **20, 23, and 38 seconds** across the three
generations, compared with **51, 49, and 49 seconds** in the earlier serialized
run below. These are single-host observations, not latency guarantees. The full
fault/pressure suite took **681.48 seconds (11m21s)**. Its report is
`test-results/vz-verified.qw_kgw2e/source/test-results/vz-docker.n32keb/report.json`.
The tested source manifest matches the implementation committed with this
record. Go race tests, vet/build, cgo-free checks, and the Linux cross-build also
passed after the endpoint-removal fix.

### Earlier serialized allocation result

`test-results/vz-verified.weziv5_q.tar.gz` passed both network gates and all
twelve Docker qualification checks. Four real JobDB jobs completed; six fresh
allocation identities across three generations were distinct, and at most two
macOS guests ran concurrently. The Docker suite took 726.53 seconds (12m07s).
This qualifies the tested single-host VZ configuration, including bounded
pressure and injected process/VM loss; physical host power loss was not tested.

Timing observations on this host:

| Scenario | Observed time / interpretation |
| --- | --- |
| Restore four prepared source filesets into a private home | 0.19 seconds in the earlier cache-only gate; excludes VM startup |
| Start a Linux Docker companion from its prepared software base | About 7 seconds in the isolated engine test |
| Build two complete macOS + Linux Docker allocations from prepared sources | 51, 49, and 49 seconds across this run's three generations |
| Build one such pair | Roughly 25 seconds is an estimate from serialized pair preparation, not a separately timed measurement |
| Start work on two already-ready allocations | About 17–18 seconds for this test's entire startup phase, including fixture setup, job handoff, and initial workload/image setup; not a pure submission benchmark |
| Recover controller ownership while VMs continue running | 2 seconds in the controller SIGKILL test |
| Reclaim a companion after primary VM loss | 8 seconds |
| Job exit and paired cleanup after engine VM loss | 100 seconds, including this fixture's failed-probe tolerance and exit behavior |
| Cold preparation with the supplied macOS source already local | Earlier run: 1m42s fixture-base setup + 4m42s pool preparation; downloads can add substantially more |

The implementation measured in that archive prepared Docker pairs one at a time,
starting the Linux engine and endpoint before restoring/configuring the macOS
primary. The current implementation overlaps independent pairs and restores
macOS while its engine starts; endpoint readiness still gates guest configuration.
The revised path is qualified in the latest result above. The seven-second
engine figure therefore does not describe complete Docker allocation readiness.
Keeping ready pairs available moves this preparation off the job admission path.
Each used pair is replaced with fresh allocation state from reusable sources.
The full suite intentionally waits for overlapping leases, executes fault and
pressure checks, and includes a 65-second fixture delay for each completed job;
its runtime is not a startup-latency benchmark.

Standalone macOS measurements with the packaged main server executable, 4 vCPU
and 8 GiB, are recorded in
`test-results/macos-timing/vz-mac-timing-4200333887/timings.json`:

| Scenario | Time |
| --- | --- |
| Cold clone/start through a guest command over SSH | 14.85 seconds |
| Clone/restore through a guest command over SSH, three runs | 8.53, 8.50, 8.49 seconds |
| Save a suspended snapshot | 2.02 seconds |

These measurements include clone/dispatch and SSH readiness, but exclude c2j
lease handoff and job execution. Reproduce with a stopped cold macOS base:

```sh
PULSE_TEST_VZ_TIMING=1 \
PULSE_VZ_MACOS_BASE=/absolute/cold-macos-base \
PULSE_VZ_RUNNER="$HOME/Applications/PulseProvider.app/Contents/MacOS/pulse-tart" \
PULSE_VZ_RESULTS="$PWD/test-results/macos-timing" CGO_ENABLED=0 \
  go test ./internal/vz -run '^TestLiveMacOSTiming$' -v -count=1 -timeout 10m
```

The preceding run,
`test-results/vz-verified.eebzmuzb.tar.gz` passed both network gates, used all
four cached sources, and obtained both allocations in 49 seconds. Both real
jobs started and overlapped with lease renewals. Controller, guest relay, host
endpoint, and Docker daemon SIGKILL recovery all passed. The run stopped after
140 seconds at the OOM assertion: CPU throttling and 4 GiB memory pressure were
observed, with seven successful peer probes, but Docker reported exit 137 and
`OOMKilled=false`.

Direct engine reproduction in `test-results/vz-oom-repro.05tf9gxn` found the
same mismatch in three of twelve containers, before and after daemon restart.
The kernel recorded a `CONSTRAINT_MEMCG` kill for each exact container ID;
Docker's flag remained false after a delay. This is an observed reporting defect
in the tested Docker 29.1.3/containerd 2.2.1 stack, not evidence that the memory
limit failed. The precise upstream cause has not been established.

The pressure gate now requires the configured 64 MiB limit, exit 137, and a
kernel record naming that container's own limiting cgroup. Exit 137 or Docker's
flag alone cannot pass; missing evidence, a different container, and global or
ancestor OOM kills fail. `container-oom.json` retains the kernel record and
Docker's reported flag separately. This qualifies memory isolation; it does
not qualify Docker's OOM reporting as reliable. The prepared-source cache is
unchanged. The subsequent full-suite pass is recorded above.
The revised CPU/memory/OOM gate and the subsequent private-disk ENOSPC and
container-recovery checks passed directly in the disposable engine; their logs
and cleanup result are retained in the same `vz-oom-repro.05tf9gxn` directory.

`test-results/vz-verified.bnosi8f8.tar.gz` passed both launchd-to-guest network
gates with only the initial permission prompt, including process restarts and a
same-path rebuild with a new UUID and matching signing requirement. Four cached
sources were reused and both Docker allocations were ready in 50 seconds. Both
workers then exited with status 1 during command execution, and the test detected
the resulting normal guest shutdown after 73 seconds overall. Full Docker
qualification was incomplete at that point.

Investigation found that the diagnostic wrapper's `umask 077` leaked into test
setup and created a root-only job-script directory. A regression test reproduced
the changed modes, and a disposable macOS guest reproduced denied execution as
`pulseworker` before the fix and successful execution after it
(`test-results/vz-job-permissions.t8mea4wq/probe.log`). The wrapper now scopes its
umask to its temporary log; setup explicitly creates the directory with mode
0755 and checks script access and Docker connectivity as the worker before
submission. Job-script output and its failing line are also captured. These
changes preserve the prepared-source cache; the subsequent full-suite pass is
recorded above.

The preceding run,
`test-results/vz-verified.o2001tyt.tar.gz` passed both launchd-to-guest
connectivity gates, including service restarts. An additional prompt was approved
during the run, so this does not establish consent persistence across rebuilds.
The Docker suite hit all four prepared sources and obtained both allocations in
49 seconds. Both leases were accepted, but neither job reached its readiness
marker; both macOS primaries had stopped before diagnostics, while their Linux
engines remained running. Host bridge logs contained no forwarding errors.
The third prompt's identity is unknown, so its dismissal cannot be assigned as
the cause. Full Docker qualification was incomplete at that point.

The qualification test now attaches a log observer before job submission to
capture worker/launcher failures before the guest's normal shutdown, and fails
early if a primary stops before job readiness. Captures are redacted into
`early-job-<pair ID>.json`. This diagnostic change does not invalidate prepared
sources or change production shutdown behavior.

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
suite subsequently passed in `test-results/vz-verified.weziv5_q.tar.gz`; its
results and measured timings are recorded above. Shared orchestration tests and
the Go endpoint's launchd lifecycle also passed.
