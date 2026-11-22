package servercli

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	macvz "github.com/colony-2/pulse-macvz"
	"github.com/colony-2/pulse-macvz/internal/backend"
	api "github.com/colony-2/pulse-provider-api"
)

type maintenanceFake struct {
	draining, closed, purged, keepJob   bool
	steps                               int
	checkpointed, committed, rolledBack bool
	upgradePhase                        string
}

func (f *maintenanceFake) ImageUpgradeState() (string, error) { return f.upgradePhase, nil }
func (f *maintenanceFake) BeginImageUpgrade(context.Context) error {
	if !f.draining || f.keepJob {
		return errors.New("checkpoint before drain")
	}
	f.checkpointed = true
	f.upgradePhase = "preparing"
	return nil
}
func (f *maintenanceFake) ActivateImageUpgrade(context.Context) error {
	f.upgradePhase = "preparing"
	return nil
}
func (f *maintenanceFake) CommitImageUpgrade(context.Context) error {
	f.committed = true
	f.upgradePhase = ""
	return nil
}
func (f *maintenanceFake) RollbackImageUpgrade(context.Context) (bool, error) {
	f.rolledBack = true
	f.upgradePhase = ""
	return true, nil
}

func (*maintenanceFake) Check(context.Context) error       { return nil }
func (f *maintenanceFake) Reconcile(context.Context) error { f.steps++; return nil }
func (f *maintenanceFake) Drain(context.Context) error     { f.draining = true; return nil }
func (f *maintenanceFake) Close() error                    { f.closed = true; return nil }
func (f *maintenanceFake) PurgeImages(context.Context) error {
	if !f.draining || f.steps < 3 || f.keepJob || !f.checkpointed {
		return errors.New("attempted purge while work was active")
	}
	f.purged = true
	return nil
}
func (f *maintenanceFake) Slots() ([]macvz.SlotStatus, error) {
	if f.draining {
		return nil, nil
	}
	return []macvz.SlotStatus{{ID: "standard-0001", Phase: "ready"}}, nil
}
func (*maintenanceFake) Images() ([]macvz.ImageStatus, error) {
	return []macvz.ImageStatus{{Name: "base", Phase: "ready"}}, nil
}
func (*maintenanceFake) DockerAllocations() ([]macvz.DockerStatus, error) { return nil, nil }
func (f *maintenanceFake) List(context.Context, api.ListRequest) (api.ListResponse, error) {
	if f.draining && (f.keepJob || f.steps < 3) {
		return api.ListResponse{Items: []api.Instance{{ID: "accepted-job"}}}, nil
	}
	return api.ListResponse{}, nil
}

