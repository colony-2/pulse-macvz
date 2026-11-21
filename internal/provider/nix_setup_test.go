package provider

import (
	"testing"

	api "github.com/colony-2/pulse-provider-api"
)

func TestLinuxNixRepairReusesIdleVMAndPreservesAcceptedJobs(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		p, f := dockerFixture(t, true)
		warmPlain(t, p)
		if accepted {
			out, err := p.Submit(t.Context(), []api.Launch{launch("already-running")})
			if err != nil || out[0].Status != api.Accepted {
				t.Fatal(out, err)
			}
		}
		var pair *PairRecord
		for _, v := range p.Store.data.Pairs {
			pair = v
		}
		id, uid, hash := pair.ID, pair.PrimaryUID, pair.ConfigHash
		pair.LinuxNixVersion = 0 // Persisted ready allocation from the old provider.
		before := f.nixSetups
		if err := p.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		if pair.ID != id || pair.PrimaryUID != uid || pair.ConfigHash != hash {
			t.Fatal("replaced an allocation during Nix repair")
		}
		if accepted {
			if f.nixSetups != before || pair.LaunchID != "already-running" || pair.LinuxNixVersion != 0 {
				t.Fatal("modified accepted allocation", pair)
			}
		} else {
			if f.nixSetups != before+1 || pair.LinuxNixVersion != linuxNixVersion || pair.Phase != "ready" {
				t.Fatal("idle allocation not repaired", pair)
			}
			if err := p.Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			if f.nixSetups != before+1 {
				t.Fatal("reconfigured an already repaired allocation")
			}
		}
	}
}

func TestLinuxNixRepairFailurePreventsHandoffAndRetries(t *testing.T) {
	p, f := dockerFixture(t, true)
	warmPlain(t, p)
	for _, pair := range p.Store.data.Pairs {
		pair.LinuxNixVersion = 0
	}
	f.nixSetupDenied = true
	out, err := p.Submit(t.Context(), []api.Launch{launch("cache-repair")})
	if err != nil || out[0].Status == api.Accepted || f.deliveries != 0 {
		t.Fatal("handed off before cache repair", out, err)
	}
	for _, pair := range p.Store.data.Pairs {
		if pair.LaunchID != "" || pair.LinuxNixVersion != 0 {
			t.Fatal("failed repair reserved a job or marked success", pair)
		}
	}
	f.nixSetupDenied = false
	out, err = p.Submit(t.Context(), []api.Launch{launch("cache-repair")})
	if err != nil || out[0].Status != api.Accepted || f.deliveries != 1 {
		t.Fatal(out, err)
	}
}
