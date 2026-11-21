package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colony-2/pulse-macvz/internal/backend"
	api "github.com/colony-2/pulse-provider-api"
)

func TestDiagnosticObservationCausesAndAuthority(t *testing.T) {
	for _, op := range []string{"inventory", "template", "reservation inventory"} {
		t.Run(op, func(t *testing.T) {
			p, _ := slotsFixture(t, 1)
			warmSlots(t, p)
			d := diagnosticWrap(p)
			switch op {
			case "inventory":
				d.failListAt = 1
			case "reservation inventory":
				d.failListAt = 2
			case "template":
				d.faults["Get:"+p.Store.data.Slots["small-0001"].Name] = errors.New(diagnosticRaw)
			}
			r := diagnosticSubmit(t, p, launch("fault"), api.Unavailable, diagnosticRaw)
			if strings.Contains(r.Reason, "count=0 limit=2") && op == "inventory" {
				t.Fatal("invented count", r)
			}
			switch op {
			case "inventory":
				diagnosticCalls(t, d, "List")
			case "template":
				diagnosticCalls(t, d, "List", "Get:template")
			case "reservation inventory":
				diagnosticCalls(t, d, "List", "Get:template", "List")
			}
		})
	}
	p, _ := fixture(t)
	d := diagnosticWrap(p)
	raw := errors.New(diagnosticRaw)
	d.faults["List"] = raw
	if e := p.Check(t.Context()); !errors.Is(e, raw) || !errors.Is(e, ErrBackend) {
		t.Fatal(e)
	}
	if _, e := p.List(t.Context(), api.ListRequest{}); !errors.Is(e, raw) {
		t.Fatal(e)
	}
	if e := p.Reconcile(t.Context()); !errors.Is(e, raw) {
		t.Fatal(e)
	}
}
func TestDiagnosticDispatchAndSequentialSaveFailures(t *testing.T) {
	for _, mode := range []string{"intent", "native", "native-save", "identity", "identity-save", "success-save", "accepted"} {
		t.Run(mode, func(t *testing.T) {
			p, _ := slotsFixture(t, 1)
			warmSlots(t, p)
			d := diagnosticWrap(p)
			raw := errors.New(diagnosticRaw)
			saveRaw := errors.New("journal failed\n\"second cause\"")
			if mode == "intent" {
				diagnosticSaveFault(p, "create", saveRaw)
			}
			if strings.HasPrefix(mode, "native") {
				d.faults["Create"] = raw
			}
			if strings.HasSuffix(mode, "-save") {
				d.onCreate = func() { diagnosticSaveFault(p, "rename", saveRaw) }
			}
			if strings.HasPrefix(mode, "identity") {
				d.Backend = &diagnosticMalformedBackend{Backend: d.Backend}
			}
			want := api.Unknown
			if mode == "accepted" {
				want = api.Accepted
			}
			l := launch("dispatch")
			r := diagnosticSubmit(t, p, l, want)
			if strings.HasPrefix(mode, "native") && !strings.Contains(r.Reason, diagnosticRaw) {
				t.Fatal(r)
			}
			if mode == "intent" || strings.HasSuffix(mode, "-save") {
				if !strings.Contains(r.Reason, saveRaw.Error()) {
					t.Fatal(r)
				}
			}
			if strings.HasPrefix(mode, "identity") && !strings.Contains(r.Reason, "identity missing or mismatched") {
				t.Fatal(r)
			}
			if mode == "accepted" && r.Reason != "" {
				t.Fatal(r)
			}
			if mode == "intent" {
				diagnosticCalls(t, d, "List", "Get:template", "List")
			} else {
				diagnosticCalls(t, d, "List", "Get:template", "List", "Create:job")
			}
			before := len(d.calls)
			replayWant := want
			if mode == "success-save" {
				replayWant = api.Unknown
			} // Broken store retains today's unknown replay.
			diagnosticSubmit(t, p, l, replayWant)
			if len(d.calls) != before {
				t.Fatal("replay dispatched")
			}
			if mode == "native" || mode == "identity" {
				p.Close()
				store, e := OpenStore(p.Config.StateDir, p.Config.Scope())
				if e != nil {
					t.Fatal(e)
				}
				p.Store = store
				r = diagnosticSubmit(t, p, l, api.Unknown, "historical cause unavailable")
				if strings.Contains(r.Reason, diagnosticRaw) {
					t.Fatal("persisted diagnostic")
				}
				if len(d.calls) != before {
					t.Fatal("reopen replay dispatched")
				}
			}
		})
	}
}

type diagnosticMalformedBackend struct{ Backend }

