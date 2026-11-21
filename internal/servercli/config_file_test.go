package servercli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestEmbeddedConfigurationExcludesParentFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "provider.json")
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "host context")
	var output bytes.Buffer
	var hostToken string
	hookCalled, runCalled := false, false
	root := &cobra.Command{Use: "pulse", Version: "host-version", PersistentPreRun: func(*cobra.Command, []string) { hookCalled = true }}
	root.PersistentFlags().StringVar(&hostToken, "host-token", "", "Host-only option")
	root.SetErr(&output)
	setup := providerCommand("setup", "test", func(got context.Context, o serverOptions) error {
		runCalled = true
		if !hookCalled || got.Value(contextKey{}) != "host context" || o.log != &output || o.version != "host-version" {
			t.Fatal("host hook, context, output, or version was lost")
		}
		return nil
	})
	root.AddCommand(setup)
	root.SetArgs([]string{"setup", "--config", path, "--host-token", "host-secret"})
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	if !runCalled || hostToken != "host-secret" {
		t.Fatal("embedded command or host flag did not run")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "host-token") || strings.Contains(string(data), "host-secret") {
		t.Fatal("provider configuration persisted a host flag")
	}
	if !strings.Contains(output.String(), "Configuration saved:") {
		t.Fatal("configuration output did not reach host stream")
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"flags":{"host-token":"override"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadConfiguration(setup, &serverOptions{configPath: path}); err == nil || !strings.Contains(err.Error(), "unsupported saved option --host-token") {
		t.Fatalf("provider configuration may alter host flags: %v", err)
	}
}

func TestInstalledConfigurationRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, "settings", "provider.json")
	setup := providerCommand("setup", "test", func(_ context.Context, o serverOptions) error {
		for _, g := range o.config.Slots {
			if g.Count > 0 && g.DockerProfile == "" {
				t.Fatal("default slot omitted Docker eligibility")
			}
		}
		if o.vmHome != filepath.Join(home, ".pulse", "vz") {
			t.Fatal("wrong storage default", o.vmHome)
		}
		if *o.config.VMCounts.Total != 1 {
			t.Fatal("explicit capacity lost")
		}
		return nil
	})
	setup.SetArgs([]string{"--config", path, "--linux-capacity", "0", "--macos-capacity", "1", "--total-capacity", "1", "--progress", "linear"})
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

func TestConfigurationLocationMigration(t *testing.T) {
	for _, tc := range []struct {
		name, modern, legacy, explicit string
		wantOwner                      string
		warning, wantError             bool
	}{
		{name: "fresh defaults", wantOwner: "server"},
		{name: "legacy fallback", legacy: `{"version":1,"flags":{"owner":"old","state-dir":"relative","backend":"vz","tart-binary":"tart","orchard-url":"","orchard-allow-http":"false"}}`, wantOwner: "old", warning: true},
		{name: "new takes precedence", modern: `{"version":1,"flags":{"owner":"new"}}`, legacy: `{"version":1,"flags":{"owner":"old"}}`, wantOwner: "new"},
		{name: "invalid new never falls back", modern: `invalid`, legacy: `{"version":1,"flags":{}}`, wantError: true},
		{name: "explicit path never falls back", legacy: `{"version":1,"flags":{"owner":"old"}}`, explicit: "custom.json", wantOwner: "server"},
		{name: "removed backend rejected", legacy: `{"version":1,"flags":{"backend":"tart"}}`, warning: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			modern := filepath.Join(home, ".pulse", "vz", "provider.json")
			legacy := filepath.Join(home, ".pulse", "provider.json")
			if err := os.MkdirAll(filepath.Dir(modern), 0700); err != nil {
				t.Fatal(err)
			}
			for path, body := range map[string]string{modern: tc.modern, legacy: tc.legacy} {
				if body != "" {
					if err := os.WriteFile(path, []byte(body), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			called := false
			var stderr bytes.Buffer
			cmd := providerCommand("setup", "test", func(_ context.Context, o serverOptions) error {
				called = true
				if o.owner != tc.wantOwner {
					t.Fatalf("owner=%s want %s", o.owner, tc.wantOwner)
				}
				if tc.name == "legacy fallback" && o.stateDir != filepath.Join(home, ".pulse", "relative") {
					t.Fatalf("legacy relative path changed: %s", o.stateDir)
				}
				return nil
			})
			cmd.SetErr(&stderr)
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			args := []string{}
			if tc.explicit != "" {
				args = []string{"--config", filepath.Join(home, tc.explicit)}
			}
			cmd.SetArgs(args)
			err := cmd.Execute()
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v", err)
			}
			if called == tc.wantError {
				t.Fatalf("run called=%v", called)
			}
			if strings.Contains(stderr.String(), "Warning: using legacy") != tc.warning {
				t.Fatalf("warning: %s", stderr.String())
			}
			if tc.legacy != "" {
				data, _ := os.ReadFile(legacy)
				if string(data) != tc.legacy {
					t.Fatal("legacy file changed")
				}
			}
			if !tc.wantError && tc.explicit == "" {
				data, err := os.ReadFile(modern)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(data), "tart-binary") || strings.Contains(string(data), "orchard-") {
					t.Fatal("removed defaults persisted")
				}
			}
		})
	}
}
