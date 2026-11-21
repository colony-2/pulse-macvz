package provider

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/colony-2/pulse-macvz/internal/backend"
	"github.com/colony-2/pulse-macvz/internal/config"
	compute "github.com/colony-2/pulse-provider-api"
)

type preparingBackend struct {
	*fake
	ready bool
	err   error
	calls int
}

type observingBaseBackend struct {
	*fake
	calls int
	err   error
}

func (b *observingBaseBackend) ObserveBasePreparation(context.Context, string, string, string) error {
	b.calls++
	return b.err
}

func TestObserveBaseRequiresMatchingIdentityAndReportsGuestFailure(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		p, f := fixture(t)
		b := &observingBaseBackend{fake: f, err: errors.New("guest reboot loop")}
		p.Backend = b
		r := p.Store.data.Images[f.baseVM.Name]
		r.Phase = "preparing"
		f.baseVM.PowerState = "running"
		f.baseVM.Conditions = []backend.Condition{{Type: "running", State: "true"}}
		if mismatch {
			f.baseVM.UID = "replacement"
		}
		err := p.reconcileImages(t.Context())
		if mismatch {
			if b.calls != 0 || r.Phase != "quarantine" {
				t.Fatal("observed replacement VM", b.calls, r.Phase)
			}
		} else if b.calls != 1 || err != nil || !errors.Is(p.preparationCauses["image:"+r.Name].cause, b.err) {
			t.Fatal("base failure must be scoped without blocking other capacity", b.calls, err)
		}
	}
}

func (b *preparingBackend) PrepareSource(context.Context, string) (bool, error) {
	b.calls++
	return b.ready, b.err
}

func TestPendingSourceDoesNotBlockReadyAllocation(t *testing.T) {
	p, f := slotsFixture(t, 1)
	warmSlots(t, p)
	b := &preparingBackend{fake: f}
	p.Backend = b
	cold := p.Config.Bases[0]
	cold.Name = "macos-cold"
	cold.Image = "example.test/cold@sha256:" + strings.Repeat("b", 64)
	p.Config.Bases = append(p.Config.Bases, cold)
	p.Config.Slots = append(p.Config.Slots, config.SlotGroup{Name: "cold", Base: cold.Name, Count: 1, CPU: 4, MemoryMiB: 8192})
	creates := f.creates
	if err := p.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if b.calls == 0 || f.creates != creates {
		t.Fatal("pending registry source was dispatched")
	}
	out, err := p.Submit(t.Context(), []compute.Launch{launch("during-import")})
	if err != nil || out[0].Status != compute.Accepted {
		t.Fatal("pending source blocked ready capacity", out, err)
	}
	b.err = errors.New("registry unavailable")
	if err := p.Reconcile(t.Context()); !errors.Is(err, b.err) {
		t.Fatal("registry failure not reported", err)
	}
	b.err, b.ready = nil, true
	if err := p.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if p.Store.data.Images[p.baseImageName(cold.Name)] == nil {
		t.Fatal("ready source did not advance to creation")
	}
}

