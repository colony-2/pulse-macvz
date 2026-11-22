package provider

import (
	"slices"
	"testing"

	api "github.com/colony-2/pulse-provider-api"
)

func TestKVMAdmissionChecksGuestAccessAndReportsExecutionCapability(t *testing.T) {
	for _, docker := range []bool{false, true} {
		p, f := dockerFixture(t, true)
		p.Config.Bases[0].Capabilities = append(p.Config.Bases[0].Capabilities, "kvm")
		l := launch("nested")
		l.Capabilities = []string{"kvm"}
		if docker {
			l.Capabilities = append(l.Capabilities, "docker")
			readyDocker(t, p)
		} else {
			warmPlain(t, p)
		}
		// Loss of KVM access after warmup must prevent admission as well.
		f.kvmDenied = true
		out, err := p.Submit(t.Context(), []api.Launch{l})
		if err != nil || out[0].Status == api.Accepted || f.deliveries != 0 {
			t.Fatal("admitted without usable KVM", out, err)
		}
		f.kvmDenied = false
		out, err = p.Submit(t.Context(), []api.Launch{l})
		if err != nil || out[0].Status != api.Accepted {
			t.Fatal(out, err)
		}
		args := f.payload.Process.Args
		found := false
		for i := 0; i+1 < len(args); i++ {
			found = found || args[i] == "--execution-capability" && args[i+1] == "kvm"
		}
		if !found || slices.Contains(l.Capabilities, "docker") != (f.engineStarts > 0) {
			t.Fatal("incorrect execution capabilities or Docker activation", args, f.engineStarts)
		}
	}
}

func TestKVMRequestIsUnsupportedWithoutHostCapability(t *testing.T) {
	p, f := dockerFixture(t, true)
	warmPlain(t, p)
	l := launch("no-kvm")
	l.Capabilities = []string{"kvm"}
	out, err := p.Submit(t.Context(), []api.Launch{l})
	if err != nil || out[0].Status != api.Unsupported || f.deliveries != 0 {
		t.Fatal(out, err)
	}
}

func TestEnablingKVMRecyclesUnusedAllocationsButPreservesAcceptedJobs(t *testing.T) {
	for _, active := range []bool{false, true} {
		p, _ := dockerFixture(t, true)
		warmPlain(t, p)
		if active {
			out, err := p.Submit(t.Context(), []api.Launch{launch("running")})
			if err != nil || out[0].Status != api.Accepted {
				t.Fatal(out, err)
			}
		}
		var old string
		for id := range p.Store.data.Pairs {
			old = id
		}
		p.Config.Bases[0].Capabilities = append(p.Config.Bases[0].Capabilities, "kvm")
		warmPlain(t, p)
		pair := p.Store.data.Pairs[old]
		if active {
			if pair == nil || pair.LaunchID != "running" || pair.Phase == "reclaiming" || pair.Phase == "gone" {
				t.Fatal("interrupted accepted job", pair)
			}
		} else if pair != nil && pair.Phase != "gone" {
			t.Fatal("reused an allocation booted without KVM", pair.Phase)
		}
	}
}

func TestKVMNetworkRepairPreservesIdentityAndAcceptedJobs(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		p, f := dockerFixture(t, true)
		p.Config.Bases[0].Capabilities = append(p.Config.Bases[0].Capabilities, "kvm")
		warmPlain(t, p)
		if accepted {
			out, err := p.Submit(t.Context(), []api.Launch{launch("running")})
			if err != nil || out[0].Status != api.Accepted {
				t.Fatal(out, err)
			}
		}
		var pair *PairRecord
		for _, v := range p.Store.data.Pairs {
			pair = v
		}
		id, uid, hash := pair.ID, pair.PrimaryUID, pair.ConfigHash
		pair.LinuxKVMVersion = 0
		before := f.nixSetups
		if err := p.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		if pair.ID != id || pair.PrimaryUID != uid || pair.ConfigHash != hash {
			t.Fatal("replaced idle worker during network repair")
		}
		if accepted {
			if pair.LinuxKVMVersion != 0 || f.nixSetups != before || pair.LaunchID != "running" {
				t.Fatal("modified accepted work")
			}
		} else {
			if pair.LinuxKVMVersion != linuxKVMVersion || f.nixSetups != before+1 || pair.Phase != "ready" {
				t.Fatal("did not repair idle network", pair)
			}
			if err := p.Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			if f.nixSetups != before+1 {
				t.Fatal("repeated completed repair")
			}
		}
	}
}

func TestKVMNetworkFailurePreventsHandoff(t *testing.T) {
	p, f := dockerFixture(t, true)
	p.Config.Bases[0].Capabilities = append(p.Config.Bases[0].Capabilities, "kvm")
	warmPlain(t, p)
	for _, pair := range p.Store.data.Pairs {
		pair.LinuxKVMVersion = 0
	}
	f.nixSetupDenied = true
	l := launch("network-repair")
	out, err := p.Submit(t.Context(), []api.Launch{l})
	if err != nil || out[0].Status == api.Accepted || f.deliveries != 0 {
		t.Fatal("accepted failed repair", out, err)
	}
	for _, pair := range p.Store.data.Pairs {
		if pair.LinuxKVMVersion != 0 || pair.LaunchID != "" {
			t.Fatal("marked failed repair ready", pair)
		}
	}
	f.nixSetupDenied = false
	f.kvmNetworkDenied = true
	out, err = p.Submit(t.Context(), []api.Launch{l})
	if err != nil || out[0].Status == api.Accepted || f.deliveries != 0 {
		t.Fatal("accepted without user network/service operations", out, err)
	}
	f.kvmNetworkDenied = false
	out, err = p.Submit(t.Context(), []api.Launch{l})
	if err != nil || out[0].Status != api.Accepted {
		t.Fatal(out, err)
	}
	proc := f.payload.Process
	if !slices.Equal(proc.Command, []string{"/usr/local/bin/pulse-kvm-run", "/opt/pulse-tart/bin/c2j"}) || string(proc.StdinBytes) != string(l.Lease) {
		t.Fatal("incorrect wrapper or lease")
	}
	if proc.Env["XDG_RUNTIME_DIR"] != "/run/user/55050" || proc.Env["DBUS_SESSION_BUS_ADDRESS"] != "unix:path=/run/user/55050/bus" {
		t.Fatal("missing user bus environment")
	}
}
