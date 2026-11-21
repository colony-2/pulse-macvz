# Provider logging proposal

Status: proposed; no logging implementation or dependency update is included.

Implement opt-in, per-launch logs for direct library users and both plugin modes.
Start with structured launch progress and the guest's existing combined worker
output. Keep log delivery independent of admission, job execution, and VM cleanup.
Pool preparation needs a separate progress interface because it usually happens
before any launch exists.

## Contract and migration

Reviewed upstream GitHub `main` at
[`2919f1ed5e42893f759af0fdd930c1a66824887b`](https://github.com/colony-2/pulse-provider-api/tree/2919f1ed5e42893f759af0fdd930c1a66824887b),
including [the log types](https://github.com/colony-2/pulse-provider-api/blob/2919f1ed5e42893f759af0fdd930c1a66824887b/launch_logs.go)
and [SDK lifecycle and migration rules](https://github.com/colony-2/pulse-provider-api/blob/2919f1ed5e42893f759af0fdd930c1a66824887b/plugin/README.md#launch-log-listeners).
This repository currently pins both modules to `27abdc0b7a4a`.

The required method becomes:

```go
Submit(context.Context, []api.Launch, api.SubmitOptions) (
    []api.Submission, api.LogSubscription, error,
)
```

A nil listener must return a nil subscription and create no logging readers or
callback workers. A requested listener gets a handle even when logging fails;
those failures must not replace admission results or errors. The listener has
its own context and duration, independent of the Submit context. Zero duration
means no lifetime cutoff. Callbacks can occur before Submit returns.

Upgrade the root API and plugin module together using published Git revisions,
without a local replacement. Update every Submit caller, wrapper, fake, and test.
Both plugin peers must support protocol baseline 3; there is no baseline-2
downgrade. Coordinate the Pulse collector/host update with this provider.
At the reviewed revision, the plugin's own go.mod still pins an older root API;
the upstream documentation explicitly distinguishes candidate checks from an
independently released SDK. Verify the selected module pair in a clean consumer
build before rollout. A matching explicit root pin can resolve the dependency
through Go module version selection, but does not establish an upstream release.

## What to stream

| Source | Initial implementation |
| --- | --- |
| `launch` | Base selection, admission decision, allocation assignment, native start/restore, SSH readiness, requested Docker/KVM readiness, guest handoff, and relevant failures. |
| `combined` | Unmodified payload bytes from the assigned primary guest's per-launch log under `/var/db/pulse-macvz/logs/<launch-key>/`, with the legacy path supported for existing images as described below. |
| `stdout`, `stderr` | Explicitly unavailable initially: the guest currently writes both streams to one file. |

`guest/main.go` already redirects both executor streams into `worker.log`.
Its current directory is `/var/db/pulse-tart`; the implementation will introduce
the new guest layout together with logging support.
Do not attempt to reconstruct their origin or label combined bytes as stdout.
For macOS plus Docker, execution output belongs to the macOS primary guest;
companion readiness belongs to `launch`. Do not stream Docker daemon logs as
executor output. For Linux allocations, use their Linux primary guest.

Launch events should carry stable, allowlisted fields in their Data payload:
event name, phase, resolved base, VM role, elapsed time, and a safe failure code.
Use transitions rather than repeating identical reconciliation messages. A
declined launch can receive its admission explanation, then close its launch
source and disclose execution output as unavailable. An uncertain submission
must remain uncertain; listening must never trigger a second dispatch.

## Native subscription implementation

Add an internal subscription manager owned by Runtime. The SDK handles transport,
but direct Go callers need the same semantics; the reviewed SDK's subscription
implementation is private, not an exported native-provider helper.

Register the batch listener before dispatch and associate accepted or uncertain
launches with immutable VM UIDs and LaunchIDs. Copy retained inputs. Keep listener
state out of durable VM/job records. Separate event production from delivery:
enqueue bounded data under short internal locks, then invoke callbacks outside
all runtime, provider, and VM locks. A callback must be able to call Stop or
read provider status without deadlocking.

Use one serial callback worker per subscription. Start with limits matching the
SDK defaults: 32 resident subscriptions, 64 queued records each, and a five-second
callback deadline. Enforce native bounds even when a transport also supplies
bounds. Queue exhaustion, callback failure, and slow listeners terminate the
subscription with the corresponding typed reason. Never block VM operations on
a consumer. A callback that ignores cancellation retains its capacity reservation
until it returns; do not replace it with an unlimited sequence of workers.

Split reads into at most 4096-byte records before assigning consecutive sequence
numbers per launch across all sources. Preserve arbitrary bytes, including NULs
and partial UTF-8, without line buffering or display prefixes. Stamp file-derived
records with host observation time; do not imply that this is the original guest
write time. Source prefixes are consumer display metadata.

Implement Stop, Done, Wait, Coverage, duration, callback cancellation, and runtime
close with stable terminal results. Normal source closure drains already queued
records. Other termination reasons discard the pending queue and cancel the
in-flight callback context. Wait's context only bounds that Wait. Coverage must
be copied, report unavailable/interrupted sources honestly, and count sequence
numbers at callback dispatch rather than callback completion. Output closure is
not a claim that the workload succeeded.

## Guest state directory migration

Newly prepared macOS and Linux images will use `/var/db/pulse-macvz`, with
logs partitioned by launch as described below. This is guest storage, separate
from the host's `~/.pulse/vz`.
The old directory name is a compatibility detail, not a Tart runtime dependency.

Migrate the whole worker state directory together. It also holds the assignment,
fingerprint, execution intent, completion marker, and service/ingest locks; moving
only the log would leave delivery and recovery split across two layouts. Update
`guest/main.go`, guest installation/bootstrap and image cleanup scripts, host
handoff/completion checks, Docker fingerprint checks, and diagnostic readers.
Retain private ownership and permissions.

Use an explicit guest layout version, recorded in prepared-image metadata and
reported by the guest helper. The provider selects one layout for a VM before
handoff, and every state reader and writer uses that selection. Include the
layout version in prepared-template compatibility checks so an old suspended
template cannot be mistaken for a new-layout template.

Existing images and running jobs keep their original directory. The updated host
supports their legacy layout until the operator refreshes the images with
`upgrade`; `setup` continues to reuse compatible prepared images. Do not move
active state, reset execution-intent markers, or start a second guest service to
perform this migration. New images are built with the new layout rather than
rewriting a running VM. Install the updated host before using new-layout images.

For older images without layout metadata, permit the legacy directory only when
the new layout is absent and the VM/assignment identity checks pass. A permission
error, corrupt marker, or mismatched LaunchID in the new directory is an error,
not a reason to fall back. If both layouts contain assignment state, report the
ambiguity and do not merge them or replay execution. Do not add a permanent
symlink or write duplicate state to both locations.

Log capture follows the selected layout. Legacy-path support does not imply that
an older helper supports streaming: if it lacks the read-only logging operation,
report execution coverage as unavailable until the image is upgraded. Keep
legacy readers explicitly identified so they can be removed in a later deliberate
compatibility change.

## Per-launch storage, rotation, and recovery

Today `worker.log` is an append-only file with no size limit. It is implicitly
associated with a one-shot VM, not named by LaunchID, and disappears when that
VM is deleted. The new design should retain bounded output by launch explicitly.

Use `/var/db/pulse-macvz/logs/<launch-key>/` for guest capture, with a private
manifest containing the original LaunchID, stream identity, format version,
retained byte range, closure information, and any known loss. Derive launch-key
from a collision-resistant hash of the LaunchID rather than placing arbitrary
IDs directly in paths. Host copies live under
`<log-directory>/<owner-scope>/<launch-key>/` (default log directory:
`~/.pulse/vz/logs`) and additionally record the immutable
VM UID and JobDB instance/tenant/job association. Never include the lease or
assignment envelope. Tenant metadata is correlation information, not authorization
to read another tenant's logs.

Expose the host root through a public `Config.Logging` configuration, including
`LoggingConfig.Directory`, so embedding Pulse can supply its own destination:

```go
cfg := macvz.Config{
    Logging: &macvz.LoggingConfig{Directory: "/path/to/provider-logs"},
}
```

These are proposed fields, not existing API. A nil logging configuration or empty
directory selects `<VZ home>/logs`, normally `~/.pulse/vz/logs`. Add a persisted
`--log-directory` option to server, setup, and upgrade, including embedded Cobra
commands. Explicit flags override saved values; saved relative paths resolve
against the configuration file, following the existing path-option rules. Direct
Go callers supply an absolute path when overriding the default. Keep scope and
launch subdirectories even with a custom root. Create private directories and
files; reject an unusable explicitly configured destination during initialization
without exposing credentials. Later storage failures affect logging, not accepted
work. Changing the root does not move or delete old logs automatically; document
how to retain access to the previous location. This host setting does not change
the guest's fixed state layout or require rebuilding prepared images.

Use an existing library for rotation rather than maintaining our own rename and
backup cleanup code. The initial candidate is
[`gopkg.in/natefinch/lumberjack.v2`](https://github.com/natefinch/lumberjack): it is
an `io.Writer` with size rotation and backup retention, intended for one writer
process per file set. Pin a reviewed version. One guest helper owns each launch's
writer; the host never writes into the guest's rotating files. Do not put the
library behind slog or another text formatter: executor output is arbitrary bytes.

Proposed configurable defaults are 16 MiB of uncompressed data per segment and
three backups plus the active segment, approximately 64 MiB of uncompressed
history per launch after cleanup. This retains
the newest output, not the complete history of an arbitrarily verbose job.
Enable Lumberjack's `Compress` option by default for guest capture and host
copies: rotated segments become gzip files while the current segment remains
uncompressed. Compression and rotation cleanup are asynchronous, so compression
can temporarily retain both representations and this is a retention target rather
than an instantaneous filesystem quota. Expose compression and rotation limits
alongside Directory in LoggingConfig, with an explicit default-versus-false value
for compression (for example `*bool`), and corresponding persisted CLI options.
Guest rotation settings are applied to future launches through versioned helper
configuration; host Directory is never passed into the guest. Add a host-wide
retained-byte budget (initially 1 GiB, measured in actual stored bytes) and
seven-day completed-launch retention, enforced at startup and periodically;
per-file rotation alone does not bound the number of launches. Evict completed
launches first and explicitly record any active-stream history lost to the
aggregate budget. Expiry and eviction must never restart or cancel execution.

Rotation is not a replay protocol. Put a small, versioned framing adapter above
the writer: bounded records contain stable stream identity, absolute payload
offset, length, and integrity information. Payload bytes remain unchanged when
delivered to the API. Recover cursors from valid persisted frames and publish
sidecar manifests atomically; ignore a torn final frame and report the gap. Do
not derive a resume cursor from a rotated filename, modification time, or the
API's subscription-local Sequence. Test the library's rotation/read races and
restart behavior before accepting it as the storage component.

Readers support both uncompressed and gzip segments and identify them by their
framed stream/offset metadata. During compression, treat the two representations
as the same segment, never duplicate output, and rediscover the segment if its
path changes before opening. Resume offsets refer to uncompressed payload bytes;
reading a gzip segment may require decompressing and skipping to that offset.
Bound decompression memory and validate integrity before trusting recovered data.
Compression errors must remain observable without losing the original segment
or being reported as successful archival completion.

The guest capture writer continues independently of Pulse and of listeners.
When Pulse restarts, it may recover the retained guest ranges for owned VMs into
the bounded host store. Persist the host's copied cursor only after the data it
describes is durable. If the requested cursor predates retained data, report the
missing range instead of silently pretending the stream is complete. Host copies
allow later retrieval after VM deletion, but cannot recover bytes never copied
before a VM or disk was lost. VM cleanup gets only a bounded final-copy attempt,
not an unlimited wait for logging; store partial coverage when necessary.

Persisted output does not revive an old callback subscription. The current API
has no attach/read-existing-launch method. Propose a separate upstream operation
that accepts an existing launch identity and opaque cursor, verifies caller
access, and returns retained data plus gap/closure information. It must never
call Submit. Until that exists, support storage recovery and a provider-local
inspection interface without claiming automatic listener reconnection. Remote
replay requires coordinated changes in Pulse and the provider API.

## Guest capture and VM lifecycle

The guest helper owns capture and rotation for the lifetime of the worker;
listeners only read its stored output. Unlike today's direct file descriptor,
an `io.Writer` rotation library requires guest-owned stdout/stderr copying.
Bound the capture queue, keep draining output after storage failure or overflow,
and report lost ranges. Do not let a failed writer close the worker's output
pipe or turn a log failure into an execution failure. This changes guest capture,
not the signed host executable layout, and adds no host Python dependency.

Add a read-only guest helper operation for following output. It must verify the
requested LaunchID against the guest's current assignment, open only the fixed
per-launch log, and emit bounded binary frames for data and explicit closure. It
must not print the assignment envelope. The host also verifies the VM UID before
attaching. A warm VM must not expose a previous launch's output; establish a
fresh per-launch log identity when accepting an assignment and reject mismatches.

The reader can start before capture exists and follow from the earliest retained
offset for that launch, reporting any missing prefix. This allows early output
to be delivered after handoff. Cancellation closes
only that SSH observation session. A missing helper in an older prepared image
means execution coverage is unavailable, not that the job should be refused or
the image rebuilt automatically. Deploy the helper through the normal image
upgrade path and disclose this limitation until those bases are refreshed.

An ordinary read reaching the current file size is not EOF for a live worker.
Declare closure only after the writer has closed and the captured final byte
count has been consumed. VM shutdown, deletion, source truncation, or loss of the
SSH session before that evidence must report interrupted coverage. Do not delay
job completion or retain a VM indefinitely to satisfy a listener. Bounded guest
retention and host copying improve recovery but do not guarantee lossless archival
delivery across shutdown. Test the shutdown race explicitly.

On provider shutdown or plugin session loss, detach subscriptions while accepted
work continues. A new provider can recover stored output, but must not reconnect
an old subscription or replay Submit. Log lifetime is not job lifetime.

## Pool preparation and diagnostics

The launch-log API does not expose a pool-wide subscription or a source inventory
for work done before Submit. It will not, by itself, answer “what is setup doing?”
or “which warm VMs are idle?” Do not create fake launches for base builders or
keep a declined launch subscribed to unrelated background preparation.

Keep `Images`, `Slots`, `DockerAllocations`, and delivery observations as the
authoritative status inputs. Propose a separate provider-local progress sink
shared by the CLI and an embedding Pulse process. It can report base/slot phases,
download bytes and estimates, guest readiness, shutdown/finalization, idle worker
counts, and assigned jobs. Reuse those events for the interactive and linear
setup displays. When a real launch is waiting on an already assigned resource,
forward only its relevant progress into that launch's stream.

Provider-generated events must exclude leases, launch envelopes, stdin, environment
values, SSH credentials, and credential-bearing URLs. Do not forward arbitrary
raw errors or entire native logs without reviewing their contents. Executor
output is application-controlled and can contain secrets; preserve its bytes
under the requested listener contract rather than claiming automatic redaction.
Apply the bounded retention policy above to host copies and restrict access to
the owning user and authorized launch consumers.

## Delivery sequence and validation

1. Migrate both module pins and Submit signatures. Implement bounded native
   subscriptions with explicit unavailable execution coverage; keep existing
   admission behavior unchanged.
2. Add launch progress events and direct-library lifecycle tests. Cover callback
   reentrancy, panic, blocked callbacks, overflow, duration zero, cancellation,
   mixed accepted/declined batches, uncertain admission, and provider close.
3. Introduce the versioned guest state layout and update all state consumers.
   Test new images, legacy images, mixed pools, interrupted preparation, and
   restart of an already assigned legacy job. Test both-directory ambiguity,
   permission failures, and mismatched identity without fallback or replay.
4. Add per-launch rotation, stable cursors, bounded host storage, and recovery.
   Test size rotation during reads, expired cursors, truncated records, disk full,
   capture overflow, aggregate retention, and provider downtime followed by
   recovery. Cover asynchronous compression, simultaneous compressed/uncompressed
   representations, resume inside a gzip segment, and interrupted compression.
   Test custom directories through direct configuration and CLI persistence,
   relative saved paths, permissions, and unchanged guest/image identity when
   only the host log root changes. Prove that private job/tenant association
   cannot mix streams.
5. Add guest output capture and immutable launch/VM identity checks. Test binary
   output, writes larger than 4096 bytes, output before Submit returns, long streams
   without a cumulative quota, terminal byte accounting, old images, and VM
   shutdown during capture. Verify that detaching never stops the worker.
6. Exercise the same contract through inbound and outbound SDK transports,
   including protocol mismatch, disconnect, reconnect, and independent admission
   and listener cancellation. Test a fresh published-module consumer.
7. Qualify on macOS, Linux, and macOS with Docker using a stopped production pool
   and cold fixture source. Verify one real execution despite listener failures.

Recommended scope: launch progress plus combined output, per-launch bounded
storage, and recoverable cursors. Implement remote reattachment only with an
explicit API extension; do not promise lossless output across VM shutdown. Keep
pool progress separate because the per-launch contract cannot represent it.
