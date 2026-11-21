package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse-macvz/internal/backend"
	"github.com/colony-2/pulse-macvz/internal/config"
	api "github.com/colony-2/pulse-provider-api"
)

const diagnosticRaw = "FIXTURE provider says: unavailable\nrequest=\"original\""

type diagnosticBackend struct {
	Backend
	templateBackend
	dockerBackend
	calls                            []string
	failListAt                       int
	lists                            int
	faults                           map[string]error
	onCreate                         func()
	onExec                           func(bool)
	readinessErr, handoffErr         error
	missingReadiness, missingHandoff bool
}

func diagnosticRole(name string) string {
	if strings.Contains(name, "-slot-") {
		return "template"
	}
	if strings.HasSuffix(name, "-base") {
		return "base"
	}
	if strings.HasSuffix(name, "-primary") {
		return "primary"
	}
	if strings.HasSuffix(name, "-engine") {
		return "engine"
	}
	return "job"
}
func (d *diagnosticBackend) List(ctx context.Context) ([]backend.VM, error) {
	d.calls = append(d.calls, "List")
	d.lists++
	if d.failListAt == d.lists {
		return nil, errors.New(diagnosticRaw)
	}
	if e := d.faults["List"]; e != nil {
		return nil, e
	}
	return d.Backend.List(ctx)
}
func (d *diagnosticBackend) Get(ctx context.Context, n string) (backend.VM, error) {
	d.calls = append(d.calls, "Get:"+diagnosticRole(n))
	if e := d.faults["Get:"+n]; e != nil {
		return backend.VM{}, e
	}
	return d.Backend.Get(ctx, n)
}
func (d *diagnosticBackend) Create(ctx context.Context, s backend.Spec) (backend.VM, error) {
	d.calls = append(d.calls, "Create:"+diagnosticRole(s.Name))
	if d.onCreate != nil {
		d.onCreate()
	}
	if e := d.faults["Create"]; e != nil {
		return backend.VM{}, e
	}
	return d.Backend.Create(ctx, s)
}
func (d *diagnosticBackend) Events(ctx context.Context, n string) ([]backend.Event, error) {
	d.calls = append(d.calls, "Events:"+diagnosticRole(n))
	if e := d.faults["Events"]; e != nil {
		return nil, e
	}
	return d.templateBackend.Events(ctx, n)
}
func (d *diagnosticBackend) Suspend(ctx context.Context, n, u string) error {
	d.calls = append(d.calls, "Suspend:template")
	if e := d.faults["Suspend"]; e != nil {
		return e
	}
	return d.templateBackend.Suspend(ctx, n, u)
}
func (d *diagnosticBackend) Exec(ctx context.Context, n, u string, s []byte) ([]byte, error) {
	ready := strings.Contains(string(s), "PULSE_DOCKER_PRIMARY_READY")
	role := "handoff"
	if ready {
		role = "readiness"
	}
	d.calls = append(d.calls, "Exec:"+role)
	if d.onExec != nil {
		d.onExec(ready)
	}
	if ready {
		if d.readinessErr != nil {
			return nil, d.readinessErr
		}
		if d.missingReadiness {
			return []byte("UNRELATED-GUEST-OUTPUT"), nil
		}
	} else {
		if d.handoffErr != nil {
			return nil, d.handoffErr
		}
		if d.missingHandoff {
			return []byte("UNRELATED-GUEST-OUTPUT"), nil
		}
	}
	return d.dockerBackend.Exec(ctx, n, u, s)
}
func diagnosticWrap(p *Provider) *diagnosticBackend {
	d := &diagnosticBackend{Backend: p.Backend, faults: map[string]error{}}
	d.templateBackend, _ = p.Backend.(templateBackend)
	d.dockerBackend, _ = p.Backend.(dockerBackend)
	p.Backend = d
	return d
}
func diagnosticSubmit(t *testing.T, p *Provider, l api.Launch, want api.Status, parts ...string) api.Submission {
	t.Helper()
	out, e := p.Submit(t.Context(), []api.Launch{l})
	if e != nil || len(out) != 1 || out[0].Status != want {
		t.Fatalf("status=%v method=%v", out, e)
	}
	for _, part := range parts {
		if !strings.Contains(out[0].Reason, part) {
			t.Errorf("missing %q in %q", part, out[0].Reason)
		}
	}
	for _, secret := range []string{"LEASE-SENTINEL", "PULSE_ENVELOPE", "UNRELATED-GUEST-OUTPUT"} {
		if strings.Contains(out[0].Reason, secret) {
			t.Errorf("diagnostic captured %s", secret)
		}
	}
	return out[0]
}
func diagnosticCalls(t *testing.T, d *diagnosticBackend, want ...string) {
	t.Helper()
	if strings.Join(d.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls=%v want=%v", d.calls, want)
	}
}

