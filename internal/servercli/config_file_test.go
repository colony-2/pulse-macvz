package servercli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestInstalledConfigurationRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, "settings", "provider.json")
	setup := providerCommand("setup", "test", func(_ context.Context, o serverOptions) error {
		if o.vmHome != filepath.Join(home, ".pulse", "vz") {
			t.Fatal("wrong storage default", o.vmHome)
		}
		if *o.config.VMCounts.Total != 1 {
			t.Fatal("explicit capacity lost")
		}
		return nil
	})
	setup.SetArgs([]string{"--config", path, "--linux-capacity", "0", "--macos-capacity", "1", "--total-capacity", "1"})
	if err := setup.Execute(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("configuration permissions", info, err)
	}
	server := newCommand(func(_ context.Context, o serverOptions) error {
		if o.capacity.macOS != 0 || o.capacity.linux != 2 || *o.config.VMCounts.Total != 1 {
			t.Fatal("saved defaults/explicit overrides lost", o.capacity)
		}
		return nil
	})
	server.SetArgs([]string{"server", "--config", path, "--macos-capacity", "0", "--linux-capacity", "2", "--mode", "inbound"})
	if err := server.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigurationKeepsAutomaticTotalAndRejectsInvalidFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.json")
	setup := providerCommand("setup", "test", func(context.Context, serverOptions) error { return nil })
	setup.SetArgs([]string{"--config", path, "--macos-capacity", "0", "--linux-capacity", "1"})
	if err := setup.Execute(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var saved savedConfiguration
	_ = json.Unmarshal(data, &saved)
	if _, exists := saved.Flags["total-capacity"]; exists {
		t.Fatal("automatic total was frozen")
	}
	for _, body := range []string{`{"version":2,"flags":{}}`, `{"version":1,"flags":{"unknown":"x"}}`, `{"version":1,"flags":{}} {}`, `{"version":1,"flags":{"timeout":"2h"}}`} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		cmd := newCommand(func(context.Context, serverOptions) error { t.Fatal("invalid file reached provider"); return nil })
		cmd.SetArgs([]string{"server", "--config", path})
		if cmd.Execute() == nil {
			t.Fatal("accepted invalid configuration", body)
		}
	}
}

func TestConfigurationRelativePathsAndFlags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"flags":{"mode":"inbound","vm-home":"vms","linux-capacity":"0"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := newCommand(func(_ context.Context, o serverOptions) error {
		if o.vmHome != filepath.Join(filepath.Dir(path), "vms") {
			t.Fatal(o.vmHome)
		}
		return nil
	})
	cmd.SetArgs([]string{"server", "--config", path})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultConfigurationIsLoaded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, ".pulse"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".pulse", "provider.json"), []byte(`{"version":1,"flags":{"mode":"inbound","macos-capacity":"0","linux-capacity":"2"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := newCommand(func(_ context.Context, o serverOptions) error {
		if o.mode != "inbound" || o.capacity.macOS != 0 || o.capacity.linux != 2 || *o.config.VMCounts.Total != 2 {
			t.Fatal("default configuration ignored")
		}
		return nil
	})
	cmd.SetArgs([]string{"server"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestManagementHelpNeverCreatesStorage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{"setup", "upgrade"} {
		cmd := newCommand(func(context.Context, serverOptions) error { t.Fatal("help started provider"); return nil })
		cmd.SetOut(io.Discard)
		cmd.SetArgs([]string{name, "--help"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".pulse")); !os.IsNotExist(err) {
		t.Fatal("help created state")
	}
}
