package tart

import (
	"context"
	"errors"
	api "github.com/colony-2/pulse-provider-api"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/colony-2/pulse-tart/internal/config"
	"github.com/colony-2/pulse-tart/internal/vz"
)

func TestVZConstructorAndScope(t *testing.T) {
	c := testConfig(t)
	c.StateDir = ""
	opts := VZConfig{Home: filepath.Join(t.TempDir(), "vz"), Runner: os.Args[0]}
	r, err := NewVZ(c, opts)
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		if !errors.Is(err, vz.ErrPlatform) {
			t.Fatal(err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.p.Config.Kind != "vz" || r.p.Config.Endpoint != opts.Home {
		t.Fatal(r.p.Config.Kind, r.p.Config.Endpoint)
	}
	tartScope := (config.Settings{Config: r.p.Config.Config, Kind: "tart", Endpoint: opts.Home}).Scope()
	if r.p.Config.Scope() == tartScope {
		t.Fatal("VZ shares Tart ownership scope")
	}
	if entries, err := os.ReadDir(filepath.Join(opts.Home, "vms")); err == nil && len(entries) != 0 {
		t.Fatal("constructor created VMs")
	}
	if _, err = NewVZ(c, opts); err == nil {
		t.Fatal("duplicate owner admitted")
	}
	c.Slots[0].Count = 0
	if r.p.Config.Slots[0].Count != 1 {
		t.Fatal("retained caller config")
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = r.List(context.Background(), api.ListRequest{}); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	reopened, err := NewVZ(c, opts)
	if err != nil {
		t.Fatal(err)
	}
	reopened.Close()
}
