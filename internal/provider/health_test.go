package provider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/colony-2/pulse-macvz/internal/backend"
	"github.com/colony-2/pulse-macvz/internal/workerhealth"
	api "github.com/colony-2/pulse-provider-api"
)

type healthFake struct {
	*fake
	observation workerhealth.Observation
	probeErr    error
	stops       int
	replace     bool
}

func (f *healthFake) ProbeHealth(_ context.Context, name, uid, tenant, job string) (workerhealth.Observation, error) {
	if f.replace {
		v := f.vms[name]
		v.UID = "replacement"
		f.vms[name] = v
	}
	return f.observation, f.probeErr
}
func (f *healthFake) Stop(_ context.Context, name, uid string) error {
	v := f.vms[name]
	if v.UID != uid {
		return errors.New("identity changed")
	}
	f.stops++
	v.Status = "failed"
	v.PowerState = "stopped"
	v.Conditions = []backend.Condition{{Type: "running", State: "false"}}
	f.vms[name] = v
	return nil
}
func healthFixture(t *testing.T) (*Provider, *healthFake, *Record) {
	p, f := fixture(t)
	v := backend.VM{Spec: backend.Spec{Name: "health-job", OS: "linux"}, UID: "vm-uid", Status: "running", PowerState: "running"}
	f.vms[v.Name] = v
	r := &Record{LaunchID: "health-launch", Name: v.Name, UID: v.UID, NativeHash: p.Store.hash(normalized(v.Spec)), Fingerprint: "test", Outcome: api.Accepted, Phase: "active", Job: api.JobRef{TenantID: "tenant", JobID: "job"}, Health: &workerhealth.State{}}
	p.Store.data.Records[r.LaunchID] = r
	b := &healthFake{fake: f, observation: workerhealth.Observation{Supported: true, Failure: "unreachable"}}
	p.Backend = b
	return p, b, r
}
func TestHealthReapAndReconcile(t *testing.T) {
	p, b, r := healthFixture(t)
	start := time.Now().UTC()
	seen := map[string]bool{}
	for i := 0; i <= 18; i++ {
		if err := p.checkLaunchHealth(context.Background(), start.Add(time.Duration(i)*workerhealth.Interval), seen); err != nil {
			t.Fatal(err)
		}
	}
	if b.stops != 1 || r.Phase != "deleting" || r.Health.ReapRequestedAt == nil {
		t.Fatal("not stopped", b.stops, r)
	}
	vms, _ := b.List(context.Background())
	if err := p.reconcileRecords(context.Background(), vms); err != nil {
		t.Fatal(err)
	}
	if r.Phase != "done" || b.deletes != 1 {
		t.Fatal("normal reclamation did not delete", r.Phase, b.deletes)
	}
}
func TestHealthNoDestructionOnUnknownOrLegacy(t *testing.T) {
	for _, mode := range []string{"legacy", "unsupported", "observation-error", "unknown-admission", "identity-change", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			p, b, r := healthFixture(t)
			switch mode {
			case "legacy":
				r.Health = nil
			case "unsupported":
				b.observation = workerhealth.Observation{}
			case "observation-error":
				b.probeErr = errors.New("unavailable")
			case "unknown-admission":
				r.Outcome = api.Unknown
			case "identity-change":
				b.replace = true
			}
			start := time.Now()
			seen := map[string]bool{}
			for i := 0; i < 30; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				if mode == "canceled" {
					cancel()
				}
				err := p.checkLaunchHealth(ctx, start.Add(time.Duration(i)*workerhealth.Interval), seen)
				cancel()
				if err != nil && mode != "canceled" {
					t.Fatal(err)
				}
			}
			if b.stops != 0 || b.deletes != 0 {
				t.Fatal("unsafe cleanup", b.stops, b.deletes)
			}
		})
	}
}
func TestHealthRestartAndRecoveredOutage(t *testing.T) {
	p, b, r := healthFixture(t)
	start := time.Now()
	seen := map[string]bool{}
	b.observation = workerhealth.Observation{Supported: true, Snapshot: &workerhealth.Snapshot{}}
	if err := p.checkLaunchHealth(context.Background(), start, seen); err != nil {
		t.Fatal(err)
	}
	b.observation = workerhealth.Observation{Supported: true, Failure: "timeout"}
	for i := 1; i <= 4; i++ {
		if err := p.checkLaunchHealth(context.Background(), start.Add(time.Duration(i)*workerhealth.Interval), seen); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.checkLaunchHealth(context.Background(), start.Add(time.Hour), map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if b.stops != 0 || r.Health.Failures != 1 {
		t.Fatal("restart counted downtime")
	}
	b.observation = workerhealth.Observation{Supported: true, Snapshot: &workerhealth.Snapshot{}}
	if err := p.checkLaunchHealth(context.Background(), start.Add(time.Hour+workerhealth.Interval), seen); err != nil {
		t.Fatal(err)
	}
	if r.Health.Status != "healthy" || r.Health.FailingSince != nil {
		t.Fatal("not recovered")
	}
}
func TestHealthReapRecoversCommittedIntent(t *testing.T) {
	p, b, r := healthFixture(t)
	at := time.Now()
	r.Health.ReapRequestedAt = &at
	r.Health.Status = "reaping"
	r.Phase = "deleting"
	// The probe need not be reachable after a durable cleanup intent.
	b.probeErr = errors.New("unavailable")
	if err := p.checkLaunchHealth(context.Background(), at, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if b.stops != 1 {
		t.Fatal("cleanup intent lost after restart")
	}
}
func TestHealthReclaimsPair(t *testing.T) {
	p, b, r := healthFixture(t)
	v := b.vms[r.Name]
	pair := &PairRecord{ID: "pair", LaunchID: r.LaunchID, Primary: v.Spec, PrimaryUID: v.UID, PrimaryHash: r.NativeHash, Phase: "running"}
	p.Store.data.Pairs[pair.ID] = pair
	r.PairID = pair.ID
	at := time.Now()
	r.Health.ReapRequestedAt = &at
	if err := p.checkLaunchHealth(context.Background(), at, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if pair.Phase != "reclaiming" || b.stops != 1 {
		t.Fatal("pair not routed to ordinary reclamation", pair.Phase, b.stops)
	}
}

func TestHealthIntentPersistsAcrossStoreReopen(t *testing.T) {
	p, b, r := healthFixture(t)
	at := time.Now().UTC()
	r.Health.ReapRequestedAt = &at
	r.Health.Status = "reaping"
	r.Phase = "deleting"
	if err := p.Store.save(); err != nil {
		t.Fatal(err)
	}
	dir, scope := p.Store.dir, p.Store.data.Scope
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(dir, scope)
	if err != nil {
		t.Fatal(err)
	}
	next := Attach(p.Config, b, reopened)
	defer next.Close()
	if err = next.checkLaunchHealth(t.Context(), at.Add(time.Hour), map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if b.stops != 1 || next.Store.data.Records[r.LaunchID].Health.ReapRequestedAt == nil {
		t.Fatal("durable cleanup intent lost")
	}
}
