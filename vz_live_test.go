package tart

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse-tart/internal/backend"
	"golang.org/x/sys/unix"
)

func prepareVZLiveBase(t *testing.T, ctx context.Context, r *Runtime, base Base, source, name string) {
	t.Helper()
	home := os.Getenv("PULSE_VZ_HOME")
	if !filepath.IsAbs(home) {
		t.Fatal("PULSE_VZ_HOME must be an isolated absolute directory")
	}
	input := name + "-fixture-input"
	seedVZLiveBase(t, source, filepath.Join(home, "vms", input))
	b := r.p.Backend.(interface {
		AdoptSource(context.Context, backend.Spec, []byte) (backend.VM, error)
		RetainSource(context.Context, string, string) error
		Stop(context.Context, string, string) error
		Events(context.Context, string) ([]backend.Event, error)
	})
	spec := backend.Spec{Name: input, Image: base.Image, OS: "darwin", Arch: "arm64", Runtime: "vz", CPU: 4, Memory: 8192, Username: base.SSHUsername, Password: string(base.SSHPassword), Headless: true, Suspendable: true}
	if _, err := b.AdoptSource(ctx, spec, base.SSHHostKey); err != nil {
		t.Fatal(err)
	}
	setup, err := r.p.Setup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	spec.Name, spec.Image = name, input
	spec.StartupScript.Content = fmt.Sprintf(`#!/bin/bash
set -euo pipefail
set +x
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
/usr/bin/base64 -D > "$work/setup.tar.gz" <<'PULSE_SETUP'
%s
PULSE_SETUP
/usr/bin/tar -xzf "$work/setup.tar.gz" -C "$work"
echo PULSE_HANDOFF_OK
sudo -n /bin/bash "$work/prepare-image.sh" --shutdown "$work"
`, base64.StdEncoding.EncodeToString(setup))
	v, err := r.p.Backend.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	complete := false
	defer func() {
		if !complete {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := b.Stop(cleanup, v.Name, v.UID); err != nil {
				t.Errorf("stop failed integration builder: %v", err)
			}
		}
	}()
	prepared := false
	waitLive(t, ctx, "VZ integration fixture base", 30*time.Minute, func(ctx context.Context) bool {
		events, err := b.Events(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		var output strings.Builder
		for _, event := range events {
			output.WriteString(event.Payload)
			prepared = prepared || strings.Contains(event.Payload, "PULSE_IMAGE_PREPARED")
		}
		got, err := r.p.Backend.Get(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if got.Stopped() && !prepared {
			text := output.String()
			if len(text) > 3000 {
				text = text[len(text)-3000:]
			}
			t.Fatalf("integration builder stopped before readiness: %s", text)
		}
		return got.Stopped() && prepared
	})
	if err := b.RetainSource(ctx, v.Name, v.UID); err != nil {
		t.Fatal(err)
	}
	if err := r.p.Backend.Delete(ctx, input); err != nil {
		t.Fatal(err)
	}
	complete = true
}

// The opt-in public API test may reuse an already prepared, cold base. Only raw
// VM data is cloned; original ownership, delivery records and locks stay put.
func seedVZLiveBase(t *testing.T, source, destination string) {
	t.Helper()
	if runtime.GOOS != "darwin" || !filepath.IsAbs(source) || !filepath.IsAbs(destination) {
		t.Fatal("VZ base fixture requires macOS and absolute paths")
	}
	disk, err := os.Open(filepath.Join(source, "disk.img"))
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	if err := unix.Flock(int(disk.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		t.Fatal("fixture source disk busy:", err)
	}
	if _, err := os.Lstat(filepath.Join(source, "state.vzvmsave")); !os.IsNotExist(err) {
		t.Fatal("fixture source must be cold")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config.json", "disk.img", "nvram.bin"} {
		from, to := filepath.Join(source, name), filepath.Join(destination, name)
		info, err := os.Lstat(from)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatal("fixture requires regular VM files:", name, err)
		}
		if data, err := exec.Command("/bin/cp", "-c", from, to).CombinedOutput(); err != nil {
			t.Fatalf("clone fixture %s: %v: %s", name, err, data)
		}
		if err := os.Chmod(to, 0600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(destination, "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["arch"] != "arm64" || cfg["os"] != "darwin" {
		t.Fatal("fixture requires arm64 macOS")
	}
	cfg["vzVersion"] = 1
	delete(cfg, "restoreRequired")
	delete(cfg, "snapshot")
	data, err = json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
