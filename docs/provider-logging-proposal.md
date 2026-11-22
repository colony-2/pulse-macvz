# Provider launch logging

Status: implemented against provider API and plugin revision `bd1a5c559150`
(protocol baseline 3). This document records the implementation and its limits.

## API and lifetime

`Runtime.Submit(ctx, launches, api.SubmitOptions)` returns admission results,
an optional `api.LogSubscription`, and the admission error. A nil listener returns
nil and allocates no subscription readers or callback workers. The provider's
background archive collector and the guest capture service operate independently
of subscriptions; they preserve output for subsequent local inspection.

A requested listener receives admission decisions and observed persisted execution
phase transitions in `launch`, and unmodified worker bytes in `combined`.
`stdout` and `stderr` are explicitly unavailable because capture combines them.
For macOS with Docker, output comes from the macOS primary, not the Docker daemon
or Linux companion. Linux allocations expose their primary's worker output.

Launch events contain only allowlisted status/phase values. They do not contain
lease bytes, process arguments, environment variables, guest scripts, arbitrary
backend errors, or credential URLs. The worker's own output is not redacted or
reformatted. Preparation/download telemetry remains the separate preparation
progress API. Individual SSH attempts and native start/restore operations are
not currently emitted as separate launch events.

Listener lifetime is independent of the admission context. Duration zero means
no cutoff. Stop, context cancellation, callback failure, a slow callback, source
loss, or provider Close terminates observation without canceling accepted work.
Callbacks may reenter Stop, List, and Close. Coverage and terminal results are
copied. Normal closure drains queued records; abnormal termination discards them.
A native output closure is not a claim that the job succeeded.

Limits per Runtime are 32 resident subscriptions, 64 queued records per
subscription, and a five-second callback deadline. Each callback has a cancelable
deadline context. Records contain at most 4096 bytes, preserving NULs and partial
UTF-8. Sequence numbers start at one per launch across sources and count callback
dispatch. Timestamps are host observation times. A callback that ignores
cancellation retains its capacity reservation until it returns; Done and Wait do
not wait for it, and no replacement callback goroutines are created.

## Retained retrieval

`Runtime` implements `api.ProviderWithLaunchLogs.GetLaunchLog`. The inbound and
reverse plugins expose it automatically. Reads use only the host archive, with no
VM contact, admission, reconciliation, or dependence on a Submit listener.
Retained records contain `combined` output; synthetic live `launch` events are not
archived. Capture begins with worker execution, not base-image preparation.

Pages contain 1–100 immutable records (default 100). Checkpoints bind the archive
root/owner scope, LaunchID, and next record's byte offset. They survive process
restarts and page-size changes; relocating the archive root invalidates them.
Every successful read returns a checkpoint, including an empty active tail.
`HasMore` describes records available now. `Finalized` means capture cannot append
more, including interrupted/unavailable capture, and is independent of job status.
`Incomplete` discloses known loss or missing legacy metadata across the log.
Missing archives return `ErrLaunchLogNotFound` before cursor validation; invalid,
cross-launch/scope, and trimmed checkpoints return `ErrLaunchLogCursor` without
silently skipping output. Cancellation and storage failures remain errors.

The size hint counts raw retained output bytes when the archive has no known
loss; otherwise it is unknown. It is never a pagination or completion signal.
Retention remains seven days by default, with earlier exceptional loss possible
under the configured per-owner byte budget, guest rotation, or storage failure.

No image rebuild is needed to retrieve already captured output. Old frames lack
capture timestamps and record numbers: retrieval preserves their bytes, exposes
zero timestamps, uses stable `offset+1` sequences, and sets `Incomplete=true`.
Upgrade prepared images to capture full metadata for future launches. Stored
sequences and timestamps are independent of live-listener sequence numbers and
host observation times. Neither retrieval nor an upgrade fabricates old metadata.

## Storage and configuration

Guest capture uses the versioned worker layout:

```
/var/db/pulse-macvz/
  intent, fingerprint, launch.json, complete, service.lock, ingest.lock
  logs/<sha256-of-launch-id>/manifest.json
  logs/<sha256-of-launch-id>/output.log
  logs/<sha256-of-launch-id>/output-<rotation-timestamp>.log.gz
```

The helper owns capture independently of Pulse and of listeners. A bounded queue
feeds `gopkg.in/natefinch/lumberjack.v2` v2.2.1. By default it rotates at 16 MiB,
retains three backups plus the active segment, and compresses backups. The limit
counts encoded journal bytes (including framing/base64), not raw worker bytes.
Compression/backup cleanup are asynchronous and may temporarily retain both
representations. This is a retention target, not an instantaneous disk quota.
Storage failure or queue overflow continues draining stdout/stderr and preserves
the worker's exit status; missing byte ranges are observable.

Frames contain capture timestamps, stable record sequence numbers, absolute
raw-byte offsets, bounded binary payloads, and CRC32
integrity checks. A private versioned manifest binds the stream to its LaunchID,
records the produced byte count and known loss, and distinguishes live output
from explicit closure. Readers handle uncompressed and gzip segments and deduplicate
by offset. Temporary EOF is not closure. Torn frames and expired history produce
gaps instead of fabricated success.

Host copies live at:

```
<directory>/<owner-scope>/<sha256-of-launch-id>/
  identity.json
  manifest.json
  <start-offset>-<end-offset>.log.gz
```

