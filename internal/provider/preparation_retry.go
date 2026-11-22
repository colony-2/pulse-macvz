package provider

import (
	"context"
	"github.com/colony-2/pulse-macvz/internal/backend"
)

// RepairPreparations is maintenance-only. It does not select jobs, allocations,
// ready sources, obsolete images, or quarantined identities for replacement.
func (p *Provider) RepairPreparations(ctx context.Context) (bool, error) {
	repair, ok := p.Backend.(interface {
		RetryPreparation(context.Context, string, string) (bool, error)
	})
	if !ok {
		return false, nil
	}
	if err := p.Store.mu.LockContext(ctx); err != nil {
		return false, err
	}
	defer p.Store.mu.Unlock()
	if err := p.Store.refresh(); err != nil {
		return false, err
	}
	wanted := p.requiredBaseImages()
	targets, err := p.slotTargets()
	if err != nil {
		return false, err
	}
	candidates := map[string]*ImageRecord{}
	for name, r := range p.Store.data.Images {
		if _, ok := wanted[name]; ok {
			candidates[name] = r
		}
	}
	for id, r := range p.Store.data.Slots {
		if _, ok := targets[id]; ok {
			candidates[r.Name] = &ImageRecord{Name: r.Name, UID: r.UID, Phase: r.Phase, NativeHash: r.NativeHash, Worker: r.Worker}
		}
	}
	pending := false
	for name, r := range candidates {
		if r.UID == "" || r.Phase == "ready" || r.Phase == "quarantine" || r.Phase == "deleting" {
			continue
		}
		v, err := p.Backend.Get(ctx, name)
		if backend.IsStatus(err, 404) {
			continue
		}
		if err != nil {
			return pending, err
		}
		if !p.imageMatches(r, v) {
			continue
		}
		retrying, err := repair.RetryPreparation(ctx, name, r.UID)
		if err != nil {
			return pending, err
		}
		pending = pending || retrying
	}
	return pending, nil
}
