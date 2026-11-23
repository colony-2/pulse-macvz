package provider

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/colony-2/pulse-macvz/internal/config"
	api "github.com/colony-2/pulse-provider-api"
)

// Preserve the pre-trapping hash to exercise migration from persisted pools.
func legacyDockerHash(p *Provider, g config.SlotGroup) string {
	g.Count = 0
	return p.Store.hash(struct {
		Base               config.Base
		Group              config.SlotGroup
		Engine             config.EngineProfile
		Endpoint           config.Endpoint
		PreparationVersion int
	}{p.base(g.Base), g, p.Config.Engine(g.DockerProfile), p.Config.DockerEndpoint, 2})
}

func TestGICPolicySeparatesARMWorkerImagesAndAllocations(t *testing.T) {
	for _, tc := range []struct {
		os, arch string
		changed  bool
	}{{"linux", "arm64", true}, {"linux", "amd64", false}, {"darwin", "arm64", false}} {
		t.Run(tc.os+tc.arch, func(t *testing.T) {
			p, _ := dockerFixture(t, tc.os == "linux")
			b := &p.Config.Bases[0]
			b.OS = tc.os
			b.Arch = tc.arch
			label := b.Name + "-docker"
			if tc.os == "linux" {
				label += "-engine-v1-29.1.3"
			}
			if tc.changed {
				label += "-gic-traps-v1"
			}
			if got, want := p.baseImageName(b.Name), config.BaseImageName(p.Config.OwnerID, label); got != want {
				t.Fatal(got, want)
			}
			if (p.dockerHash(p.Config.Slots[0]) != legacyDockerHash(p, p.Config.Slots[0])) != tc.changed {
				t.Fatal("unexpected allocation policy migration")
			}
		})
	}
}

func TestGICMigrationPreservesAcceptedJobsAndReplacesUnusedWorkers(t *testing.T) {
	for _, active := range []bool{false, true} {
		p, _ := dockerFixture(t, true)
		warmPlain(t, p)
		if active {
			out, err := p.Submit(t.Context(), []api.Launch{launch("running")})
			if err != nil || out[0].Status != api.Accepted {
				t.Fatal(out, err)
			}
		}
		var pair *PairRecord
		for _, v := range p.Store.data.Pairs {
			pair = v
		}
		oldID, oldUID := pair.ID, pair.PrimaryUID
		pair.ConfigHash = legacyDockerHash(p, p.Config.Slots[0])
		warmPlain(t, p)
		remaining := p.Store.data.Pairs[oldID]
		if active {
			if remaining == nil || remaining.LaunchID != "running" || remaining.PrimaryUID != oldUID || remaining.Phase == "reclaiming" || remaining.Phase == "gone" {
				t.Fatal("interrupted accepted work", remaining)
			}
		} else if remaining != nil && remaining.Phase != "gone" {
			t.Fatal("reused legacy worker", remaining.Phase)
		}
	}
}

func TestGICReadinessGatesHandoffEvenWithoutKVMRequest(t *testing.T) {
	p, f := dockerFixture(t, true)
	warmPlain(t, p)
	f.gicDenied = true
	out, err := p.Submit(t.Context(), []api.Launch{launch("trap-check")})
	if err != nil || out[0].Status == api.Accepted || f.deliveries != 0 {
		t.Fatal(out, err)
	}
	f.gicDenied = false
	out, err = p.Submit(t.Context(), []api.Launch{launch("trap-check")})
	if err != nil || out[0].Status != api.Accepted {
		t.Fatal(out, err)
	}
}

func TestGICProbeRequiresAllThreeEffectiveArguments(t *testing.T) {
	args := "kvm-arm.vgic_v3_group0_trap=1 kvm-arm.vgic_v3_group1_trap=1 kvm-arm.vgic_v3_common_trap=1"
	for _, tc := range []struct {
		name, line string
		ok         bool
	}{
		{"all", "root=UUID=test quiet " + args, true},
		{"missing", "root=UUID=test", false},
		{"partial", strings.ReplaceAll(args, "kvm-arm.vgic_v3_common_trap=1", ""), false},
		{"conflict", args + " kvm-arm.vgic_v3_group0_trap=0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := strings.ReplaceAll(gicTrapsReadinessProbe, "open('/proc/cmdline').read()", strconv.Quote(tc.line))
			output, err := exec.Command("/bin/sh", "-c", script).CombinedOutput()
			if (err == nil) != tc.ok || tc.ok && strings.TrimSpace(string(output)) != "PULSE_GIC_TRAPS_READY" {
				t.Fatalf("probe: %v %s", err, output)
			}
		})
	}
}
