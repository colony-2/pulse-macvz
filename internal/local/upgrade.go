package local

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/colony-2/pulse-macvz/internal/backend"
	"github.com/colony-2/pulse-macvz/internal/config"
)

type upgradeImage struct{ Name, UID string }
type imageUpgrade struct {
	Version      int
	Scope, Phase string
	Key          []byte
	Images       []upgradeImage
}
type preparedMover interface {
	MovePrepared(context.Context, string, string, bool) error
}

func (b *Client) upgradePath() string { return filepath.Join(b.dir, "image-upgrade.json") }
func (b *Client) upgradeDir() string  { return filepath.Join(b.dir, "previous-images") }
func (b *Client) readUpgrade() (*imageUpgrade, error) {
	data, err := privateFile(b.upgradePath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var u imageUpgrade
	if err = json.Unmarshal(data, &u); err != nil {
		return nil, err
	}
	if u.Version != 1 || u.Scope != b.scope || len(u.Key) != 32 || (u.Phase != "staging" && u.Phase != "staged" && u.Phase != "preparing" && u.Phase != "rollback" && u.Phase != "committed") {
		return nil, errors.New("invalid image upgrade checkpoint")
	}
	seen := map[string]bool{}
	for _, i := range u.Images {
		if !config.Name(i.Name) || i.UID == "" || seen[i.Name] {
			return nil, errors.New("invalid checkpoint image")
		}
		seen[i.Name] = true
	}
	return &u, nil
}
func (b *Client) saveUpgrade(u *imageUpgrade) error {
	data, err := json.Marshal(u)
	if err != nil {
		return err
	}
	return atomicPrivateFile(b.upgradePath(), data)
}

// UpgradePropertyKey keeps one property signing key across an empty inventory
// during replacement, so restored images and newly prepared sources agree.
func (b *Client) UpgradePropertyKey() ([]byte, error) {
	u, err := b.readUpgrade()
	if u == nil || err != nil {
		return nil, err
	}
	return u.Key, nil
}
func (b *Client) ImageUpgradeState() (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u, err := b.readUpgrade()
	if u == nil {
		return "", err
	}
	return u.Phase, err
}
func (b *Client) BeginImageUpgrade(ctx context.Context, names []string) error {
	if err := b.mu.LockContext(ctx); err != nil {
		return err
	}
	defer b.mu.Unlock()
	if err := b.readProperties(); err != nil {
		return err
	}
	mover, ok := b.hypervisor().(preparedMover)
	if !ok {
		return errors.New("backend cannot checkpoint prepared images")
	}
	u, err := b.readUpgrade()
	if err != nil {
		return err
	}
	if u != nil && u.Phase != "staging" {
		return nil
	}
	if u == nil {
		wanted := map[string]bool{}
		for _, name := range names {
			wanted[name] = true
		}
		u = &imageUpgrade{Version: 1, Scope: b.scope, Phase: "staging"}
		for name, r := range b.records {
			var p preparationProperties
			if err = json.Unmarshal(b.properties[name], &p); err != nil {
				return err
			}
			if len(p.Job) != 0 || len(p.Pair) != 0 || p.Slot != nil {
				return errors.New("drain work before checkpointing images")
			}
			if len(p.Key) == 32 {
				u.Key = p.Key
			}
			if !wanted[name] || p.Image == nil || p.Image.Phase != "ready" || p.Image.Kind != "base" {
				continue
			}
			if _, err = b.preparationIdentity(name, r.UID); err != nil {
				return err
			}
			v, err := b.get(ctx, name)
			if err != nil {
				return err
			}
			if !v.Stopped() || b.workers[name] {
				return fmt.Errorf("source %s is still active", name)
			}
			u.Images = append(u.Images, upgradeImage{Name: name, UID: r.UID})
		}
		// An empty pool has nothing to protect.
		if len(u.Images) == 0 {
			return nil
		}
		sort.Slice(u.Images, func(i, j int) bool { return u.Images[i].Name < u.Images[j].Name })
		if err = b.saveUpgrade(u); err != nil {
			return err
		}
	}
	for _, i := range u.Images {
		if err = b.checkpointIdentity(i); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if _, err = b.preparationIdentity(i.Name, i.UID); err != nil {
			return err
		}
		v, err := b.get(ctx, i.Name)
		if err != nil {
			return err
		}
		if !v.Stopped() || b.workers[i.Name] {
			return fmt.Errorf("source %s became active", i.Name)
		}
		if err = mover.MovePrepared(ctx, i.Name, b.upgradeDir(), false); err != nil {
			return err
		}
		delete(b.records, i.Name)
		delete(b.properties, i.Name)
	}
	u.Phase = "staged"
	return b.saveUpgrade(u)
}
func (b *Client) checkpointIdentity(i upgradeImage) error {
	data, err := privateFile(filepath.Join(b.upgradeDir(), i.Name, "pulse.json"))
	if err != nil {
		return err
	}
	var v vmFile
	if json.Unmarshal(data, &v) != nil || v.Scope != b.scope || v.Native == nil || v.Native.UID != i.UID || v.Native.Spec.Name != i.Name || !v.Native.Reusable {
		return errors.New("checkpoint identity mismatch")
	}
	var p preparationProperties
	u, err := b.readUpgrade()
	if err != nil {
		return err
	}
	if u == nil || json.Unmarshal(v.Properties, &p) != nil || p.Scope != b.scope || !bytes.Equal(p.Key, u.Key) || len(p.Job) != 0 || len(p.Pair) != 0 || p.Slot != nil || p.Image == nil || p.Image.Name != i.Name || p.Image.UID != i.UID || p.Image.Phase != "ready" || p.Image.Kind != "base" {
		return errors.New("checkpoint source properties mismatch")
	}
	return nil
}

// RollbackImageUpgrade restores only the saved sources. Every replacement must
// still have unused image properties. Active jobs and other scopes are untouched.
func (b *Client) RollbackImageUpgrade(ctx context.Context) (bool, error) {
	if err := b.mu.LockContext(ctx); err != nil {
		return false, err
	}
	defer b.mu.Unlock()
	if err := b.readProperties(); err != nil {
		return false, err
	}
	u, err := b.readUpgrade()
	if err != nil || u == nil {
		return true, err
	}
	if u.Phase == "committed" {
		return true, errors.New("upgrade already committed")
	}
	mover, ok := b.hypervisor().(preparedMover)
	if !ok {
		return false, errors.New("backend cannot restore checkpoint")
	}
	u.Phase = "rollback"
	if err = b.saveUpgrade(u); err != nil {
		return false, err
	}
	for _, i := range u.Images {
		r := b.records[i.Name]
		if r != nil && r.UID == i.UID {
			p, identityErr := b.preparationIdentity(i.Name, i.UID)
			if identityErr != nil {
				return false, identityErr
			}
			v, observeErr := b.get(ctx, i.Name)
			if observeErr != nil {
				return false, observeErr
			}
			if p.Image == nil || p.Image.Phase != "ready" || !v.Stopped() {
				return false, fmt.Errorf("restored source %s no longer ready", i.Name)
			}
			continue
		} // Already restored, or staging had not moved it.
		if err = b.checkpointIdentity(i); err != nil {
			return false, err
		}
		if r != nil {
			if _, err = b.preparationIdentity(i.Name, r.UID); err != nil {
				return false, err
			}
			stopped, err := b.quiescePreparation(ctx, i.Name, r.UID)
			if err != nil {
				return false, err
			}
			if !stopped {
				return false, nil
			}
			if err = b.hypervisor().Delete(ctx, i.Name); err != nil {
				return false, err
			}
			delete(b.records, i.Name)
			delete(b.properties, i.Name)
		}
		if err = mover.MovePrepared(ctx, i.Name, b.upgradeDir(), true); err != nil {
			return false, err
		}
		if err = b.readProperties(); err != nil {
			return false, err
		}
	}
	if err = b.clearPreparationRetries(); err != nil {
		return false, err
	}
	return true, b.clearUpgrade()
}
func (b *Client) CommitImageUpgrade(ctx context.Context) error {
	if err := b.mu.LockContext(ctx); err != nil {
		return err
	}
	defer b.mu.Unlock()
	u, err := b.readUpgrade()
	if err != nil {
		return err
	}
	if u == nil {
		return b.clearPreparationRetries()
	}
	if u.Phase == "rollback" || u.Phase == "staging" || u.Phase == "staged" {
		return errors.New("upgrade has not finished staging or rollback")
	}
	// Caller verifies all configured capacity. Independently require a ready,
	// stopped replacement for each saved base before deleting its checkpoint.
	for _, i := range u.Images {
		r := b.records[i.Name]
		if r == nil {
			return fmt.Errorf("replacement absent: %s", i.Name)
		}
		p, err := b.preparationIdentity(i.Name, r.UID)
		if err != nil {
			return err
		}
		if p.Image == nil || p.Image.Phase != "ready" {
			return fmt.Errorf("replacement not ready: %s", i.Name)
		}
		v, err := b.get(ctx, i.Name)
		if err != nil {
			return err
		}
		if !v.Stopped() {
			return backend.ErrBusy
		}
	}
	u.Phase = "committed"
	if err = b.saveUpgrade(u); err != nil {
		return err
	}
	if err = b.clearUpgrade(); err != nil {
		return err
	}
	return b.clearPreparationRetries()
}
func (b *Client) clearUpgrade() error {
	if err := os.RemoveAll(b.upgradeDir()); err != nil {
		return err
	}
	if err := os.Remove(b.upgradePath()); err != nil {
		return err
	}
	dir, err := os.Open(b.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (b *Client) clearPreparationRetries() error {
	dir := filepath.Join(b.dir, "preparation-retries")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := privateFile(path)
		if err != nil {
			return err
		}
		var r preparationRetries
		if json.Unmarshal(data, &r) == nil && r.Count > 0 {
			if err = os.Remove(path); err != nil {
				return err
			}
		}
	}
	return nil
}

// ActivateImageUpgrade records that retirement finished. A crash before this
// transition resumes draining/retirement, rather than leaving abandoned builders.
func (b *Client) ActivateImageUpgrade(ctx context.Context) error {
	if err := b.mu.LockContext(ctx); err != nil {
		return err
	}
	defer b.mu.Unlock()
	u, err := b.readUpgrade()
	if err != nil {
		return err
	}
	if u == nil {
		return b.clearPreparationRetries()
	}
	if u.Phase != "staged" {
		return errors.New("upgrade images are not staged")
	}
	if err = b.clearPreparationRetries(); err != nil {
		return err
	}
	u.Phase = "preparing"
	return b.saveUpgrade(u)
}
