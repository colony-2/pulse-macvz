# Diagnose an existing Tart worker

Run from this checkout on the Apple Silicon worker, as the same account and with
the same `TART_HOME` as Pulse. Leave Pulse running:

```sh
./scripts/diagnose-tart.sh
```

Python 3 and Tart are required. The collector takes three snapshots with 30 seconds
between them, then checks updater discovery and any running unfinished macOS
builders. Commands have a 15-second timeout; a busy worker may take longer than a
minute overall. Each invocation creates a private
`test-results/<UTC timestamp>-tart-diagnosis-<pid>/` directory and a `.tar.gz`
archive. Send the archive printed on the final `Send:` line, including on failure.

This is an observation tool for preparation stalls, unavailable capacity, restore
failures, recovery/reservation problems, and image updater discovery failures. It
does not start a provider, reconcile, run jobs, rebuild images, stop VMs, change
configuration, or repair anything. It can run alongside Pulse. SSH probes only
inspect already-running, unfinished macOS source builders identified by their VM
properties. Add `--jobs` to inspect running macOS job clones as well. Neither mode
resumes suspended sources.

## What the report contains

- `environment.json`: OS, Tart and checkout versions, resources, free disk space,
  selected Tart home and collector fingerprint.
- `snapshot-*.json`: all local VMs (including foreign ones), native state/hardware,
  selected slot/image/job/pair metadata, file timestamps, readiness identities,
  known error signals, VZ error codes, and candidate capacity reservations by name.
- `pulse-builds.json`: build information from visible Pulse executables, including
  their `pulse-tart` dependency when `go version -m` is available. The executable
  path is resolved from the process's mapped files with `lsof`, even when `ps`
  reports only `pulse`.
- Each snapshot's `pulse_status`: selected `/status` fields from discovered
  loopback/wildcard TCP listeners owned by Pulse, including version and controller
  poll counters. Collection uses read-only HTTP requests through `curl`.
- `updater-discovery.json`: the read-only updater `--list` result. Discovery failure
  is evidence, not a reason to discard the report.
- `guest-builders.json`: read-only SSH observations of unfinished macOS builders:
  actual OS/boot identity, readiness/failure files, tool presence, base/template
  service state and exit code, and recognized preparation-log errors. Unreachable
  guests are recorded.
- With `--jobs`, each snapshot also has `guest_jobs` and `job_observations`:
  launch/job/slot IDs, restore identity checks, startup/handoff/worker markers,
  guest service state, worker process presence, completion exit code, log size and
  modification time, and recognized lease/network errors.
- `commands.json`: attempted commands, exit codes/timeouts and recognized stdout/stderr
  signals. `summary.json` lists findings and VMs that changed between snapshots.

The collector reads private VM properties locally but exports an explicit field
allowlist. Credentials, host keys, signing keys, leases, startup scripts, environment
variables, process arguments and raw logs are excluded. Machine identity is hashed
for comparison. Log extraction retains recognized signals and valid readiness
identities, not arbitrary text; unfamiliar errors may need targeted follow-up.
Guest SSH uses the existing VM credentials and pinned host key when available,
with temporary askpass/known-host files outside the report.

## Optional inputs

Pulse stdout/stderr files are discovered through `lsof` when possible. If Pulse
logs to a terminal, a logging service, or a file the collector cannot discover,
supply its JSON log explicitly. Only selected diagnostic records are extracted:

```sh
./scripts/diagnose-tart.sh --pulse-log /path/to/pulse.jsonl
```

Other useful options:

```sh
# A longer observation window, five snapshots one minute apart.
./scripts/diagnose-tart.sh --samples 5 --interval 60

# A quick host-only snapshot.
./scripts/diagnose-tart.sh --samples 1 --skip-guest

# Non-default Tart home, or an older provider's journal/native artifacts.
./scripts/diagnose-tart.sh --tart-home /path/to/tart --state-dir /path/to/StateDir
```

`--state-dir` and `--pulse-log` are repeatable. Legacy state is explicitly selected
so an old copied journal is not mistaken for current state. Its sanitized records
are stored separately and are not included in the current capacity estimate.
Use `--command-timeout SECONDS` for slower hosts or `--output-root DIRECTORY` to
change the results destination. Paths passed as options should be absolute.