func TestDiagnosticAdmissionReservationAndRequestFit(t *testing.T) {
	t.Run("reservation budget", func(t *testing.T) {
		p, _ := slotsFixture(t, 1)
		warmSlots(t, p)
		diagnosticSubmit(t, p, launch("first"), api.Accepted)
		d := diagnosticWrap(p)
		diagnosticSubmit(t, p, launch("next"), api.NoCapacity, "journal reservation occupancy=1 configured slot budget=1")
		diagnosticCalls(t, d)
	})
	for _, tc := range []struct {
		name   string
		change func(*api.Launch)
		status api.Status
		part   string
	}{
		{"base", func(l *api.Launch) { l.Base = "ubuntu*" }, api.Unsupported, "does not match selector"},
		{"capability", func(l *api.Launch) { l.Capabilities = []string{"absent"} }, api.Unsupported, "lacks a requested capability"},
		{"cpu", func(l *api.Launch) { l.CPUMillis = 4001 }, api.Unsupported, "CPU request=5 limit=4"},
		{"memory", func(l *api.Launch) { l.MemoryBytes = (8192 << 20) + 1 }, api.Unsupported, "memory MiB request=8193 limit=8192"},
		{"invalid", func(l *api.Launch) { l.Job.JobDB = "https://user:PRIVATE@jobs.test" }, api.Rejected, "validation failed"},
		{"unsupported semantics", func(l *api.Launch) { l.ScratchBytes = 1 }, api.Unsupported, "semantics unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := slotsFixture(t, 1)
			warmSlots(t, p)
			d := diagnosticWrap(p)
			l := launch("request")
			tc.change(&l)
			diagnosticSubmit(t, p, l, tc.status, tc.part)
			diagnosticCalls(t, d)
		})
	}
}
func TestDiagnosticSlotReadinessAndShortCircuits(t *testing.T) {
	for _, phase := range []string{"missing", "preparing", "quarantine", "stale", "reserved"} {
		t.Run(phase, func(t *testing.T) {
			p, _ := slotsFixture(t, 1)
			warmSlots(t, p)
			s := p.Store.data.Slots["small-0001"]
			switch phase {
			case "missing":
				delete(p.Store.data.Slots, s.ID)
			case "stale":
				s.ConfigHash = "stale"
			case "reserved":
				p.Store.data.Records["existing"] = &Record{Phase: "active", SlotID: s.ID}
				p.Config.Slots[0].Count = 2
				delete(p.Store.data.Slots, "small-0002")
			default:
				s.Phase = phase
			}
			d := diagnosticWrap(p)
			r := diagnosticSubmit(t, p, launch("blocked"), api.NoCapacity)
			if strings.Contains(strings.Join(d.calls, ","), "Get:") {
				t.Fatal("skipped template read", d.calls)
			}
			if phase == "reserved" && !strings.Contains(r.Reason, "reservation occupied") {
				t.Fatal(r)
			}
			if (phase == "missing" || phase == "preparing" || phase == "quarantine") && strings.Contains(r.Reason, "reservation occupied") {
				t.Fatal("invented occupancy", r)
			}
		})
	}
}
func TestDiagnosticMixedCandidatesAndEarlyReadFailure(t *testing.T) {
	for _, readFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(readFailure), func(t *testing.T) {
			p, f := slotsFixture(t, 3)
			warmSlots(t, p)
			diagnosticSubmit(t, p, launch("prior"), api.Accepted)
			second := p.Store.data.Slots["small-0002"]
			f.vms["foreign"] = backend.VM{Spec: backend.Spec{Name: "foreign", Image: second.LocalName}, UID: "foreign", Conditions: []backend.Condition{{Type: "running", State: "false"}}}
			third := p.Store.data.Slots["small-0003"]
			v := f.vms[third.Name]
			v.PowerState = "running"
			f.vms[third.Name] = v
			d := diagnosticWrap(p)
			want := api.NoCapacity
			if readFailure {
				d.faults["Get:"+third.Name] = errors.New(diagnosticRaw)
				want = api.Unavailable
			}
			r := diagnosticSubmit(t, p, launch("mixed"), want, "reservation occupied", "source-referencing clone")
			if readFailure {
				if !strings.Contains(r.Reason, diagnosticRaw) {
					t.Fatal(r)
				}
				diagnosticCalls(t, d, "List", "Get:template")
			} else {
				if !strings.Contains(r.Reason, "suspension invalid; quarantined") || third.Phase != "quarantine" {
					t.Fatal(r)
				}
				diagnosticCalls(t, d, "List", "Get:template", "List")
			}
		})
	}
}
func TestDiagnosticConfiguredCountsAndPhysicalCeiling(t *testing.T) {
	for _, tc := range []struct {
		name          string
		counts        config.VMCounts
		used          map[string]string
		spec          backend.Spec
		part, skipped string
	}{
		{"mac", config.VMCounts{MacOS: diagnosticCount(0)}, map[string]string{}, backend.Spec{Name: "new", OS: "darwin"}, "macOS count=1 limit=0", "configured Linux"},
		{"linux", config.VMCounts{Linux: diagnosticCount(0)}, map[string]string{}, backend.Spec{Name: "new", OS: "linux"}, "Linux count=1 limit=0", "configured total"},
		{"total", config.VMCounts{MacOS: diagnosticCount(2), Linux: diagnosticCount(2), Total: diagnosticCount(1)}, map[string]string{"warm": "linux"}, backend.Spec{Name: "new", OS: "darwin"}, "total count=2 limit=1", "physical macOS ceiling"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := fixture(t)
			p.Config.VMCounts = tc.counts
			p.diagnostic = &diagnostics{}
			if p.fitsJobCounts(tc.used, tc.spec) {
				t.Fatal("counts passed")
			}
			r := p.submission("x", api.NoCapacity)
			if !strings.Contains(r.Reason, tc.part) || strings.Contains(r.Reason, tc.skipped) {
				t.Fatal(r)
			}
		})
	}
	p, f := fixture(t)
	d := diagnosticWrap(p)
	p.diagnostic = &diagnostics{}
	p.Config.VMCounts = config.VMCounts{MacOS: diagnosticCount(0)}
	ok, e := p.canReserveJobs(t.Context(), backend.Spec{Name: "new", OS: "darwin"})
	if ok || e != nil {
		t.Fatal(ok, e)
	}
	diagnosticCalls(t, d)
	p.Config.VMCounts = config.VMCounts{MacOS: diagnosticCount(2), Linux: diagnosticCount(4), Total: diagnosticCount(8)}
	p.diagnostic = &diagnostics{}
	f.vms["foreign"] = backend.VM{Spec: backend.Spec{Name: "foreign"}, UID: "foreign"}
	f.vms["builder"] = backend.VM{Spec: backend.Spec{Name: "builder", OS: "darwin"}, UID: "builder", Status: "pending"}
	ok, e = p.canReserveJobs(t.Context(), backend.Spec{Name: "new", OS: "darwin"})
	if ok || e != nil {
		t.Fatal(ok, e)
	}
	r := p.submission("x", api.NoCapacity)
	if !strings.Contains(r.Reason, "physical macOS ceiling count=3 limit=2") || !strings.Contains(r.Reason, "foreign OS unknown=1") {
		t.Fatal(r)
	}
	delete(f.vms, "builder")
	ok, e = p.canReserveJobs(t.Context(), backend.Spec{Name: "new", OS: "darwin"})
	if !ok || e != nil {
		t.Fatal("boundary", ok, e)
	}
	diagnosticCalls(t, d, "List", "List")
}
func diagnosticCount(n int) *int { return &n }