func TestPrepareBaseOnceAndReuseSourcesAfterDrain(t *testing.T) {
	p, f := fixture(t)
	f.baseVM = nil
	p.Store.data.Images = map[string]*ImageRecord{}
	builds := 0
	p.Setup = func(context.Context) ([]byte, error) { builds++; return []byte("setup"), nil }
	for range 10 {
		if err := p.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if builds != 1 || f.creates != 3 {
		t.Fatalf("base prepared %d times; %d native creations", builds, f.creates)
	}
	for _, s := range p.Store.data.Slots {
		if s.Phase != "ready" {
			t.Fatal(s.Phase)
		}
	}
	base := p.Store.data.Images[config.BaseImageName(p.Config.OwnerID, p.Config.Bases[0].Name)]
	for _, s := range p.Store.data.Slots {
		if f.vms[s.Name].Image != base.LocalName {
			t.Fatal("slot did not use shared prepared base")
		}
	}
	p.Config.Slots[0].Count = 0
	if err := p.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(p.Store.data.Slots) != 0 || len(f.vms) != 3 {
		t.Fatal("drain removed reusable sources")
	}
	p.Config.C2JVersion = "different-version"
	p.Config.Slots[0].Count = 2
	for range 3 {
		if err := p.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if builds != 1 || f.creates != 3 {
		t.Fatal("existing source was rebuilt due to release selection")
	}
}
func TestJobStartupDoesNotCarryInstallerOrCachedIdentity(t *testing.T) {
	p, f := slotsFixture(t, 1)
	warmSlots(t, p)
	out, err := p.Submit(t.Context(), []compute.Launch{launch("new")})
	if err != nil || out[0].Status != compute.Accepted {
		t.Fatal(out, err)
	}
	script := f.vms[p.Store.data.Records["new"].Name].StartupScript.Content
	for _, bad := range []string{"PULSE_BUNDLE", "flake.nix", "install-guest.sh", p.Store.data.Slots["small-0001"].BootID} {
		if bad != "" && strings.Contains(script, bad) {
			t.Fatalf("startup contains %s", bad)
		}
	}
	if !strings.Contains(script, "/var/db/pulse-tart-template/ready") {
		t.Fatal("missing snapshot restore check")
	}
}

func TestSourceStoppedUsesObservedState(t *testing.T) {
	for _, tc := range []struct {
		power, running, local string
		want                  bool
	}{
		{"running", "false", "local-base", true}, // Orchard keeps desired power after shutdown.
		{"stopped", "false", "local-base", true},
		{"suspended", "false", "local-base", false},
		{"stopped", "true", "local-base", false},
		{"stopped", "", "local-base", false},
		{"stopped", "false", "", false},
	} {
		v := backend.VM{PowerState: tc.power, LocalName: tc.local, Conditions: []backend.Condition{{Type: "running", State: tc.running}}}
		if sourceStopped(v) != tc.want {
			t.Fatalf("source stopped: %+v", tc)
		}
	}
}

func TestDrainClearsFailedSourceProbeFallback(t *testing.T) {
	p, _ := slotsFixture(t, 1)
	warmSlots(t, p)
	for _, s := range p.Store.data.Slots {
		s.Phase = "fallback"
		s.UID = ""
	}
	p.Config.Slots[0].Count = 0
	if err := p.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(p.Store.data.Slots) != 0 {
		t.Fatal("drain retained a deleted source probe")
	}
}

func TestRetireOnlyUnusedFailedSupersededBase(t *testing.T) {
	for _, mode := range []string{"expired", "ready", "running", "referenced", "identity-changed", "required"} {
		t.Run(mode, func(t *testing.T) {
			p, f := dockerFixture(t, false)
			spec := p.sourceSpec(p.Config.Bases[0], 4, 8192)
			spec.Name = "old-ordinary-base"
			if mode == "required" {
				spec.Name = p.baseImageName(p.Config.Bases[0].Name)
			}
			vm := backend.VM{Spec: spec, UID: "old-uid", Status: "failed", PowerState: "stopped", LocalName: spec.Name, Conditions: []backend.Condition{{Type: "running", State: "false"}}}
			record := &ImageRecord{Name: spec.Name, UID: vm.UID, NativeHash: p.Store.hash(normalized(spec)), LocalName: spec.Name, Kind: "base", Phase: "preparing"}
			if mode == "ready" {
				record.Phase = "ready"
			}
			if mode == "running" {
				vm.Status = "running"
				vm.PowerState = "running"
				vm.Conditions = []backend.Condition{{Type: "running", State: "true"}}
			}
			if mode == "identity-changed" {
				vm.UID = "replacement"
			}
			f.vms[spec.Name] = vm
			p.Store.data.Images[spec.Name] = record
			inventory := []backend.VM{vm}
			if mode == "referenced" {
				inventory = append(inventory, backend.VM{Spec: backend.Spec{Name: "accepted-job", Image: spec.Name}})
			}
			if err := p.retireObsoleteBuilders(t.Context(), inventory); err != nil {
				t.Fatal(err)
			}
			_, exists := p.Store.data.Images[spec.Name]
			if exists != (mode != "expired") {
				t.Fatal("incorrect retirement", mode, exists)
			}
		})
	}
}

func TestImageStatusDistinguishesSupersededBaseFromCompanion(t *testing.T) {
	p, _ := dockerFixture(t, false)
	old := "old-ordinary-base"
	current := p.baseImageName(p.Config.Bases[0].Name)
	companion := p.engineBaseName(p.Config.Engine("engine"))
	for _, name := range []string{old, current, companion} {
		p.Store.data.Images[name] = &ImageRecord{Name: name, Kind: "base", Phase: "preparing"}
	}
	images, err := p.Images()
	if err != nil {
		t.Fatal(err)
	}
	for _, image := range images {
		if image.Name == old && (!image.Obsolete || image.Companion) {
			t.Fatal("old base labeled as companion", image)
		}
		if image.Name == current && (image.Obsolete || image.Companion || image.Base == "") {
			t.Fatal("current base mislabeled", image)
		}
		if image.Name == companion && (image.Obsolete || !image.Companion) {
			t.Fatal("companion mislabeled", image)
		}
	}
}
