package local

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"time"

	"github.com/colony-2/pulse-macvz/internal/backend"
	"github.com/colony-2/pulse-macvz/internal/config"
)

// AdoptSource registers a configured, named source as read-only. It deliberately
// does not claim its directory: the standalone updater may replace that source.
// Job clones continue to use strict UID/directory/hardware ownership binding.
func (b *Client) AdoptSource(ctx context.Context, s backend.Spec, hostKey []byte) (backend.VM, error) {
	if e := b.mu.LockContext(ctx); e != nil {
		return backend.VM{}, e
	}
	defer b.mu.Unlock()
	if !config.Name(s.Name) {
		return backend.VM{}, failure("source identity/hardware/state condition failed", nil)
	}
	if existing := b.records[s.Name]; existing != nil {
		if _, err := os.Lstat(b.path(s.Name)); os.IsNotExist(err) && existing.Reusable {
			// A deliberately deleted published source may be prepared again.
			// This exception never applies to an uncertain job creation.

			delete(b.records, s.Name)
			delete(b.properties, s.Name)
			return backend.VM{}, &backend.Error{Code: 404}
		}
		return b.get(ctx, s.Name)
	}
	if _, e := os.Lstat(b.path(s.Name)); os.IsNotExist(e) {
		return backend.VM{}, &backend.Error{Code: 404}
	}
	h, e := b.inspect(ctx, s.Name)
	if e != nil {
		return backend.VM{}, failure("adopt inspect", e)
	}
	if h.CPU != s.CPU || h.Memory != s.Memory || (h.State != "stopped" && h.State != "suspended") {
		return backend.VM{}, failure("source identity/hardware/state condition failed", nil)
	}
	var id [16]byte
	if _, e = rand.Read(id[:]); e != nil {
		return backend.VM{}, failure("source random identity", e)
	}
	r := &record{Spec: s, UID: hex.EncodeToString(id[:]), Phase: "prepared", Reusable: true, Delivered: true, MAC: h.MACAddress, Created: time.Now().UTC()}
	r.HostKey = append([]byte(nil), hostKey...)
	if e = b.save(r); e != nil {
		return backend.VM{}, e
	}
	b.records[s.Name] = r
	return b.get(ctx, s.Name)
}
func (b *Client) RetainSource(ctx context.Context, name, uid string) error {
	if e := b.mu.LockContext(ctx); e != nil {
		return e
	}
	defer b.mu.Unlock()
	r := b.records[name]
	if r == nil || r.UID != uid {
		return failure("retain source UID mismatch", nil)
	}
	r.Reusable = true
	r.Delivered = true
	return b.save(r)
}