func (d *diagnosticMalformedBackend) Create(ctx context.Context, s backend.Spec) (backend.VM, error) {
	v, e := d.Backend.Create(ctx, s)
	v.UID = ""
	return v, e
}
func TestDiagnosticTemplateQuarantineSaveFailure(t *testing.T) {
	p, f := slotsFixture(t, 1)
	warmSlots(t, p)
	s := p.Store.data.Slots["small-0001"]
	v := f.vms[s.Name]
	v.UID = "replacement"
	f.vms[s.Name] = v
	raw := errors.New("quarantine save failed")
	diagnosticSaveFault(p, "rename", raw)
	d := diagnosticWrap(p)
	diagnosticSubmit(t, p, launch("mismatch"), api.Unavailable, "template identity mismatch; suspension unknown", raw.Error())
	diagnosticCalls(t, d, "List", "Get:template")
	if s.Phase != "quarantine" {
		t.Fatal(s.Phase)
	}
}
func TestDiagnosticDockerReadinessHandoffAndSave(t *testing.T) {
	for _, mode := range []string{"readiness-error", "readiness-marker", "handoff-error", "handoff-marker", "handoff-save", "marker-save", "intent", "accepted", "identity-save", "get-404", "get-error"} {
		t.Run(mode, func(t *testing.T) {
			p, _ := dockerFixture(t, false)
			pair := readyDocker(t, p)
			d := diagnosticWrap(p)
			raw := errors.New(diagnosticRaw)
			saveRaw := errors.New("Docker save failure\n\"second cause\"")
			want := api.Unknown
			switch mode {
			case "readiness-error":
				d.readinessErr = raw
				want = api.Unavailable
			case "readiness-marker":
				d.missingReadiness = true
				want = api.Unavailable
			case "handoff-error":
				d.handoffErr = raw
			case "handoff-marker":
				d.missingHandoff = true
			case "handoff-save":
				d.handoffErr = raw
				d.onExec = func(ready bool) {
					if !ready {
						diagnosticSaveFault(p, "rename", saveRaw)
					}
				}
			case "marker-save":
				d.missingHandoff = true
				d.onExec = func(ready bool) {
					if !ready {
						diagnosticSaveFault(p, "rename", saveRaw)
					}
				}
			case "intent":
				diagnosticSaveFault(p, "create", saveRaw)
			case "identity-save":
				v := p.Backend.(*diagnosticBackend).Backend.(*dockerFake).vms[pair.Primary.Name]
				v.UID = "replacement"
				p.Backend.(*diagnosticBackend).Backend.(*dockerFake).vms[v.Name] = v
				diagnosticSaveFault(p, "rename", saveRaw)
				want = api.Unavailable
			case "get-404":
				d.faults["Get:"+pair.Primary.Name] = &backend.Error{Code: 404}
				want = api.NoCapacity
			case "get-error":
				d.faults["Get:"+pair.Primary.Name] = raw
				want = api.Unavailable
			case "accepted":
				want = api.Accepted
			}
			r := diagnosticSubmit(t, p, dockerLaunch(), want)
			if mode == "accepted" && r.Reason != "" {
				t.Fatal(r)
			}
			if strings.HasSuffix(mode, "error") || mode == "handoff-save" {
				if !strings.Contains(r.Reason, diagnosticRaw) {
					t.Fatal(r)
				}
			}
			if mode == "readiness-marker" && !strings.Contains(r.Reason, "missing PULSE_DOCKER_PRIMARY_READY") {
				t.Fatal(r)
			}
			if mode == "handoff-marker" && !strings.Contains(r.Reason, "missing PULSE_HANDOFF_OK") {
				t.Fatal(r)
			}
			if mode == "handoff-save" || mode == "identity-save" || mode == "marker-save" || mode == "intent" {
				if !strings.Contains(r.Reason, saveRaw.Error()) {
					t.Fatal(r)
				}
			}
			if mode == "identity-save" && !strings.Contains(r.Reason, "identity mismatch") {
				t.Fatal(r)
			}
			before := len(d.calls)
			if want == api.Unknown || want == api.Accepted {
				diagnosticSubmit(t, p, dockerLaunch(), want)
				if len(d.calls) != before {
					t.Fatal("handoff replay")
				}
			}
			switch mode {
			case "accepted", "handoff-error", "handoff-marker", "handoff-save", "marker-save":
				diagnosticCalls(t, d, "Get:primary", "Get:engine", "Exec:readiness", "Exec:handoff")
			case "readiness-error", "readiness-marker", "intent":
				diagnosticCalls(t, d, "Get:primary", "Get:engine", "Exec:readiness")
			case "identity-save", "get-error":
				diagnosticCalls(t, d, "Get:primary")
			}
		})
	}
}
func TestDiagnosticPreparationKeepsNilAndExpires(t *testing.T) {
	p, _ := dockerFixture(t, false)
	pair := readyDocker(t, p)
	pair.Phase = "configuring"
	d := diagnosticWrap(p)
	d.readinessErr = errors.New(diagnosticRaw)
	if e := p.Reconcile(t.Context()); e != nil {
		t.Fatal("new stopping return", e)
	}
	diagnosticSubmit(t, p, dockerLaunch(), api.NoCapacity, "earlier preparation observation", diagnosticRaw)
	// A changed phase or configuration must invalidate the previous observation.
	pair.Phase = "primary-dispatch"
	p.prunePreparation()
	if len(p.preparationCauses) != 0 {
		t.Fatal("phase retained stale evidence")
	}
	p.preparation("pair:"+pair.ID, errors.New(diagnosticRaw))
	p.Config.Slots[0].Count++
	p.prunePreparation()
	if len(p.preparationCauses) != 0 {
		t.Fatal("config retained stale evidence")
	}
	p.preparation("pair:"+pair.ID, errors.New(diagnosticRaw))
	p.preparation("pair:"+pair.ID, nil)
	if len(p.preparationCauses) != 0 {
		t.Fatal("success retained evidence")
	}
	p.preparation("pair:"+pair.ID, errors.New(diagnosticRaw))
	pair.PrimaryUID = "changed"
	p.prunePreparation()
	if len(p.preparationCauses) != 0 {
		t.Fatal("identity retained evidence")
	}
	p.preparation("pair:"+pair.ID, errors.New(diagnosticRaw))
	delete(p.Store.data.Pairs, pair.ID)
	p.prunePreparation()
	if len(p.preparationCauses) != 0 {
		t.Fatal("removal retained evidence")
	}
	b, _ := os.ReadFile(filepath.Join(p.Config.StateDir, "state.json"))
	if strings.Contains(string(b), diagnosticRaw) {
		t.Fatal("preparation cause persisted")
	}
}

