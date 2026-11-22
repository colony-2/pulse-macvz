package local

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/colony-2/pulse-macvz/internal/backend"
	"github.com/colony-2/pulse-macvz/internal/config"
)

// The ledger outlives VM deletion and controller restart. A failed replacement
// cannot obtain a fresh retry budget by losing its pulse.json.
type preparationRetries struct {
	Count   int       `json:"count"`
	UID     string    `json:"uid"`
	Started time.Time `json:"started"`
}
type preparationProperties struct {
	Scope string                                   `json:"scope"`
	Key   []byte                                   `json:"key"`
	Job   json.RawMessage                          `json:"job"`
	Pair  json.RawMessage                          `json:"pair"`
	Image *struct{ Name, UID, Phase, Kind string } `json:"image"`
	Slot  *struct{ Name, UID, Phase string }       `json:"slot"`
}

func (b *Client) preparationIdentity(name, uid string) (preparationProperties, error) {
	var p preparationProperties
	r := b.records[name]
	if !config.Name(name) || !config.Name(uid) || r == nil || r.UID != uid || json.Unmarshal(b.properties[name], &p) != nil || p.Scope != b.scope || len(p.Job) != 0 || len(p.Pair) != 0 {
		return p, fmt.Errorf("preparation identity rejected: %s", name)
	}
	if p.Image != nil && p.Image.Phase == "quarantine" || p.Slot != nil && p.Slot.Phase == "quarantine" {
		return p, fmt.Errorf("quarantined preparation identity: %s", name)
	}
	if p.Image != nil && p.Image.Name == name && p.Image.UID == uid && (p.Image.Kind == "base" || p.Image.Kind == "slot") && p.Image.Phase != "quarantine" {
		return p, nil
	}
	if p.Slot != nil && p.Slot.Name == name && p.Slot.UID == uid && p.Slot.Phase != "quarantine" {
		return p, nil
	}
	return p, fmt.Errorf("preparation properties rejected: %s", name)
}

// RetryPreparation discards an unused failed builder, never replaying its
// possibly executed command. The provider checks the full native spec first.
// Only explicit maintenance calls this; accepted work is rejected independently.
func (b *Client) RetryPreparation(ctx context.Context, name, uid string) (bool, error) {
	if err := b.mu.LockContext(ctx); err != nil {
		return false, err
	}
	defer b.mu.Unlock()
	p, err := b.preparationIdentity(name, uid)
	if err != nil {
		return false, err
	}
	r := b.records[name]
	if r.Reusable || p.Image != nil && p.Image.Phase == "ready" || p.Slot != nil && p.Slot.Phase == "ready" {
		return false, nil
	}
	failed := p.Image != nil && p.Image.Phase == "failed" || p.Slot != nil && p.Slot.Phase == "failed"
	failed = failed || r.Delivery != nil && r.Delivery.Expired && !r.Delivered && !r.Delivery.ExecutionPossible
	if s := b.basePreparations[name]; s != nil && s.uid == uid && s.done && s.err != nil {
		failed = true
	}
	if !failed {
		return false, nil
	}
	dir := filepath.Join(b.dir, "preparation-retries")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return false, err
	}
	path := filepath.Join(dir, name+".json")
	var saved preparationRetries
	data, err := privateFile(path)
	if err == nil {
		if err = json.Unmarshal(data, &saved); err != nil {
			return false, err
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}

	// Preserve private ownership/delivery evidence before any stop/delete.
	evidence := filepath.Join(dir, uid+".json")
	if _, err = os.Lstat(evidence); os.IsNotExist(err) {
		data, err = privateFile(b.artifact(name, "json"))
		if err != nil {
			return false, err
		}
		if err = atomicPrivateFile(evidence, data); err != nil {
			return false, err
		}
	} else if err != nil {
		return false, err
	}
	stopped, err := b.quiescePreparation(ctx, name, uid)
	if err != nil {
		return false, err
	}
	if !stopped {
		return true, nil
	}
	if saved.UID != uid {
		if saved.Count >= 2 {
			return false, nil
		}
		saved.Count++
		saved.UID = uid
		saved.Started = time.Now().UTC()
		data, _ = json.Marshal(saved)
		if err = atomicPrivateFile(path, data); err != nil {
			return false, err
		}
	}
	if err = b.hypervisor().Delete(ctx, name); err != nil {
		return false, err
	}
	if _, err = os.Lstat(b.path(name)); !os.IsNotExist(err) {
		return false, fmt.Errorf("retry deletion not confirmed: %s", name)
	}
	delete(b.records, name)
	delete(b.properties, name)
	delete(b.basePreparations, name)
	return true, nil
}

// StopPreparation is used only by explicit image retirement after the provider
// has drained accepted work and verified the native spec.
func (b *Client) StopPreparation(ctx context.Context, name, uid string) error {
	if err := b.mu.LockContext(ctx); err != nil {
		return err
	}
	defer b.mu.Unlock()
	done, err := b.quiescePreparation(ctx, name, uid)
	if err != nil {
		return err
	}
	if !done {
		return backend.ErrBusy
	}
	return nil
}
func (b *Client) quiescePreparation(ctx context.Context, name, uid string) (bool, error) {
	if _, err := b.preparationIdentity(name, uid); err != nil {
		return false, err
	}
	v, err := b.get(ctx, name)
	if err != nil {
		return false, err
	}
	if !v.Stopped() {
		if err = b.hypervisor().Stop(ctx, name); err != nil {
			return false, err
		}
		return false, nil
	}
	if b.workers[name] {
		if cancel := b.workerCancels[name]; cancel != nil {
			cancel()
		}
		return false, nil
	}
	if observer := b.basePreparations[name]; observer != nil && observer.uid == uid && !observer.done {
		if observer.cancel != nil {
			observer.cancel()
		}
		return false, nil
	}
	return true, nil
}
