package local

import (
	"context"
	"path/filepath"
	"time"

	"github.com/colony-2/pulse-macvz/internal/config"
)

// Hypervisor owns native VM operations. Ownership, guest delivery and recovery
// belong to Client. Implementations must not retry a dispatched start/restore.
type Hypervisor interface {
	Info(context.Context) error
	List(context.Context) ([]Hardware, error)
	Inspect(context.Context, string) (Hardware, error)
	Clone(context.Context, string, string) error
	Configure(context.Context, string, MachineOptions) error
	Start(string) (int, error)
	Suspend(context.Context, string) error
	Stop(context.Context, string) error
	Delete(context.Context, string) error
	IP(context.Context, string) (string, error)
}

type MachineOptions struct {
	CPU, Memory             int64 // Memory is MiB.
	RandomMAC, RandomSerial bool
}

type Options struct {
	Kind, Home                                  string
	CommandTimeout, SSHTimeout, DeliveryTimeout time.Duration
	EndpointArgs                                func(target, ready string, port int) []string
}

func NewNative(c Options, common config.Config, h Hypervisor) (*Client, error) {
	if c.Kind != "vz" || h == nil || !filepath.IsAbs(c.Home) {
		return nil, config.Err
	}
	if c.CommandTimeout == 0 {
		c.CommandTimeout = 2 * time.Minute
	}
	if c.SSHTimeout == 0 {
		c.SSHTimeout = 15 * time.Second
	}
	if c.CommandTimeout < time.Second || c.SSHTimeout < time.Second || c.DeliveryTimeout < 0 || (c.DeliveryTimeout > 0 && c.DeliveryTimeout < time.Second) {
		return nil, config.Err
	}
	b, err := openNative(config.Local{Home: c.Home, CommandTimeout: c.CommandTimeout, SSHTimeout: c.SSHTimeout, DeliveryTimeout: c.DeliveryTimeout}, common, c.Kind, h)
	if err != nil {
		return nil, err
	}
	b.endpointArgs = c.EndpointArgs
	return b, nil
}