func TestDiagnosticLongReturnedErrorsAndPreparationCreate(t *testing.T) {
	p, _ := slotsFixture(t, 1)
	warmSlots(t, p)
	d := diagnosticWrap(p)
	raw := diagnosticRaw + strings.Repeat("returned-text", 2000)
	d.faults["Create"] = errors.New(raw)
	r := diagnosticSubmit(t, p, launch("long"), api.Unknown, raw)
	if strings.Contains(r.Reason, "[truncated") {
		t.Fatal("existing returned text truncated")
	}
	p2, _ := fixture(t)
	d2 := diagnosticWrap(p2)
	d2.faults["Create"] = errors.New(diagnosticRaw)
	if e := p2.Reconcile(t.Context()); e != nil {
		t.Fatal("new stopping preparation error", e)
	}
	diagnosticSubmit(t, p2, launch("unprepared"), api.NoCapacity, "earlier preparation observation", diagnosticRaw)
	for _, key := range []string{"slot:small-0001", "slot:small-0002"} {
		if obs, ok := p2.preparationCauses[key]; !ok || !strings.Contains(obs.cause.Error(), diagnosticRaw) {
			t.Fatal("missing latest preparation cause", key)
		}
	}
	p2.Close()
	if len(p2.preparationCauses) != 0 {
		t.Fatal("close retained causes")
	}
}

func TestDiagnosticPreparationReturnedOperations(t *testing.T) {
	for _, op := range []string{"Get", "Events", "Suspend"} {
		t.Run(op, func(t *testing.T) {
			p, _ := slotsFixture(t, 1)
			d := diagnosticWrap(p)
			raw := errors.New(diagnosticRaw)
			key := op
			if op == "Get" {
				key = "Get:" + p.Store.data.Slots["small-0001"].Name
			}
			d.faults[key] = raw
			e := p.reconcileSlots(t.Context())
			if !errors.Is(e, raw) || !errors.Is(e, ErrBackend) || !strings.Contains(e.Error(), op) {
				t.Fatal(e)
			}
			switch op {
			case "Get":
				diagnosticCalls(t, d, "Get:template")
			case "Events":
				diagnosticCalls(t, d, "Get:template", "Events:template")
			case "Suspend":
				diagnosticCalls(t, d, "Get:template", "Events:template", "Suspend:template")
			}
		})
	}
	p, f := fixture(t)
	delete(p.Store.data.Images, p.baseImageName(p.Config.Bases[0].Name))
	f.baseVM = nil
	raw := errors.New(diagnosticRaw)
	p.Setup = func(context.Context) ([]byte, error) { return nil, raw }
	d := diagnosticWrap(p)
	if e := p.Reconcile(t.Context()); !errors.Is(e, raw) || !strings.Contains(e.Error(), "prepare base setup") {
		t.Fatal(e)
	}
	diagnosticCalls(t, d, "List", "Get:base", "List")
}
func TestDiagnosticRoomRequestErrorRetainsEarlierConstraints(t *testing.T) {
	p, _ := slotsFixture(t, 1)
	warmSlots(t, p)
	p.Store.data.Slots["small-0001"].Phase = "quarantine"
	d := diagnosticWrap(p)
	d.failListAt = 2
	diagnosticSubmit(t, p, launch("room"), api.Unavailable, "phase=quarantine", "room request", diagnosticRaw)
	diagnosticCalls(t, d, "List", "List")
	if p.preferredGroup != "small" {
		t.Fatal("room preference changed")
	}
}
