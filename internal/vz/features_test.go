package vz

import (
	"reflect"
	"slices"
	"testing"

	"github.com/colony-2/pulse-tart/internal/config"
)

func TestRunnerFeaturesRequireExplicitCompatibleReply(t *testing.T) {
	for _, reply := range []string{`"pulse-vz/1"`, `{}`, `{"protocol":"pulse-vz/1"}`, `{"protocol":"old","nested_virtualization":true}`, `{"protocol":"pulse-vz/1","nested_virtualization":null}`, `{"protocol":"pulse-vz/1","nested_virtualization":"true"}`} {
		if _, err := parseFeatures([]byte(reply)); err == nil {
			t.Fatalf("accepted incomplete or incompatible runner: %s", reply)
		}
	}
	for _, enabled := range []bool{false, true} {
		reply := `{"protocol":"pulse-vz/1","nested_virtualization":false}`
		if enabled {
			reply = `{"protocol":"pulse-vz/1","nested_virtualization":true}`
		}
		f, err := parseFeatures([]byte(reply))
		if err != nil || f.NestedVirtualization != enabled {
			t.Fatal(f, err)
		}
	}
}

func TestKVMCapabilitiesFollowHostAndGuestArchitecture(t *testing.T) {
	c := config.Config{Bases: []config.Base{
		{Name: "mac", OS: "darwin", Capabilities: []string{"git"}},
		{Name: "linux-arm", OS: "linux", Capabilities: []string{"git", "nix"}},
		{Name: "linux-x86", OS: "linux", Arch: "amd64", Capabilities: []string{"git"}},
	}}
	for _, supported := range []bool{false, true} {
		result := WithCapabilities(c, Features{NestedVirtualization: supported})
		for i, base := range result.Bases {
			if slices.Contains(base.Capabilities, "kvm") != (supported && i == 1) {
				t.Fatal("incorrect KVM advertisement", supported, base.Name, base.Capabilities)
			}
		}
		if !reflect.DeepEqual(result, WithCapabilities(result, Features{NestedVirtualization: supported})) {
			t.Fatal("feature resolution is not idempotent")
		}
	}
	if !reflect.DeepEqual(c.Bases[1].Capabilities, []string{"git", "nix"}) {
		t.Fatal("mutated caller's capabilities")
	}
}