`identity.json` stores the immutable VM UID/name and JobDB instance, tenant, and
job identifiers, without the lease or assignment envelope. Host copies use
bounded, atomically published batches, optionally gzip-compressed, rather than
keeping a Lumberjack writer and compression goroutine for every historical job.
Data is synced before the durable cursor advances. Deterministic range filenames
make replay after a crash idempotent. Scope and per-launch directories are private.
Tenant metadata is correlation information, not an access-control boundary.

The default directory is `<VZ home>/logs`, normally `~/.pulse/vz/logs`. Go callers
can configure it without changing pool identity:

```go
cfg.Logging = macvz.LoggingConfig{
    Directory: "/path/to/private/provider-logs",
    SegmentMiB: 16,
    Backups: 3,
    MaxBytes: 1 << 30,
    MaxAge: 7 * 24 * time.Hour,
    // Compress: nil means true; a pointer to false disables compression.
}
```

Zero values select defaults. Segment size is 1–64 MiB and backup count is 1–16.
Direct Go callers must supply an absolute override directory. Directory creation
or later storage failures affect logging, never admission. A pre-existing
permissive or symlinked scope/stream directory is rejected for writes.

The shared Cobra commands expose and persist:

- `--log-directory`: host archive root.
- `--log-segment-mib`: guest rotation size, default 16.
- `--log-backups`: retained guest backups, default 3.
- `--log-compress`: guest backup and host batch compression, default true.
- `--log-max-bytes`: host archive budget per owner scope, default 1 GiB.
- `--log-max-age`: completed archive retention, default 168h.

Saved relative paths resolve against the configuration file; explicit flags win.
Guest rotation options travel as versioned helper controls for future launches
and are removed from the worker environment. The host directory never travels
to the guest. Changing the directory does not relocate older archives. Use the
old `--log-directory` to inspect those files.

Retention runs at collector startup and between collection passes. It counts
stored compressed bytes, removes expired completed streams, then evicts oldest
completed streams before active ones when over budget. Small consecutive host batches coalesce into bounded files to avoid a file
per poll. There are also 4096-stream and 65536-file limits. An active eviction interrupts logging, not the job. The budget is a
cleanup target; in-flight batches can temporarily exceed it.

## Recovery and VM lifecycle

The provider collector checks immutable VM identity before each guest read. The
read-only `logs` helper checks the LaunchID against the queued assignment or
execution intent and returns at most 64 KiB of payload. The host uses at most
four concurrent management reads. Collection resumes from the host's durable
raw-byte cursor after a provider restart and never calls Submit to reattach.
If reconciliation temporarily holds the store lock, collection continues using
its previously verified identities.

On normal execution completion, the guest gives the host up to ten seconds to
acknowledge durable final output, then proceeds with its normal three-second
shutdown delay. Acknowledgement is a separate operation that validates the
LaunchID and final byte count. It does not change the execution result or replay
work. The provider never holds a VM indefinitely for logging. If the VM stops,
is deleted, or loses its disk before bytes are copied, the archive can be partial;
those bytes cannot be recovered from the host. Loss/interruption is not reported
as a complete stream.

The guest can keep capturing while Pulse is absent. Recovery requires the VM and
retained guest data still to exist. Persisted output does not revive a previous
callback. The current API has no attach-existing-launch operation; remote
listener replay requires an upstream API addition. For local inspection, without
opening a controller or acquiring its pool lock:

```sh
pulse-macvz logs --launch-id LAUNCH_ID
pulse-macvz logs --launch-id LAUNCH_ID --offset 4096 --log-directory /old/log/root
```

Standard output contains raw worker bytes. Gap/incomplete notices go to stderr.
The command is a snapshot, not a follow operation, and never submits work.

## Existing images

The updated helper identifies itself as `pulse-macvz-guest 0.2.0`. Newly built
images use `/var/db/pulse-macvz` for the whole worker state directory, including
intent/fingerprint/completion/locks. The host recognizes the old
`pulse-tart-guest 0.1.0` layout only when the new directory is absent. New capture
never reads legacy `worker.log` as if it were the new journal, and new helpers
refuse legacy execution state rather than moving it or resetting intent.

Old prepared images remain usable but report `combined` unavailable because
their helper lacks the log-read operation. `setup` can continue reusing these
images; run the normal `upgrade` workflow to rebuild them with capture support.
Running jobs are not migrated and logging never automatically rebuilds images.
Install the new host before preparing new-layout images. The executable/service
names under `/opt/pulse-tart` remain compatibility details, not a Tart dependency.

## Validation

Tests cover binary subprocess output, storage failures preserving exit status,
assignment isolation, acknowledgement validation, compressed rotation and retention
gaps, cursor recovery, idempotent replay, immutable VM identities, private storage,
archive expiry/budget eviction, callback reentrancy/deadlines, retained capacity
for stuck callbacks, queue overflow, copied coverage, configuration persistence,
local inspection, paginated retrieval across runtime restarts, expired and
cross-scope checkpoints, active tails, legacy metadata, and real reverse SDK
delivery/retrieval over the tested authentication and transport variants. The full suite runs with the race detector.

VM qualification remains opt-in. Unit and SDK tests do not establish that an
existing prepared image already contains the new helper; live qualification must
build/upgrade disposable images before asserting execution-log availability.