## Accepted launches that retry or occupy every slot

`accepted` establishes admission, not successful guest execution. If later launches
report both `standard-0001` and `standard-0002` as `reservation occupied`, those
slots are reserved by earlier launches. `template identity/suspension unknown` in
that branch is not itself a failed template check. The launch's one-minute
cooldown explains admission retries; it does not explain why existing reservations
remain occupied.

Leave Pulse and the VMs running and collect repeated observations:

```sh
./scripts/diagnose-tart.sh --jobs --samples 13 --interval 10
```

This takes at least two minutes, plus command/SSH time. Send the archive printed
at the end. Add `--pulse-log /absolute/path/to/pulse.jsonl` if Pulse writes to a
file the collector cannot discover. Terminal output cannot be recovered by this
collector.

The snapshots distinguish a restore failure, no observed startup delivery,
successful handoff, worker intent/process presence, and worker exit. They correlate
reservations with launch and job IDs, including jobs disappearing during collection.
Guest probes only inspect existing state: they do not read the launch payload,
restart the guest service, replay a lease, or run c2j. Log error categories are
recognized text matches, not an authoritative diagnosis of lease validity.

A running c2j process does not prove lease renewal or job progress; a worker exit
code does not independently establish JobDB completion. Missing markers can mean
delivery has not happened or evidence was unavailable. Check command/SSH outcomes.
OpenSSH reachability also does not prove the provider's separate Go SSH delivery
worker succeeded. Older provider builds may retry without retaining the error in VM events. This checkout records sanitized delivery stages and causes in VM properties; compare the running binary with the checkout.
Do not rerun the stalled-template repair or clear reservations based on occupancy
alone; first determine which launches own them and where those launches stopped.

When a running job has no handoff and OpenSSH succeeds, use the transport probe:

```sh
./scripts/diagnose-tart.sh --delivery --samples 1
```

This implies `--jobs` and requires Go. It builds a temporary helper from this
checkout, then checks directory/ownership/hardware binding and startup-script
presence before probing each running job. `delivery_probes` records local check
results and the Go SSH stage/outcome: TCP, handshake/authentication/host key,
session, and shell/sudo. The helper uses the same `golang.org/x/crypto/ssh`
dependency and password/key settings as the provider. It sends only a fixed
marker command and `sudo -n /usr/bin/true`; it never sends the stored startup
script or lease and does not create VM event files. Credentials travel through
stdin, not arguments or archived files. Errors use fixed categories; raw peer
output is discarded. Build time is bounded at 120 seconds, each probe at 35.
`--skip-guest` also disables this probe.

The helper is a separate process: success does not establish that the running
Pulse binary has the same process permissions, configuration, or live delivery
worker state. Compare `pulse-builds.json` with the checkout. If all probe stages
pass while Pulse still has no delivery events, the next step is instrumentation
of Pulse's actual delivery worker, not rebuilding the templates.

### November 18 execution-stall report

The report `20261118T225253Z-tart-diagnosis-69487` contains 13 snapshots from
22:52:54 to 22:55:12 UTC. Both macOS 26 job clones are running with
`native_delivered=false` and no host `pulse.events`. All 26 OpenSSH job probes
succeed, but neither guest has a launch payload, fingerprint, intent, PID,
completion file, worker log, or c2j process. Both restored MAC/boot identities
match the saved template identities. All four source templates are recorded ready
and suspended. Pulse's controller passes increase from 922 to 950.

Slot `standard-0001` holds launch `4de308878ce22f24af48853ba7cfa895`, created
21:36:08 UTC for job `3MIE8Sz8SdXHxbJVxhrOyBD5Q3Z`. Slot `standard-0002` holds
launch `4056e69ebf766777546d8dc9e7383353`, created 21:41:14 UTC for a different
job, `3MIVSn38VBHnu56ASMvryosjSIi`. This establishes a stall before guest handoff;
the report does not identify the underlying delivery error. It contains no Pulse
log file and cannot establish live delivery-goroutine progress. The collector
also omitted `guest_service=not_loaded` because its value parser rejected the
underscore; that parser is corrected. Absent service fields in this report
therefore provide no service-state evidence.

