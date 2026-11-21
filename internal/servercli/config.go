package servercli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	macvz "github.com/colony-2/pulse-macvz"
)

// capacityOptions describes job capacity, not image builders or Docker companions.
// macDocker optionally limits Docker eligibility; -1 enables every macOS slot.
type capacityOptions struct {
	macBase                 string
	macOS, linux, macDocker int
	total                   int
	totalSet                bool
}

func (o capacityOptions) config(owner, stateDir, c2j string, poll, bootstrap time.Duration) (macvz.Config, error) {
	if o.macOS < 0 || o.macOS > 2 {
		return macvz.Config{}, fmt.Errorf("--macos-capacity must be between 0 and 2")
	}
	if o.linux < 0 || o.linux > 1000 {
		return macvz.Config{}, fmt.Errorf("--linux-capacity must be between 0 and 1000")
	}
	if o.macDocker < -1 || o.macDocker > o.macOS {
		return macvz.Config{}, fmt.Errorf("--macos-docker-capacity must be -1 (all) or between 0 and --macos-capacity")
	}
	counts := macvz.VMCounts{MacOS: macvz.VMCount(o.macOS), Linux: macvz.VMCount(o.linux)}
	if o.totalSet {
		if o.total < 0 || o.total > 1002 {
			return macvz.Config{}, fmt.Errorf("--total-capacity must be between 0 and 1002")
		}
		counts.Total = macvz.VMCount(o.total)
	}
	counts, err := counts.Normalize()
	if err != nil {
		return macvz.Config{}, err
	}
	c := macvz.Config{OwnerID: owner, StateDir: stateDir, C2JVersion: c2j, PollInterval: poll, BootstrapTimeout: bootstrap, VMCounts: counts}
	engine := macvz.DefaultDockerEngine()
	macBases := macvz.DefaultBases()
	selected := map[string]bool{}
	if o.macBase == "" || o.macBase == "all" {
		for _, b := range macBases {
			selected[b.Name] = true
		}
	} else {
		for _, name := range strings.Split(o.macBase, ",") {
			name = strings.TrimSpace(name)
			found := false
			for _, b := range macBases {
				found = found || b.Name == name
			}
			if !found || selected[name] {
				return macvz.Config{}, fmt.Errorf("unsupported or duplicate --macos-base %q", name)
			}
			selected[name] = true
		}
	}
	// Catalog order keeps slot names stable when the flag order changes.
	for _, b := range macBases {
		if selected[b.Name] {
			c.Bases = append(c.Bases, b)
		}
	}
	c.Bases = append(c.Bases, macvz.DefaultLinuxBase())
	c.Docker = &macvz.DockerConfig{Profiles: []macvz.DockerEngineProfile{engine}}
	// Keep stable pool names when capacity changes. Missing Docker groups retire
	// through normal reconciliation when their count is reduced to zero.
	for _, base := range c.Bases {
		if base.OS == "linux" {
			c.Slots = append(c.Slots, macvz.SlotGroup{Name: "linux", Base: base.Name, Count: min(o.linux, *counts.Total), DockerProfile: engine.Name})
			continue
		}
		name := "standard"
		if base.Name != macBases[0].Name {
			name = "mac-" + base.Name
		}
		if o.macDocker == -1 {
			c.Slots = append(c.Slots, macvz.SlotGroup{Name: name, Base: base.Name, Count: min(o.macOS, *counts.Total), DockerProfile: engine.Name})
			continue
		}
		c.Slots = append(c.Slots, macvz.SlotGroup{Name: name, Base: base.Name, Count: min(o.macOS-o.macDocker, *counts.Total)})
		if o.macDocker > 0 {
			c.Slots = append(c.Slots, macvz.SlotGroup{Name: name + "-docker", Base: base.Name, Count: min(o.macDocker, *counts.Total), DockerProfile: engine.Name})
		}
	}
	resolved, err := macvz.ResolveConfig(c)
	if err != nil {
		return macvz.Config{}, fmt.Errorf("invalid provider configuration (owner, state path, capacity, or preparation settings): %w", err)
	}
	return resolved, nil
}

func absolutePath(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if value == "~" || len(value) > 1 && value[:2] == "~/" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		value = filepath.Join(home, value[1:])
	}
	return filepath.Abs(value)
}
