package servercli

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	api "github.com/colony-2/pulse-provider-api"
	tart "github.com/colony-2/pulse-tart"
)

type maintenanceFake struct {
	draining, closed, purged, keepJob bool
	steps                             int
}

func (*maintenanceFake) Check(context.Context) error       { return nil }
func (f *maintenanceFake) Reconcile(context.Context) error { f.steps++; return nil }
func (f *maintenanceFake) Drain(context.Context) error     { f.draining = true; return nil }
func (f *maintenanceFake) Close() error                    { f.closed = true; return nil }
func (f *maintenanceFake) PurgeImages(context.Context) error {
	if !f.draining || f.steps < 3 || f.keepJob {
		return errors.New("attempted purge while work was active")
	}
	f.purged = true
	return nil
}
func (f *maintenanceFake) Slots() ([]tart.SlotStatus, error) {
	if f.draining {
		return nil, nil
	}
	return []tart.SlotStatus{{ID: "standard-0001", Phase: "ready"}}, nil
}
func (*maintenanceFake) Images() ([]tart.ImageStatus, error) {
	return []tart.ImageStatus{{Name: "base", Phase: "ready"}}, nil
}
func (*maintenanceFake) DockerAllocations() ([]tart.DockerStatus, error) { return nil, nil }
func (f *maintenanceFake) List(context.Context, api.ListRequest) (api.ListResponse, error) {
	if f.draining && (f.keepJob || f.steps < 3) {
		return api.ListResponse{Items: []api.Instance{{ID: "accepted-job"}}}, nil
	}
	return api.ListResponse{}, nil
}

func maintenanceOptions(t *testing.T) serverOptions {
	t.Helper()
	c, err := (capacityOptions{macOS: 1}).config("test", "", "", 0, 0)
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
