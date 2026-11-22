package local

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"

	"github.com/colony-2/pulse-macvz/internal/workerhealth"
)

// ProbeHealth is read-only and uses the same native identity as management SSH.
func (b *Client) ProbeHealth(ctx context.Context, name, uid, tenant, job string) (workerhealth.Observation, error) {
	if err := b.mu.LockContext(ctx); err != nil {
		return workerhealth.Observation{}, err
	}
	v, err := b.get(ctx, name)
	b.mu.Unlock()
	if err != nil || v.UID != uid || v.Stopped() {
		return workerhealth.Observation{}, workerhealth.ErrIdentity
	}
	return ProbeHealth(ctx, b.cfg.Home, b.scope, name, uid, tenant, job, b.hypervisor().IP)
}

// ProbeHealth can also be used by read-only CLI inspection while a controller
// owns the pool. It never opens a controller, modifies metadata or guesses IPs.
func ProbeHealth(ctx context.Context, home, scope, name, uid, tenant, job string, ip func(context.Context, string) (string, error)) (workerhealth.Observation, error) {
	var empty workerhealth.Observation
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." || uid == "" {
		return empty, workerhealth.ErrIdentity
	}
	dir := filepath.Join(home, "vms", name)
	check := func() bool {
		data, err := privateFile(filepath.Join(dir, "pulse.json"))
		if err != nil {
			return false
		}
		var v struct {
			Scope  string
			Native struct{ UID string }
		}
		return json.Unmarshal(data, &v) == nil && v.Scope == scope && v.Native.UID == uid
	}
	if !check() {
		return empty, workerhealth.ErrIdentity
	}
	version, err := ReadImageVersion(dir)
	if err != nil {
		return empty, err
	}
	if !version.HealthProbe {
		return empty, nil
	}
	ctx, cancel := context.WithTimeout(ctx, workerhealth.Timeout)
	defer cancel()
	address, err := ip(ctx, name)
	var result workerhealth.Observation
	if err != nil || net.ParseIP(address) == nil {
		result = workerhealth.Observation{Supported: true, Failure: "guest IP unavailable"}
	} else {
		result = workerhealth.Fetch(ctx, net.JoinHostPort(address, workerhealth.Port), tenant, job)
	}
	if !check() {
		return empty, workerhealth.ErrIdentity
	}
	if err := ctx.Err(); err != nil && result.Failure == "" {
		return empty, err
	}
	return result, nil
}
