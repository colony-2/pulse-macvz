package vz

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/colony-2/pulse-tart/internal/config"
	"github.com/colony-2/pulse-tart/internal/local"
)

func New(c config.VZ, common config.Config) (*local.Client, error) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return nil, ErrPlatform
	}
	for _, b := range common.Bases {
		if b.Architecture() != "arm64" {
			return nil, fmt.Errorf("%w: base %s requires %s", ErrPlatform, b.Name, b.Architecture())
		}
	}
	if !filepath.IsAbs(c.Home) {
		return nil, config.Err
	}
	if c.Runner == "" {
		var err error
		c.Runner, err = os.Executable()
		if err != nil {
			return nil, err
		}
	}
	runner, err := exec.LookPath(c.Runner)
	if err != nil {
		return nil, err
	}
	c.Runner, err = filepath.Abs(runner)
	if err != nil {
		return nil, err
	}
	if c.CommandTimeout == 0 {
		c.CommandTimeout = 2 * time.Minute
	}
	pullTimeout := common.BootstrapTimeout
	if pullTimeout == 0 {
		pullTimeout = 45 * time.Minute
	}
	store := &Store{Home: c.Home, Runner: c.Runner, Timeout: c.CommandTimeout, PullTimeout: pullTimeout}
	return local.NewNative(local.Options{Kind: "vz", Home: c.Home, CommandTimeout: c.CommandTimeout, SSHTimeout: c.SSHTimeout, DeliveryTimeout: c.DeliveryTimeout,
		EndpointArgs: func(target, ready string, port int) []string {
			return []string{c.Runner, "__pulse-vm", "bridge", "--target", target, "--ready", ready, "--port", strconv.Itoa(port)}
		},
	}, common, store)
}
