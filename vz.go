package tart

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/colony-2/pulse-tart/internal/config"
	engine "github.com/colony-2/pulse-tart/internal/provider"
	"github.com/colony-2/pulse-tart/internal/vz"
)

type VZConfig = config.VZ

// RunnerMain dispatches private VM/bridge invocations and exits, or returns for
// ordinary host invocations. Call it before other work in main. The executable
// needs com.apple.security.virtualization=true in its code-signing entitlements.
func RunnerMain() { vz.RunnerMain() }

// NewVZ adds an independent local provider. Provider/default selection belongs
// to the caller; Tart and Orchard remain available. Close leaves VMs running.
func NewVZ(c Config, options VZConfig) (*Runtime, error) {
	c, err := prepare(c)
	if err != nil {
		return nil, err
	}
	if options.Home == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		options.Home = filepath.Join(home, ".pulse", "vz")
	}
	if !filepath.IsAbs(options.Home) {
		return nil, ErrConfig
	}
	options.Home = filepath.Clean(options.Home)
	if c.NeedsDockerEndpoint() {
		if err = options.DockerEndpoint.Validate(); err != nil {
			return nil, err
		}
	}
	b, err := vz.New(options, c)
	if err != nil {
		return nil, err
	}
	for _, base := range c.Bases {
		if base.OS == "linux" {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			features, err := vz.RunnerFeatures(ctx, options.Runner)
			cancel()
			if err != nil {
				b.Close()
				return nil, err
			}
			c = vz.WithCapabilities(c, features)
			break
		}
	}
	settings := config.Settings{Config: c, Kind: "vz", Endpoint: options.Home, Worker: "local", DockerEndpoint: options.DockerEndpoint}
	store, err := engine.OpenVMStore(c.StateDir, settings.Scope(), b)
	if err != nil {
		b.Close()
		return nil, err
	}
	return newRuntime(engine.Attach(settings, b, store), c), nil
}