func maintenanceOptions(t *testing.T) serverOptions {
	t.Helper()
	c, err := (capacityOptions{macOS: 1, macBase: "macos-26-arm64"}).config("test", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return serverOptions{config: c, poll: time.Millisecond, managementTimeout: time.Second, log: io.Discard}
}
func TestUpgradeDrainsBeforePurgingAndPreparing(t *testing.T) {
	old, new := &maintenanceFake{}, &maintenanceFake{}
	opens := 0
	err := manage(t.Context(), maintenanceOptions(t), true, func(serverOptions) (managedRuntime, error) {
		opens++
		if opens == 1 {
			return old, nil
		}
		if !old.closed || !old.purged {
			t.Fatal("new capacity started before old runtime drained")
		}
		return new, nil
	})
	if err != nil || opens != 2 || !new.closed || new.steps == 0 {
		t.Fatal("incomplete upgrade", err, opens)
	}
}
func TestUpgradeTimeoutPreservesAcceptedJobsAndSources(t *testing.T) {
	f := &maintenanceFake{keepJob: true}
	o := maintenanceOptions(t)
	o.managementTimeout = 10 * time.Millisecond
	err := manage(t.Context(), o, true, func(serverOptions) (managedRuntime, error) { return f, nil })
	if !errors.Is(err, context.DeadlineExceeded) || f.purged || !f.closed {
		t.Fatal("timeout did not preserve active work", err)
	}
}
func TestSetupReusesReadySources(t *testing.T) {
	f := &maintenanceFake{}
	if err := manage(t.Context(), maintenanceOptions(t), false, func(serverOptions) (managedRuntime, error) { return f, nil }); err != nil {
		t.Fatal(err)
	}
	if f.draining || f.purged || !f.closed {
		t.Fatal("setup replaced prepared sources")
	}
}

type historicalDeliveryRuntime struct{ progressRuntime }

func (f *historicalDeliveryRuntime) Reconcile(context.Context) error {
	return errors.New("historical delivery failure")
}
func (f *historicalDeliveryRuntime) ReconcileMaintenance(context.Context) error {
	f.steps++
	return nil
}
func TestSetupIgnoresSupersededSourceFailureButRequiresCurrentSources(t *testing.T) {
	for _, phase := range []string{"ready", "failed"} {
		t.Run(phase, func(t *testing.T) {
			f := &historicalDeliveryRuntime{progressRuntime: progressRuntime{images: []macvz.ImageStatus{
				{Name: "old-base", Phase: "failed", Obsolete: true},
				{Name: "current-base", Phase: phase},
			}, slots: []macvz.SlotStatus{{ID: "standard-0001", Phase: "ready"}}}}
			err := manage(t.Context(), maintenanceOptions(t), false, func(serverOptions) (managedRuntime, error) { return f, nil })
			if (err == nil) != (phase == "ready") || f.steps == 0 {
				t.Fatal("incorrect setup readiness", err, f.steps)
			}
		})
	}
}

type progressingFailure struct{ progressRuntime }

func (f *progressingFailure) Reconcile(context.Context) error {
	f.steps++
	if f.steps == 3 {
		f.images[1].Phase = "ready"
		f.slots = []macvz.SlotStatus{{ID: "standard-0001", Phase: "ready"}}
	}
	return nil
}
func TestSetupCompletesIndependentPreparationBeforeReportingFailure(t *testing.T) {
	f := &progressingFailure{progressRuntime: progressRuntime{images: []macvz.ImageStatus{
		{Name: "failed-companion", Companion: true, Phase: "failed"},
		{Name: "base", Base: "macos-26-arm64", Phase: "preparing"},
	}}}
	err := manage(t.Context(), maintenanceOptions(t), false, func(serverOptions) (managedRuntime, error) { return f, nil })
	var failure *preparationFailure
	if !errors.As(err, &failure) || f.steps != 3 {
		t.Fatal("abandoned independent work", err, f.steps)
	}
}
func TestSetupResumesCheckpointWithoutDrainingOrPurging(t *testing.T) {
	f := &maintenanceFake{upgradePhase: "preparing"}
	err := manage(t.Context(), maintenanceOptions(t), true, func(serverOptions) (managedRuntime, error) { return f, nil })
	if err != nil || f.draining || f.purged || !f.committed {
		t.Fatal("did not resume", err, f)
	}
}

type failingUpgrade struct{ progressRuntime }

func (f *failingUpgrade) RollbackImageUpgrade(context.Context) (bool, error) {
	f.rolledBack = true
	f.upgradePhase = ""
	f.images[0].Phase = "ready"
	return true, nil
}
func TestUpgradeFailureRestoresAndVerifiesPreviousCapacity(t *testing.T) {
	f := &failingUpgrade{progressRuntime: progressRuntime{
		maintenanceFake: maintenanceFake{upgradePhase: "preparing"},
		images:          []macvz.ImageStatus{{Name: "base", Base: "macos-26-arm64", Phase: "failed"}},
		slots:           []macvz.SlotStatus{{ID: "standard-0001", Phase: "ready"}},
	}}
	err := manage(t.Context(), maintenanceOptions(t), false, func(serverOptions) (managedRuntime, error) { return f, nil })
	if err == nil || !f.rolledBack || f.steps < 2 || f.committed {
		t.Fatal("did not restore failed upgrade", err, f.steps)
	}
}
func TestInterruptedPreparationRetainsCheckpoint(t *testing.T) {
	f := &progressRuntime{maintenanceFake: maintenanceFake{upgradePhase: "preparing"}, images: []macvz.ImageStatus{{Name: "base", Phase: "preparing"}}}
	o := maintenanceOptions(t)
	o.managementTimeout = 5 * time.Millisecond
	err := manage(t.Context(), o, false, func(serverOptions) (managedRuntime, error) { return f, nil })
	if !errors.Is(err, context.DeadlineExceeded) || f.rolledBack || f.committed || f.upgradePhase != "preparing" {
		t.Fatal("discarded interrupted upgrade", err)
	}
}

func TestSetupMissingCompanionCannotReportReadyOnRegistryFailure(t *testing.T) {
	c, err := (capacityOptions{macOS: 1, macDocker: -1, macBase: "macos-26-arm64"}).config("test", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	ep := c.Engine(c.Slots[0].DockerProfile)
	f := &progressRuntime{images: []macvz.ImageStatus{{Name: "base", Base: "macos-26-arm64", Phase: "ready"}}, slots: []macvz.SlotStatus{{ID: "standard-0001", Phase: "ready"}}, progress: macvz.PreparationProgress{Sources: []macvz.SourceProgress{{Source: ep.Image, TransferProgress: backend.TransferProgress{Stage: "failed"}}}}}
	ready, err := capacityReady(c, f)
	var failure *preparationFailure
	if ready || !errors.As(err, &failure) {
		t.Fatal("missing failed companion reported ready", ready, err)
	}
}

func TestSetupResumesRetirementAfterCheckpointInterruption(t *testing.T) {
	old, next := &maintenanceFake{upgradePhase: "staged"}, &maintenanceFake{}
	opens := 0
	err := manage(t.Context(), maintenanceOptions(t), false, func(serverOptions) (managedRuntime, error) {
		opens++
		if opens == 1 {
			return old, nil
		}
		return next, nil
	})
	if err != nil || !old.draining || !old.purged || !old.closed || opens != 2 || !next.committed {
		t.Fatal("retirement not resumed", err, opens)
	}
}