The follow-up report `20261118T230711Z-tart-diagnosis-87975` narrows this further:
both Go probes fail at `stage=tcp` with `cause=network_unreachable`, while both
OpenSSH probes succeed. Directory identity, owner marker, hardware, and script
presence checks all pass. Neither guest service is loaded. Thus the reproduced
failure precedes SSH authentication and guest script execution. It is not evidence
that the templates need rebuilding.

The operator runs Pulse and the collector inside tmux in iTerm2. A leading
hypothesis is macOS Local Network privacy/process attribution; a per-process
network filter or different OpenSSH routing remains possible. The archive does
not contain an authoritative OS permission decision.

For a controlled comparison, keep the VMs and existing Pulse running, open Apple's
Terminal app, and run the same `--delivery --samples 1` command from a plain shell
outside tmux, as the same user and with the same `TART_HOME`. Apple documents a
local-network exception for tools launched from Terminal in
[TN3179](https://developer.apple.com/documentation/technotes/tn3179-understanding-local-network-privacy).
If both Go probes succeed there, stop only the existing Pulse with Ctrl-C and
launch Pulse from that working shell with the same settings. This lets normal
delivery/reconciliation resume; old leases may have expired, so verify fresh
handoff/worker progress rather than treating another `accepted` as completion.

For iTerm2, check System Settings > Privacy & Security > Local Network and enable
iTerm2 if listed, as described in
[Apple's Local Network settings guide](https://support.apple.com/en-lk/guide/mac-help/mchla4f49138/mac).
Retest the existing tmux session separately; attaching from a different terminal
does not create a new tmux server. Do not kill all tmux sessions, reset privacy
databases, run Pulse as root, or alter the VM templates as part of this comparison.

The comparison report `20261118T230937Z-tart-diagnosis-91676` passes both Go probes
through `shell_and_sudo`, with the same VM names, identities, and successful local
checks. The prior run failed at TCP. This isolates a launch-context-dependent
connectivity problem; it does not by itself identify the exact OS permission or
filter responsible. The original Pulse PID 71730 is still running with start time
21:36:07 UTC, and both job records still show no delivery. Restart that Pulse
process from the shell where the probe succeeds, preserving user, configuration,
and Tart home, then collect `--jobs --samples 13 --interval 10` there to verify
handoff, worker execution/exit, and reservation recovery. Probe success alone is
not a successful job execution result.

The recovery report `20261118T231138Z-tart-diagnosis-94695` follows a new Pulse
start at 23:10:34 UTC. Both old job reservations are absent. A new launch,
`eef854d5a7e1f0ed3c0c3d1821e57ea6`, has a launch payload, fingerprint, worker
intent, live PID/c2j process, active guest service, and worker log. Only one macOS
job reservation remains, and the four Linux pairs are ready. This demonstrates
recovery through worker startup, not final JobDB completion. `native_delivered`
can remain false during active work because that older delivery function waits
for the startup SSH command to return before updating that field.

### Preventing silent delivery failures

Do not reject execution merely because `TMUX` is set. The observed failure is a
network-access failure in a particular process context. Tmux presence is useful
diagnostic context, not a reliable capability test.

This checkout now persists delivery attempts, stages, sanitized causes, and a fence before guest execution. `TartConfig.DeliveryTimeout` defaults to two minutes and bounds pre-exec work. Readiness markers update delivery state before SSH exits. Recovery stops expired, undispatched owned VMs, then normal reconciliation releases their resources. An uncertain exec is never replayed or stopped by this deadline; legacy undelivered records without a fence are also treated as uncertain and require investigation.

`Runtime.DeliveryHealth` and `DeliveryObservations` expose this state. Persistent failures block new admissions; reconciliation continues even when recovery or health reports an error. A nil health result without a successful delivery is not a connectivity guarantee. The Pulse build helper applies the accompanying maintenance-status patch so controller progress can be distinguished from provider degradation.

Further operational guidance (the collector does not change host settings):

- Probe a real reachable guest from Pulse's own process when one is available;
  a successful external OpenSSH command is not sufficient. If there is no running
  guest at startup, report readiness as unverified until actual connectivity can
  be checked, rather than inventing a healthy or failed result.
- Preserve delivery stage, underlying error/errno, attempt count, and last success
  in diagnostics. Surface persistent pre-handoff failure as Tart unavailable and
  in overall health, with a macOS Local Network/process-context hint where
  appropriate. Do not classify every unreachable-host error as permission denial.
- Bound failed delivery and recover owned clone/reservation resources using the
  provider's existing lifecycle guarantees. A launch that was already accepted
  requires reconciliation, not simply a process exit that leaves it occupying a
  slot. Healthy providers should remain available.
- For an explicit startup requirement that Tart be ready, fail after a bounded
  readiness check with an actionable error. A runtime failure should normally
  degrade Tart and retry with backoff rather than terminate unrelated providers.

Apple documents no universal permission-query API. For a specific TCP connection,
Network.framework can identify `localNetworkDenied`; plain Go socket errors do
not provide that distinction. Apple also documents signed program identity,
responsible-app attribution, and narrow subnet exemptions for managed CI hosts in
[TN3179](https://developer.apple.com/documentation/technotes/tn3179-understanding-local-network-privacy).
Subnet exemptions affect all processes reaching that subnet and require a restart.
A packaged service/launcher should establish a stable permission identity; merely
switching to a LaunchAgent does not confer the LaunchDaemon exemption. None of
these host settings are changed by the diagnosis or repair scripts.

## Interpretation and limits

### Startup identity mismatch after a host reboot

The old native binding persisted `st_dev` as part of VM identity. macOS can
renumber devices across reboots, so an unchanged owner marker and directory inode
could still fail at `write VM properties` before reconciliation started.

The provider now records `native.volume_uuid` using the filesystem's persistent
volume UUID. It checks that UUID, the directory inode, and `.pulse-owner`; `device`
is retained as a current observation for diagnostics. A different saved UUID is
always rejected, even if the device number happens to match.

Existing records gain a UUID during normal property writes. If an old record's
device number has already changed, migration requires the original owner marker
and inode plus a successful Tart inspection matching the saved CPU, memory, and
nonempty MAC. It only updates the binding metadata. It does not launch a VM,
replay a job, change its reservation, or rebuild a template. Older records have
no prior volume UUID to compare: this one-time migration relies on those retained
identity checks. Partial clones without a saved MAC require separate investigation.

After updating this checkout on the Mac, validate the native UUID reader and
build Pulse with the local provider:

```sh
go test ./internal/tart -run 'TestNativeVolumeUUID|TestBinding'
./scripts/build-pulse-with-local-tart.sh
./dist/pulse
```

Stop any existing Pulse first and use the same user, configuration arguments,
and `TART_HOME` when starting `./dist/pulse`. The startup property write performs
the migration; normal reconciliation then handles jobs interrupted by the reboot.
Building this library alone does not update an already-installed Pulse binary.

The upgrade report `20261119T030752Z-tart-diagnosis-32868` shows the macOS 15 base
present and stopped with a new MAC/machine identity, and newly adopted metadata
created at 23:35:19 UTC. All four slot templates retain their prior identities and
are suspended/ready. There are no macOS job reservations and no retained staging
VMs. Thus the failed first publication did change the base despite the old updater
printing zero publications; the report alone cannot verify its installed c2j
version or disk equality with the now-deleted builder. The updater now compares
complete VM files against the prepared builder before accepting any native
publication error as success. An ordinary update can rebuild and publish the
whole selected pool again; `--ensure` would skip the old slots and is not an
upgrade recovery mechanism.

Exit zero means collection completed, **not** that the pool is healthy. Nonzero
means collection was incomplete; the archive still contains available evidence.
An empty pool, missing log, inaccessible guest or updater discovery failure is
reported without inventing a health result. See individual command outcomes for
optional tools that are unavailable.

Capacity is a best-effort snapshot across local owners. It lists running/unknown
VMs and attached reservation candidates, distinguishing Linux from macOS/unknown.
The `with_one_more_macos` count includes the hypothetical new VM; it is not a
running-VM count. Observations race with normal activity and do not reproduce the
live provider's locked admission decision. Missing in-memory intents, unreadable
metadata and legacy state can limit this estimate.

Unchanged snapshots or a visible Pulse process do not prove whether reconciliation
is running. Increasing controller poll counters establish controller progress,
not progress of the separate Tart delivery worker. A ready source adopted from the updater can legitimately have no host
`PULSE_TEMPLATE_READY` event; do not diagnose it as broken from that alone.

Pulse's implementation reviewed at commit
[`f9387b773994a5a4a151333c665b134660efd2b5`](https://github.com/colony-2/pulse/commit/f9387b773994a5a4a151333c665b134660efd2b5)
calls provider `Maintain` from the controller. Tart's `macProvider.Maintain` starts
a background loop that calls `runtime.Reconcile`, records the error, then waits
two seconds before another pass. It retries after errors and lives until provider
close. That is separate from the library's public `Runtime.Run`, which returns on
an error. `pulse check` does not start warming. Compare the collected running binary
revision with the source being investigated; process evidence alone cannot verify
the live goroutine's state.

The no-option updater now discovers incomplete pools as well as prepared sources. It can fill standard two-slot pools and repair verified unfinished builders with Pulse stopped; see [image preparation](prepared-images.md). The diagnostic collector only invokes `--list` and never performs that repair.

## One-off repair for the November 18 stalled pool

The two reports ending in `37401` and `43145` identified the macOS 26 and macOS 15
first-slot builders in owner token `id-565a157910ca985c18d3`. Both were reachable,
unused, and still waiting for template-script delivery. For that exact pool:

1. Stop Pulse normally, using Ctrl-C in its terminal. This leaves its VMs intact.
2. As the same user with the same `TART_HOME`, run:

   ```sh
   ./scripts/repair-stalled-macos-pool.sh
   ```

3. After success, restart Pulse with your usual command, then run
   `./scripts/diagnose-tart.sh` and send both archives.

The repair holds the provider's pool lock, verifies the original builder UIDs,
directory ownership, hardware, and absence of job/pair reservations. It runs the
installed template preparer over OpenSSH, verifies its real boot UUID/MAC marker,
records that marker in the host events file, and suspends each builder. It never
edits `pulse.json` or assigns readiness phases. Pulse performs the normal readiness
transition after restart.

Once both builders are suspended, the script calls the existing updater with
explicit names and `--ensure` to create the missing second slot for each OS from
the retained prepared base. It does not rebuild bases or download/install a new
c2j release. New builders get independent MAC and machine identities through the
existing updater. The target is four suspended templates sharing the same physical
two-macOS execution limit.

Each run produces `test-results/<timestamp>-tart-repair-<pid>/` and a `.tar.gz`,
including failures. Guest failures stop repair without deleting existing VMs or
inventing readiness. Completed suspensions can be reused on a rerun. `--check`
performs preflight only (Pulse must still release its lock); `--timeout SECONDS`
changes the default 900-second guest preparation limit. Unknown/replaced builders,
failed or stopped builders, active job reservations, and concurrent updaters require
investigation rather than automatic deletion. This is a narrowly scoped recovery
tool, not the general-purpose updater repair implementation.

### Startup fails with `legacy properties key mismatch`

Creating the missing second-slot names can expose a leftover legacy journal from
an earlier pool generation. On startup, the current provider may import an old
reusable native record onto a newly published source, then refuse to combine the
old journal's signing key with the current attached VM properties. The initial
one-off repair did not account for this; the revised script does.

If template repair already succeeded and Pulse exits with this error, leave Pulse
stopped and run:

```sh
./scripts/repair-stalled-macos-pool.sh --retire-legacy-state
pulse
```

The default legacy directory is
`~/Library/Application Support/pulse/providers/pulse-tart-e3ae0782ba0071285ea94`.
For an explicitly configured `state_dir`, pass `--state-dir /absolute/path`.

This mode verifies all four templates are suspended, both macOS bases are stopped,
the retained resources have consistent current keys, and there are no attached
job/pair reservations. It preserves the legacy journal in an adjacent private
`.retired-.../journal` backup. Any missing-property metadata on the second slots is
moved aside only when its UID matches the legacy native record and its old MAC
differs from the actual replacement's MAC. It refuses other unmigrated resources.
Current VM metadata, signing keys, disks and saved snapshots remain intact. No SSH,
VM restart, image rebuild or Pulse rebuild is required. The private backup is not
included in the shareable repair archive.

The ordinary repair path now performs this check after preparing all four sources,
so it can retire a conflicting leftover journal before the next Pulse startup.
