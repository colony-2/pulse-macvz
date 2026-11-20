package local

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestVZEndpointUsesRunnerBundleIdentity(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS plist extraction")
	}
	root := t.TempDir()
	app := filepath.Join(root, "Runner.app")
	executable := filepath.Join(app, "Contents", "MacOS", "pulse-vz")
	if err := os.MkdirAll(filepath.Dir(executable), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	info := filepath.Join(app, "Contents", "Info.plist")
	if err := os.WriteFile(info, []byte(`<?xml version="1.0"?><plist version="1.0"><dict><key>CFBundleIdentifier</key><string>dev.pulse.fixture</string></dict></plist>`), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "runner")
	if err := os.Symlink(executable, alias); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"xml1", "binary1"} {
		if out, err := exec.Command("/usr/bin/plutil", "-convert", format, info).CombinedOutput(); err != nil {
			t.Fatalf("convert plist: %v %s", err, out)
		}
		id, err := endpointBundleIdentifier(t.Context(), alias)
		if err != nil || id != "dev.pulse.fixture" {
			t.Fatalf("%s bundle id: %q %v", format, id, err)
		}
	}
	b, _, _ := fixture(t)
	b.kind = "vz"
	b.endpointArgs = func(string, string, int) []string { return []string{alias} }
	missing := missingService(t)
	b.service = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "print" {
			return nil, missing
		}
		if args[0] != "bootstrap" {
			t.Fatalf("unexpected operation %v", args)
		}
		data, err := os.ReadFile(args[2])
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "<key>AssociatedBundleIdentifiers</key><array><string>dev.pulse.fixture</string></array>") {
			t.Fatal("launch agent lost its owning bundle")
		}
		return nil, nil
	}
	if _, err := b.ensureEndpoint(t.Context(), "owned", "127.0.0.1:2222", 45100); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(info); err != nil {
		t.Fatal(err)
	}
	if _, err := endpointBundleIdentifier(t.Context(), alias); err == nil {
		t.Fatal("invalid app silently treated as a standalone executable")
	}
}
