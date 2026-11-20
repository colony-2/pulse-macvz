package local

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/colony-2/pulse-tart/internal/config"
	"golang.org/x/sys/unix"
)

// pulse.json is host-side metadata attached to one VM, never a second inventory.
// It contains private SSH/delivery data and must not be exported in diagnostics.
type vmFile struct {
	Version    int             `json:"version"`
	Scope      string          `json:"scope"`
	Native     *record         `json:"native"`
	Properties json.RawMessage `json:"properties,omitempty"`
}

func (b *Client) artifact(name, kind string) string {
	return filepath.Join(b.path(name), "pulse."+kind)
}

func (b *Client) lock(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return fmt.Errorf("another runtime owns this %s pool: %w", b.kind, err)
	}
	b.locks = append(b.locks, f)
	return nil
}

func (b *Client) openProperties() error {
	if err := os.MkdirAll(b.dir, 0700); err != nil {
		return err
	}
	if err := b.lock(filepath.Join(b.dir, "lock")); err != nil {
		return err
	}
	// Coordinate migration with an old Pulse binary as well as with new ones.
	if b.legacyDir != "" {
		if _, err := os.Stat(b.legacyDir); err == nil {
			if err := b.lock(filepath.Join(b.legacyDir, "lock")); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if err := b.readProperties(); err != nil {
		return err
	}
	return b.importNative()
}

func privateFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("VM properties must be private and regular: %s", path)
	}
	return os.ReadFile(path)
}

func (b *Client) readProperties() error {
	entries, err := os.ReadDir(filepath.Join(b.cfg.Home, "vms"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	next := map[string]*record{}
	props := map[string]json.RawMessage{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := b.artifact(entry.Name(), "json")
		data, err := privateFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		var file vmFile
		if err = json.Unmarshal(data, &file); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
		if file.Scope != b.scope {
			continue
		}
		r := file.Native
		if file.Version != 1 || r == nil || !config.Name(r.Spec.Name) || r.Spec.Name != entry.Name() || r.UID == "" {
			return fmt.Errorf("invalid VM properties: %s", path)
		}
		if old := b.records[r.Spec.Name]; old != nil && old.UID == r.UID {
			*old = *r // Delivery workers retain this pointer; updates hold b.mu.
			r = old
		}
		next[r.Spec.Name] = r
		props[r.Spec.Name] = bytes.Clone(file.Properties)
	}
	b.records, b.properties = next, props
	return nil
}

// Legacy files are read-only inputs. Only existing VM directories with matching
// ownership are migrated; absent VMs never become reservations again.
func (b *Client) importNative() error {
	if b.legacyDir == "" {
		return nil
	}
	dir := filepath.Join(b.legacyDir, "native")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	validateLegacy := func() error {
		data, err := privateFile(filepath.Join(b.legacyDir, "state.json"))
		if err != nil {
			return fmt.Errorf("legacy migration needs the matching provider journal: %w", err)
		}
		var header struct {
			Scope   string
			Version int
		}
		if err = json.Unmarshal(data, &header); err != nil {
			return err
		}
		if header.Scope != b.scope || header.Version != 3 {
			return fmt.Errorf("legacy migration scope/version mismatch")
		}
		return nil
	}
	validated := false
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".json")
		if !config.Name(name) {
			return fmt.Errorf("invalid legacy VM name")
		}
		if b.records[name] != nil {
			continue
		}
		if _, err := os.Lstat(b.path(name)); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		// Never overwrite another owner's attached metadata.
		if _, err := os.Lstat(b.artifact(name, "json")); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if !validated {
			if err := validateLegacy(); err != nil {
				return err
			}
			validated = true
		}
		data, err := privateFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return err
		}
		var r record
		if err = json.Unmarshal(data, &r); err != nil {
			return err
		}
		if r.Spec.Name != name || r.UID == "" {
			return fmt.Errorf("invalid legacy native identity")
		}
		if !r.Reusable {
			if err = b.binding(&r, false); err != nil {
				return err
			}
		}
		// Copy observations before publishing pulse.json, making interrupted
		// migration retryable without ever restarting a native launch.
		for _, suffix := range []string{"exit", "events", "run.log"} {
			data, err := os.ReadFile(filepath.Join(dir, name+"."+suffix))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			if err = atomicPrivateFile(b.artifact(name, suffix), data); err != nil {
				return err
			}
		}
		b.records[name] = &r
		if err = b.save(&r); err != nil {
			return err
		}
	}
	return nil
}

func atomicPrivateFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".pulse-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (b *Client) save(r *record) error {
	info, err := os.Lstat(b.path(r.Spec.Name))
	if os.IsNotExist(err) {
		return nil
	} // Pre-clone intent is only in memory.
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return failure("VM properties parent is not a directory", nil)
	}
	data, err := json.Marshal(vmFile{Version: 1, Scope: b.scope, Native: r, Properties: b.properties[r.Spec.Name]})
	if err != nil {
		return err
	}
	path := b.artifact(r.Spec.Name, "json")
	if old, err := privateFile(path); err == nil && bytes.Equal(old, data) {
		return nil
	}
	return atomicPrivateFile(path, data)
}

func (b *Client) ReadVMProperties() (map[string]json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.readProperties(); err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{}
	for name := range b.records {
		out[name] = bytes.Clone(b.properties[name])
	}
	return out, nil
}

func (b *Client) WriteVMProperties(props map[string]json.RawMessage) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for name := range props {
		if !config.Name(name) {
			return failure("invalid VM property name", nil)
		}
	}
	for name, r := range b.records {
		if _, err := os.Lstat(b.path(name)); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if !r.Reusable && r.Device != 0 {
			if err := b.binding(r, false); err != nil {
				return err
			}
		}
		// A live delivery worker can write native progress concurrently; always
		// combine the latest native record and properties under the same lock.
		b.properties[name] = bytes.Clone(props[name])
		if err := b.save(r); err != nil {
			return err
		}
	}
	b.staged = map[string]json.RawMessage{}
	for name, raw := range props {
		if b.records[name] == nil {
			b.staged[name] = bytes.Clone(raw)
		}
	}
	return nil
}
