package launchlogs

import (
	"context"
	"encoding/json"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colony-2/pulse-macvz/guest/logstore"
	"github.com/colony-2/pulse-macvz/internal/backend"
	"github.com/colony-2/pulse-macvz/internal/config"
	"github.com/colony-2/pulse-macvz/internal/provider"
)

type archiveBackend struct {
	provider.Backend
	calls int
	page  logstore.Page
}

func (b *archiveBackend) Get(context.Context, string) (backend.VM, error) {
	return backend.VM{UID: "identity"}, nil
}
func (b *archiveBackend) Exec(_ context.Context, _, _ string, _ []byte) ([]byte, error) {
	b.calls++
	return json.Marshal(b.page)
}
func testArchive(t *testing.T, b *archiveBackend) *Archive {
	t.Helper()
	c, _ := (config.Logging{}).Normalize()
	return &Archive{root: t.TempDir(), config: c, provider: &provider.Provider{Backend: b}, phases: map[string]string{}, failures: map[string]error{}}
}
func TestArchiveResumesCursorAndBindsIdentity(t *testing.T) {
	data := []byte("hello")
	b := &archiveBackend{page: logstore.Page{Manifest: logstore.Manifest{Version: 1, LaunchID: "launch", End: 5, Closed: true}, Frames: []logstore.Frame{{Data: data, CRC: crc32.ChecksumIEEE(data)}}, Next: 5}}
	a := testArchive(t, b)
	target := provider.LogTarget{LaunchID: "launch", Name: "vm", UID: "identity", Phase: "active"}
	a.collect(t.Context(), target)
	p, _, e := a.Read(t.Context(), "launch", 0)
	if e != nil || p.Gap || !p.Manifest.Closed || len(p.Frames) != 1 {
		t.Fatal(p, e)
	}
	reopened := testArchive(t, b)
	reopened.root = a.root
	calls := b.calls
	reopened.collect(t.Context(), target)
	if b.calls != calls {
		t.Fatal("completed archive queried VM again")
	}
	// An incomplete copy may resume only the same immutable VM identity.
	p.Manifest.Closed = false
	if e := logstore.Save(logstore.Dir(a.root, "launch"), p.Manifest); e != nil {
		t.Fatal(e)
	}
	target.UID = "replacement"
	reopened.collect(t.Context(), target)
	if reopened.failures["launch"] == nil || b.calls != calls {
		t.Fatal("reused identity accepted")
	}
}
func TestRetentionAndPrivacy(t *testing.T) {
	a := testArchive(t, &archiveBackend{})
	a.config.MaxAge = time.Minute
	dir := logstore.Dir(a.root, "old")
	m := logstore.Manifest{Version: 1, LaunchID: "old", Closed: true, Updated: time.Now().Add(-time.Hour)}
	raw, _ := json.Marshal(m)
	if e := logstore.Atomic(dir, "manifest.json", raw); e != nil {
		t.Fatal(e)
	}
	a.clean()
	if _, e := os.Stat(dir); !os.IsNotExist(e) {
		t.Fatal("expired archive retained", e)
	}
	target := provider.LogTarget{LaunchID: "new", UID: "identity"}
	if e := os.Chmod(a.root, 0755); e != nil {
		t.Fatal(e)
	}
	a.collect(t.Context(), target)
	if a.failures["new"] == nil {
		t.Fatal("public archive root accepted")
	}
}
func TestArchiveBudgetInterruptsActiveStream(t *testing.T) {
	a := testArchive(t, &archiveBackend{})
	a.config.MaxBytes = 128
	dir := logstore.Dir(a.root, "active")
	if e := logstore.Save(dir, logstore.Manifest{LaunchID: "active", End: 1024}); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, "output.log"), make([]byte, 1024), 0600); e != nil {
		t.Fatal(e)
	}
	a.clean()
	p, _, e := a.Read(t.Context(), "active", 0)
	if e == nil || p.Manifest.Closed {
		t.Fatal("eviction silently reported as closure", e)
	}
}
