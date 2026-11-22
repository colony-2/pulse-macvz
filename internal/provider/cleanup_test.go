package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/colony-2/pulse-macvz/internal/backend"
	api "github.com/colony-2/pulse-provider-api"
)

type busyDeleteBackend struct {
	*fake
	busy string
}

func (b *busyDeleteBackend) Delete(ctx context.Context, name string) error {
	if name == b.busy {
		return backend.ErrBusy
	}
	return b.fake.Delete(ctx, name)
}

func TestBusyCleanupRetainsReservationAndAllowsOtherCleanup(t *testing.T) {
	p, f := slotsFixture(t, 2)
	warmSlots(t, p)
	for _, id := range []string{"one", "two"} {
		out, err := p.Submit(t.Context(), []api.Launch{launch(id)})
		if err != nil || out[0].Status != api.Accepted {
			t.Fatal(out, err)
		}
	}
	one, two := p.Store.data.Records["one"], p.Store.data.Records["two"]
	for _, r := range []*Record{one, two} {
		v := f.vms[r.Name]
		v.Status = "failed"
		v.Conditions = []backend.Condition{{Type: "running", State: "false"}}
		f.vms[r.Name] = v
	}
	b := &busyDeleteBackend{fake: f, busy: one.Name}
	p.Backend = b
	vs, _ := b.List(t.Context())
	if err := p.reconcileRecords(t.Context(), vs); err != nil {
		t.Fatal("transient cleanup escaped reconciliation", err)
	}
	if one.Phase != "deleting" || two.Phase != "done" || !p.slotBusy(one.SlotID) {
		t.Fatal("cleanup dropped reservation or blocked unrelated VM", one.Phase, two.Phase)
	}
	b.busy = ""
	vs, _ = b.List(t.Context())
	if err := p.reconcileRecords(t.Context(), vs); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Get(t.Context(), one.Name); !backend.IsStatus(err, 404) || one.Phase != "done" {
		t.Fatal("cleanup never retried", err, one.Phase)
	}
}

func TestBusyCleanupSentinelSurvivesDiagnosticWrapping(t *testing.T) {
	if !errors.Is(backendError("delete VM vm launch=id", backend.ErrBusy), backend.ErrBusy) {
		t.Fatal("lost cleanup cause")
	}
}

type busyDockerDeleteBackend struct {
	*dockerFake
	busy string
}

func (b *busyDockerDeleteBackend) Delete(ctx context.Context, name string) error {
	if name == b.busy {
		return backend.ErrBusy
	}
	return b.dockerFake.Delete(ctx, name)
}

func TestBusyPairCleanupRetainsSlotAndPortUntilBothMembersDeleted(t *testing.T) {
	p, f := dockerFixture(t, false)
	pair := readyDocker(t, p)
	for _, member := range []struct{ name, uid string }{{pair.Primary.Name, pair.PrimaryUID}, {pair.Engine.Name, pair.EngineUID}} {
		if err := f.Stop(t.Context(), member.name, member.uid); err != nil {
			t.Fatal(err)
		}
	}
	pair.Phase = "reclaiming"
	b := &busyDockerDeleteBackend{dockerFake: f, busy: pair.Primary.Name}
	p.Backend = b
	if err := p.reclaimPair(t.Context(), pair); err != nil {
		t.Fatal(err)
	}
	if pair.Phase != "reclaiming" || !p.slotBusy(pair.SlotID) {
		t.Fatal("busy cleanup released reservation")
	}
	if _, err := b.Get(t.Context(), pair.Engine.Name); err != nil {
		t.Fatal("busy primary cleanup lost companion", err)
	}
	b.busy = ""
	if err := p.reclaimPair(t.Context(), pair); err != nil {
		t.Fatal(err)
	}
	if pair.Phase != "gone" {
		t.Fatal("pair cleanup did not finish", pair.Phase)
	}
}
