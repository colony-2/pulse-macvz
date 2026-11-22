package macvz

import (
	"context"
	"errors"
)

type imageMaintenance interface {
	ImageUpgradeState() (string, error)
	BeginImageUpgrade(context.Context, []string) error
	ActivateImageUpgrade(context.Context) error
	CommitImageUpgrade(context.Context) error
	RollbackImageUpgrade(context.Context) (bool, error)
}

func (r *Runtime) ImageUpgradeState() (string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return "", ErrClosed
	}
	if b, ok := r.p.Backend.(imageMaintenance); ok {
		return b.ImageUpgradeState()
	}
	return "", nil
}
func (r *Runtime) BeginImageUpgrade(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if b, ok := r.p.Backend.(imageMaintenance); ok {
		return b.BeginImageUpgrade(ctx, r.maintenanceImages)
	}
	return errors.New("backend does not support recoverable image upgrades")
}
func (r *Runtime) CommitImageUpgrade(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if b, ok := r.p.Backend.(imageMaintenance); ok {
		return b.CommitImageUpgrade(ctx)
	}
	return nil
}
func (r *Runtime) RollbackImageUpgrade(ctx context.Context) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false, ErrClosed
	}
	if b, ok := r.p.Backend.(imageMaintenance); ok {
		return b.RollbackImageUpgrade(ctx)
	}
	return false, errors.New("backend does not support image rollback")
}

func (r *Runtime) ActivateImageUpgrade(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if b, ok := r.p.Backend.(imageMaintenance); ok {
		return b.ActivateImageUpgrade(ctx)
	}
	return errors.New("backend does not support image upgrade activation")
}
