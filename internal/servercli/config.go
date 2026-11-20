package servercli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	tart "github.com/colony-2/pulse-tart"
)

// capacityOptions describes job capacity, not image builders or Docker companions.
// macDocker reserves part of the macOS capacity for paired Docker jobs.
type capacityOptions struct {
	macBase                 string
	macOS, linux, macDocker int
	total                   int
	totalSet                bool
}

func (o capacityOptions) config(owner, stateDir, c2j string, poll, bootstrap time.Duration) (tart.Config, error) {
	if o.macOS < 0 || o.macOS > 2 {
		return tart.Config{}, fmt.Errorf("--macos-capacity must be between 0 and 2")
	}
	if o.linux < 0 || o.linux > 1000 {
		return tart.Config{}, fmt.Errorf("--linux-capacity must be between 0 and 1000")
	}
	if o.macDocker < 0 || o.macDocker > o.macOS {
		return tart.Config{}, fmt.Errorf("--macos-docker-capacity must be between 0 and --macos-capacity")
	}
	counts := tart.VMCounts{MacOS: tart.VMCount(o.macOS), Linux: tart.VMCount(o.linux)}
	if o.totalSet {
		if o.total < 0 || o.total > 1002 {
			return tart.Config{}, fmt.Errorf("--total-capacity must be between 0 and 1002")
		}
		counts.Total = tart.VMCount(o.total)
	}
	counts, err := counts.Normalize()
	if err != nil {
		return tart.Config{}, err
	}
	c := tart.Config{OwnerID: owner, StateDir: stateDir, C2JVersion: c2j, PollInterval: poll, BootstrapTimeout: bootstrap, VMCounts: counts}
	engine := tart.DefaultDockerEngine()
	macBases := tart.DefaultBases()
	mac := macBases[0]
	if o.macBase != "" {
		found := false
		for _, b := range macBases {
			if b.Name == o.macBase {
				mac = b
				found = true
				break
			}
		}
		if !found {
			return tart.Config{}, fmt.Errorf("unsupported --macos-base %q", o.macBase)
		}
	}
	c.Bases = []tart.Base{mac, tart.DefaultLinuxBase()}
	c.Docker = &tart.DockerConfig{Profiles: []tart.DockerEngineProfile{engine}}
	// Keep stable pool names when capacity changes. Missing Docker groups retire
	// through normal reconciliation when their count is reduced to zero.
	for i, base := range c.Bases {
		if base.OS == "linux" {
			c.Slots = append(c.Slots, tart.SlotGroup{Name: "linux", Base: base.Name, Count: min(o.linux, *counts.Total), DockerProfile: engine.Name})
			continue
		}
		name := "standard"
		if i > 0 {
			name = "mac-" + base.Name
		}
		c.Slots = append(c.Slots, tart.SlotGroup{Name: name, Base: base.Name, Count: min(o.macOS-o.macDocker, *counts.Total)})
		if o.macDocker > 0 {
			c.Slots = append(c.Slots, tart.SlotGroup{Name: name + "-docker", Base: base.Name, Count: min(o.macDocker, *counts.Total), DockerProfile: engine.Name})
		}
	}
	resolved, err := tart.ResolveConfig(c)
	if err != nil {
		return tart.Config{}, fmt.Errorf("invalid provider configuration (owner, state path, capacity, or preparation settings): %w", err)
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
