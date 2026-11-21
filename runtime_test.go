package macvz

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/colony-2/pulse-macvz/internal/backend"
	"github.com/colony-2/pulse-macvz/internal/bundle"
	"github.com/colony-2/pulse-macvz/internal/config"
	engine "github.com/colony-2/pulse-macvz/internal/provider"
	api "github.com/colony-2/pulse-provider-api"
)

func TestSubmitLoggingClosedDoesNotReplaceAdmissionError(t *testing.T) {
	r := &Runtime{closed: true}
	results, sub, err := r.Submit(t.Context(), []api.Launch{{LaunchID: "launch"}}, api.SubmitOptions{Listener: &api.ListenOptions{}})
	if results != nil || !errors.Is(err, ErrClosed) || sub == nil {
		t.Fatal(results, sub, err)
	}
	logs, logErr := sub.Wait(t.Context())
	if logs.Reason != api.ListenerProviderClosed || !errors.Is(logErr, api.ErrProviderClosed) {
		t.Fatal(logs, logErr)
	}
	_, sub, err = r.Submit(t.Context(), nil, api.SubmitOptions{})
	if !errors.Is(err, ErrClosed) || sub != nil {
		t.Fatal(sub, err)
	}
}

type failingRecoveryBackend struct {
	engine.Backend
	recovery, inventory, health error
	calls                       []string
}

func (b *failingRecoveryBackend) Recover(context.Context) error {
	b.calls = append(b.calls, "recover")
	return b.recovery
}
func (b *failingRecoveryBackend) List(context.Context) ([]backend.VM, error) {
	b.calls = append(b.calls, "reconcile")
	return nil, b.inventory
}
func (b *failingRecoveryBackend) DeliveryHealth(context.Context) error {
	b.calls = append(b.calls, "health")
	return b.health
}

func TestRecoveryFailureStillReconcilesAndReportsDeliveryHealth(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := engine.OpenStore(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	b := &failingRecoveryBackend{recovery: errors.New("recovery failed"), inventory: errors.New("inventory failed"), health: errors.New("delivery failed")}
	r := &Runtime{p: &engine.Provider{Store: store, Backend: b}}
	defer func() {
		if r.archive != nil {
			r.archive.Close()
		}
	}()
	err = r.Reconcile(t.Context())
	for _, cause := range []error{b.recovery, b.inventory, b.health} {
		if !errors.Is(err, cause) {
			t.Fatalf("lost error %v: %v", cause, err)
		}
	}
	if !reflect.DeepEqual(b.calls, []string{"recover", "reconcile", "health"}) {
		t.Fatal(b.calls)
	}
	r.closed = true
	if err := r.DeliveryHealth(t.Context()); err != ErrClosed {
		t.Fatal(err)
	}
	if _, err := r.DeliveryObservations(t.Context()); err != ErrClosed {
		t.Fatal(err)
	}
}

func testConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	return Config{OwnerID: "library", StateDir: dir, Bases: []Base{{Name: "macos-sequoia-15.7.7-arm64", Image: DefaultBases()[1].Image, OSVersion: "15.7.7", SSHPassword: "admin", Capabilities: []string{"git", "nix"}}}, Slots: []SlotGroup{{Name: "standard", Count: 1, Base: "macos-sequoia-15.7.7-arm64"}}}
}
func TestSharedConfigAndCopies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode([]any{}) }))
	defer server.Close()
	c := testConfig(t)
	r, e := newTestRuntime(c)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	if r.p.Config.Slots[0].CPU != 4 || r.p.Config.Slots[0].MemoryMiB != 8192 {
		t.Fatal("wrong defaults")
	}
	c.Bases[0].Capabilities[0] = "modified"
	c.Slots[0].Count = 0
	if r.p.Config.Bases[0].Capabilities[0] != "git" || r.p.Config.Slots[0].Count != 1 {
		t.Fatal("retained mutable config")
	}
	if e = r.Close(); e != nil {
		t.Fatal(e)
	}
	if e = r.Reconcile(context.Background()); e != ErrClosed {
		t.Fatal("closed runtime used")
	}
}
func TestEmbeddedBundleMatchesPreparationScript(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "bundle.tar.gz")
	cmd := exec.Command("bash", "scripts/build-bundle.sh", dest, "--c2j", strings.Repeat("a", 40))
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("%v: %s", e, out)
	}
	unpack := func(b []byte) map[string]string {
		t.Helper()
		gz, e := gzip.NewReader(bytes.NewReader(b))
		if e != nil {
			t.Fatal(e)
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		files := map[string]string{}
		for {
			h, e := tr.Next()
			if e == io.EOF {
				break
			}
			if e != nil {
				t.Fatal(e)
			}
			data, e := io.ReadAll(tr)
			if e != nil {
				t.Fatal(e)
			}
			files[h.Name] = string(data)
		}
		return files
	}
	a, e := bundle.Build(assets, "default", strings.Repeat("a", 40))
	if e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(dest)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(unpack(a), unpack(b)) {
		t.Fatal("library and image preparation environments differ")
	}
	if strings.Contains(string(a), "LEASE-SENTINEL") {
		t.Fatal("launch payload embedded")
	}
}

func TestMaintenanceDoesNotConflateHistoricalHealthWithReconciliation(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := engine.OpenStore(dir, "maintenance")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	b := &failingRecoveryBackend{inventory: errors.New("inventory failed"), health: errors.New("historical delivery failed")}
	r := &Runtime{p: &engine.Provider{Store: store, Backend: b}}
	defer func() {
		if r.archive != nil {
			r.archive.Close()
		}
	}()
	err = r.ReconcileMaintenance(t.Context())
	if !errors.Is(err, b.inventory) || errors.Is(err, b.health) {
		t.Fatal("maintenance lost reconciliation error or included stale health", err)
	}
	if !reflect.DeepEqual(b.calls, []string{"recover", "reconcile"}) {
		t.Fatal(b.calls)
	}
	if err := r.DeliveryHealth(t.Context()); !errors.Is(err, b.health) {
		t.Fatal("delivery health hidden", err)
	}
}

// No platform VM or transport is needed to test the public runtime contract.
func newTestRuntime(c Config) (*Runtime, error) {
	c, err := prepare(c)
	if err != nil {
		return nil, err
	}
	settings := config.Settings{Config: c, Kind: "vz", Endpoint: "test", Worker: "local"}
	store, err := engine.OpenStore(c.StateDir, settings.Scope())
	if err != nil {
		return nil, err
	}
	return newRuntime(engine.Attach(settings, &failingRecoveryBackend{}, store), c), nil
}
