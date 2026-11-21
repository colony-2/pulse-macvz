package vz

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/colony-2/pulse-macvz/internal/config"
)

// Features describe the selected runner, rather than assuming that an external
// runner has the same implementation or host support as the provider library.
type Features struct {
	Protocol             string `json:"protocol"`
	NestedVirtualization bool   `json:"nested_virtualization"`
}

func RunnerFeatures(ctx context.Context, runner string) (Features, error) {
	path, err := resolveRunner(runner)
	if err != nil {
		return Features{}, err
	}
	s := &Store{Runner: path, Timeout: 10 * time.Second}
	out, err := s.command(ctx, "features")
	if err != nil {
		return Features{}, fmt.Errorf("query VZ runner features (update the runner with the provider): %w", err)
	}
	return parseFeatures(out)
}

func parseFeatures(out []byte) (Features, error) {
	var reply struct {
		Protocol string `json:"protocol"`
		Nested   *bool  `json:"nested_virtualization"`
	}
	if json.Unmarshal(out, &reply) != nil || reply.Protocol != runnerProtocol || reply.Nested == nil {
		return Features{}, fmt.Errorf("invalid VZ runner features; update the runner with the provider")
	}
	return Features{Protocol: reply.Protocol, NestedVirtualization: *reply.Nested}, nil
}

// WithCapabilities adds host-dependent execution capabilities after normalizing
// user configuration. Like docker, kvm is derived, not a configured software tool.
// Guest readiness must also prove KVM works before any allocation is admitted.
func WithCapabilities(c config.Config, f Features) config.Config {
	c.Bases = slices.Clone(c.Bases)
	for i := range c.Bases {
		b := &c.Bases[i]
		if f.NestedVirtualization && b.OS == "linux" && b.Architecture() == "arm64" {
			b.Capabilities = append(slices.Clone(b.Capabilities), "kvm")
			slices.Sort(b.Capabilities)
			b.Capabilities = slices.Compact(b.Capabilities)
		}
	}
	return c
}
