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
| `combined` | Unmodified bytes from the assigned primary guest's `/var/db/pulse-tart/worker.log`. |
| `stdout`, `stderr` | Explicitly unavailable initially: the guest currently writes both streams to one file. |

`guest/main.go` already redirects both executor streams into `worker.log`.
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

## Guest capture and VM lifecycle

Keep executor output directed to its existing file, not a pipe owned by a log
subscriber. Detaching a reader therefore cannot block the executor or send it
SIGPIPE. This change adds no host Python dependency or separately signed binary.

Add a read-only guest helper operation for following output. It must verify the
requested LaunchID against the guest's current assignment, open only the fixed
worker log, and emit bounded binary frames for data and explicit closure. It
must not print the assignment envelope. The host also verifies the VM UID before
attaching. A warm VM must not expose a previous launch's output; establish a
fresh per-launch log identity when accepting an assignment and reject mismatches.

The reader can start before the file exists and follow from byte zero for that
launch, allowing early output to be delivered after handoff. Cancellation closes
only that SSH observation session. A missing helper in an older prepared image
means execution coverage is unavailable, not that the job should be refused or
the image rebuilt automatically. Deploy the helper through the normal image
upgrade path and disclose this limitation until those bases are refreshed.

An ordinary read reaching the current file size is not EOF for a live worker.
Declare closure only after the writer has closed and the captured final byte
count has been consumed. VM shutdown, deletion, source truncation, or loss of the
SSH session before that evidence must report interrupted coverage. Do not delay
job completion or retain a VM indefinitely to satisfy a listener. Initially this
is best-effort live capture; guaranteed archival delivery would need a separate
durable output design. Test the shutdown race explicitly.

On provider shutdown or plugin session loss, detach subscriptions while accepted
work continues. Do not reconnect an old subscription, replay Submit, or imply
historical retrieval. Log lifetime is not job lifetime.

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
Do not add a host log archive or retention policy in this change.

## Delivery sequence and validation

1. Migrate both module pins and Submit signatures. Implement bounded native
   subscriptions with explicit unavailable execution coverage; keep existing
   admission behavior unchanged.
2. Add launch progress events and direct-library lifecycle tests. Cover callback
   reentrancy, panic, blocked callbacks, overflow, duration zero, cancellation,
   mixed accepted/declined batches, uncertain admission, and provider close.
3. Add guest output capture and immutable launch/VM identity checks. Test binary
   output, writes larger than 4096 bytes, output before Submit returns, long streams
   without a cumulative quota, terminal byte accounting, old images, and VM
   shutdown during capture. Verify that detaching never stops the worker.
4. Exercise the same contract through inbound and outbound SDK transports,
   including protocol mismatch, disconnect, reconnect, and independent admission
   and listener cancellation. Test a fresh published-module consumer.
5. Qualify on macOS, Linux, and macOS with Docker using a stopped production pool
   and cold fixture source. Verify one real execution despite listener failures.

Recommended initial scope: launch progress plus combined output, no historical
log API, and no promise of lossless output across VM shutdown. Keep pool progress
as a separate integration, because the per-launch contract cannot represent it.
