package local

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"time"

	"github.com/colony-2/pulse-tart/internal/config"
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
	if c.Kind == "" || c.Kind == "tart" || h == nil || !filepath.IsAbs(c.Home) {
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
	b, err := openNative(config.Tart{Home: c.Home, CommandTimeout: c.CommandTimeout, SSHTimeout: c.SSHTimeout, DeliveryTimeout: c.DeliveryTimeout}, common, c.Kind, h)
	if err != nil {
		return nil, err
	}
	b.endpointArgs = c.EndpointArgs
	return b, nil
}

// The Tart adapter preserves its CLI contract and test injection points.
type tartHypervisor struct{ b *Client }

func (t tartHypervisor) Info(ctx context.Context) error {
	_, err := t.b.command(ctx, "--version")
	return err
}
func (t tartHypervisor) List(ctx context.Context) ([]Hardware, error) {
	data, err := t.b.command(ctx, "list", "--source", "local", "--format", "json")
	if err != nil {
		return nil, err
	}
	var result []Hardware
	if err = json.Unmarshal(data, &result); err != nil {
		return nil, failure("decode inventory", err)
	}
	if result == nil {
		return nil, errors.New("inventory is null")
	}
	return result, nil
}
func (t tartHypervisor) Inspect(ctx context.Context, name string) (Hardware, error) {
	var h Hardware
	data, err := t.b.command(ctx, "get", name, "--format", "json")
	if err != nil {
		return h, err
	}
	if err = json.Unmarshal(data, &h); err != nil {
		return h, failure("decode inspect", err)
	}
	return h, nil
}
func (t tartHypervisor) Clone(ctx context.Context, src, dst string) error {
	_, err := t.b.command(ctx, "clone", src, dst)
	return err
}
func (t tartHypervisor) Configure(ctx context.Context, name string, c MachineOptions) error {
	args := []string{"set", name, "--cpu", strconv.FormatInt(c.CPU, 10), "--memory", strconv.FormatInt(c.Memory, 10)}
	if c.RandomMAC {
		args = append(args, "--random-mac")
	}
	if c.RandomSerial {
		args = append(args, "--random-serial")
	}
	_, err := t.b.command(ctx, args...)
	return err
}
func (t tartHypervisor) Start(name string) (int, error) { return t.b.launch(name) }
func (t tartHypervisor) Suspend(ctx context.Context, name string) error {
	_, err := t.b.command(ctx, "suspend", name)
	return err
}
func (t tartHypervisor) Stop(ctx context.Context, name string) error {
	_, err := t.b.command(ctx, "stop", name)
	return err
}
func (t tartHypervisor) Delete(ctx context.Context, name string) error {
	_, err := t.b.command(ctx, "delete", name)
	return err
}
func (t tartHypervisor) IP(ctx context.Context, name string) (string, error) {
	data, err := t.b.command(ctx, "ip", name, "--wait", "5")
	return string(data), err
}
