package vz

import (
	"context"
	"errors"
	"fmt"
	"github.com/colony-2/pulse-macvz/internal/config"
	"os"
	"path/filepath"
)

// MovePrepared relocates an inactive VM directory without changing identity or
// dropping private metadata. The caller owns the pool lock and validates its UID.
// Destination absence makes retries safe after an interrupted rename.
func (s *Store) MovePrepared(ctx context.Context, name, checkpoint string, restore bool) error {
	if !config.Name(name) || !filepath.IsAbs(checkpoint) {
		return errors.New("invalid preparation checkpoint")
	}
	vm, err := s.path(name)
	if err != nil {
		return err
	}
	saved := filepath.Join(checkpoint, name)
	src, dst := vm, saved
	if restore {
		src, dst = saved, vm
	}
	lock, err := lockFile(ctx, filepath.Join(src, "vz.lock"), true, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	if held, err := lockHeld(filepath.Join(src, "pulse.run.lock")); err != nil || held {
		return fmt.Errorf("checkpoint runner active or unavailable: %v", err)
	}
	c, err := readConfig(src)
	if err != nil {
		return err
	}
	if err = validateOwned(c); err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	if err = renameExclusive(src, dst); err != nil {
		return err
	}
	if err = syncDir(filepath.Dir(src)); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dst))
}
