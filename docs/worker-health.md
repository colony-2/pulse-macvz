# Worker health monitoring

The provider probes the c2j worker in each newly admitted launch. Monitoring
runs in the provider process, independently of preparation/reconciliation and
log collection. The reverse connection is not the heartbeat transport: the
provider talks directly to its VM over the host's local network.

`pulse-macvz launches health LAUNCH_ID` performs a fresh read-only probe while
the controller is running. Add `--json` (`-j`) for structured output. The command
shows the current probe separately from the last recorded monitor observation,
which may be stale if the provider is stopped. It includes tenant/job identity,
worker start, current task and task start, extension selector, and last known
lease expiry. `launches show LAUNCH_ID --json` retains the last health state and
cleanup reason after the VM has been deleted.

## Timing and cleanup

| Setting | Default |
| --- | --- |
| Probe | `GET http://<guest-ip>:8080/health` |
| Heartbeat interval | 5 seconds |
| Timeout per probe, including IP discovery | 3 seconds |
| Initial startup allowance | 60 seconds |
| Continuous failure required after the allowance or a successful probe | 30 seconds, with at least three failures |

A worker that never answers has approximately 90 seconds before a stop decision.
A previously healthy worker normally gets stopped about 30–40 seconds after its
endpoint becomes unavailable. Scheduling and controller lock contention can add
delay. Short outages reset on the next successful probe. Unsupported images,
missing/unreadable native metadata, and canceled provider operations are not
failed heartbeats. Unknown submission outcomes are not automatically reaped.

Before stopping, the provider verifies the VM's immutable UID and specification
against the launch record, and persists a cleanup intent. Existing reclamation
then stops/deletes the primary and its Docker companion, releases the slot, and
replenishes configured capacity. The reason and last observation are retained in
launch history. No launch is replayed.

The health decision survives provider restarts. A restart or a gap in monitoring
starts a fresh failure window; time spent with no provider running never counts
as observed failure. A cleanup intent already committed before the restart is
completed. Monitoring requires a running provider: closing it leaves jobs alone.

## Endpoint semantics and compatibility

The [c2j health contract](https://github.com/colony-2/c2j#worker-health-probes)
returns HTTP 200 when c2j can serve a snapshot. This is a liveness test, not proof
of task progress or fresh lease authority. Task age and lease expiry are shown
for diagnosis; they are not automatic kill criteria. Long operations and waits
are legitimate. A hung HTTP listener, non-200/malformed response, or wrong
job/tenant response counts as a failed probe. Redirects and proxy environment
settings are ignored; responses are size-bounded and response bodies never
appear in error messages.

Image observation checks `c2j run with-lease --help` for `--health-port` and stores
that capability with the image's c2j version. Clones inherit it, and native guest
delivery refreshes it. The default c2j port, 8080, is reserved for this worker
probe. No guest-helper replacement or extra service is required.

Launches admitted by an older provider retain their original behavior. New
launches from older images show `unsupported` and are never killed for lacking
an endpoint. Install/restart the updated provider and upgrade prepared images to
a c2j revision containing the endpoint, for example:

```sh
make install
# Stop the owner scope's provider before management; upgrade drains accepted work.
pulse-macvz upgrade --c2j-version main
# Restart the separately supervised provider with its usual server options.
pulse-macvz launches health LAUNCH_ID
```

`latest` selects the latest release; `main` explicitly selects the branch.
Neither installing the provider nor running setup forces an existing completed
base to rebuild. Upgrade is the operation that updates its c2j executable.

## Validation

c2j main revision `3991db6930ec62fb0bf85e4da824d6a8fc4fdfa7` was cross-built for
Linux ARM64 and measured inside a disposable Ubuntu VM (2 vCPU, 4 GiB).
Ten ordinary-user `run with-lease` invocations answered in 21.4–24.0 ms
(mean 22.1 ms). These measure process-to-endpoint startup, not VM boot. A local
JobDB fixture held the synthetic lease-import request; no production jobs were
claimed or modified. The startup allowance includes substantial VM scheduling,
boot, and restore headroom; these measurements are not a latency guarantee.

The normal guest-helper handoff answered its first host probe in 0.60 seconds.
A sustained process freeze resulted in native VM stop and confirmed deletion
in about 40 seconds; a transient freeze recovered without cleanup.

The opted-in live test uses a cold prepared Linux image, an actual c2j binary,
the normal one-shot guest helper, and a loopback JobDB fixture. It pauses c2j to
verify transient recovery, then freezes it permanently and checks that the real
provider monitor stops and deletes the VM. It never starts the production pool.

```sh
PULSE_VZ_LINUX_BASE=/absolute/path/to/cold/prepared/ubuntu-base \
PULSE_VZ_RUNNER=/absolute/path/to/signed/pulse-macvz \
PULSE_VZ_RESULTS="$PWD/test-results" \
PULSE_HEALTH_C2J_BINARY=/absolute/path/to/linux-arm64/c2j \
CGO_ENABLED=0 go test ./internal/provider \
  -run '^TestLiveWorkerHealthReaping$' -count=1 -timeout=5m -v
```

Unit tests cover startup/failure windows, recovery, restart/gaps, durable cleanup
intents, unsupported images, ambiguous admission, canceled observation, changed
VM identities, paired reclamation, bounded HTTP parsing, and read-only CLI output.