func TestDiagnosticEarlyDeadlinesConflictsAndAcceptedReplay(t *testing.T) {
	p, _ := slotsFixture(t, 1)
	warmSlots(t, p)
	d := diagnosticWrap(p)
	expired := time.Now().Add(-time.Hour)
	l := launch("expired")
	l.StartBefore = &expired
	diagnosticSubmit(t, p, l, api.Rejected, "start deadline expired")
	diagnosticCalls(t, d)
	future := time.Now().Add(time.Hour)
	l = launch("future")
	l.StartBefore = &future
	diagnosticSubmit(t, p, l, api.Unsupported, "start window semantics unsupported")
	diagnosticCalls(t, d)
	l = launch("accepted")
	r := diagnosticSubmit(t, p, l, api.Accepted)
	if r.Reason != "" {
		t.Fatal(r)
	}
	diagnosticCalls(t, d, "List", "Get:template", "List", "Create:job")
	d.calls = nil
	r = diagnosticSubmit(t, p, l, api.Accepted)
	if r.Reason != "" {
		t.Fatal(r)
	}
	diagnosticCalls(t, d)
	l.Lease = "OTHER-PRIVATE-LEASE"
	diagnosticSubmit(t, p, l, api.Rejected, "fingerprint conflicts")
	diagnosticCalls(t, d)
}
