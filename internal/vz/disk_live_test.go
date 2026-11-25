//go:build darwin && arm64

package vz

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse-macvz/internal/backend"
	"github.com/colony-2/pulse-macvz/internal/config"
	"golang.org/x/sys/unix"
)

// Uses one macOS position at a time. Requires a cold prepared base with admin
// credentials, and never modifies the supplied base or a production allocation.
func TestLiveMacOSDiskGrowth(t *testing.T) {
	base, runner, root := os.Getenv("PULSE_VZ_MACOS_DISK_BASE"), os.Getenv("PULSE_VZ_RUNNER"), os.Getenv("PULSE_VZ_RESULTS")
	if base == "" || runner == "" || root == "" {
		t.Skip("set PULSE_VZ_MACOS_DISK_BASE, PULSE_VZ_RUNNER, PULSE_VZ_RESULTS")
	}
	home, err := os.MkdirTemp(root, "macos-disk-")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("artifacts:", home)
	dir := filepath.Join(home, "vms", "source")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// Hold all source locks while cloning, then immediately release them.
	func() {
		for _, name := range []string{"disk.img", "vz.lock", "pulse.run.lock"} {
			f, err := os.Open(filepath.Join(base, name))
			if os.IsNotExist(err) && name != "disk.img" {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
				t.Fatal("source active:", err)
			}
		}
		if _, err := os.Lstat(filepath.Join(base, "state.vzvmsave")); !os.IsNotExist(err) {
			t.Fatal("source must be cold")
		}
		for _, name := range []string{"config.json", "disk.img", "nvram.bin"} {
			if err := cloneFile(filepath.Join(base, name), filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
		}
	}()
	c, err := readConfig(dir)
	if err != nil || c.OS != "darwin" {
		t.Fatal("requires macOS base", err)
	}
	c.Version, c.Arch = layoutVersion, "arm64"
	if err := atomicJSON(filepath.Join(dir, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	b, err := New(config.VZ{Home: home, Runner: runner, SSHTimeout: 5 * time.Second, DeliveryTimeout: 2 * time.Minute}, config.Config{OwnerID: "disk-growth-test", BootstrapTimeout: 3 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	owned := map[string]string{}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for name, uid := range owned {
			if err := b.Stop(ctx, name, uid); err != nil {
				t.Error("cleanup:", err)
			}
		}
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	source := backend.Spec{Name: "source", OS: "darwin", Arch: "arm64", CPU: int64(c.CPU), Memory: int64(c.Memory >> 20), Username: "admin", Password: "admin", Suspendable: true}
	if _, err := b.AdoptSource(ctx, source, nil); err != nil {
		t.Fatal(err)
	}
	spec := source
	spec.Name = "grow"
	spec.Image = "source"
	spec.CPU = 4
	spec.Memory = 8192
	spec.StartupScript.Content = "echo PULSE_HANDOFF_OK\n"
	vm, err := b.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	owned[vm.Name] = vm.UID
	exec := func(v backend.VM, script string) []byte {
		t.Helper()
		// Only the harmless connectivity probe is retried, never the disk mutation.
		for {
			if _, err := b.Exec(ctx, v.Name, v.UID, []byte("true\n")); err == nil {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(time.Second):
			}
		}
		out, err := b.Exec(ctx, v.Name, v.UID, []byte(script))
		if err != nil {
			t.Fatalf("guest: %v\n%s", err, out)
		}
		return out
	}
	raw, err := os.ReadFile("../../scripts/prepare-base.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	start, end := strings.Index(script, "macos_free_space()"), strings.Index(script, "state=/var/db/pulse-tart-base")
	if start < 0 || end < start {
		t.Fatal("missing storage helpers")
	}
	grow := "sudo -n /bin/bash <<'PULSE_DISK_TEST'\nset -euo pipefail\n" + script[start:end] + "\ngrow_macos_storage\nmacos_free_space\ndf -h /System/Volumes/Data /nix/store\nPULSE_DISK_TEST\n"
	for i := 0; i < 2; i++ {
		t.Logf("growth/check %d:\n%s", i+1, exec(vm, grow))
	}
	info, err := os.Stat(filepath.Join(home, "vms", "grow", "disk.img"))
	if err != nil || info.Size() < config.MacOSDiskBytes {
		t.Fatal("disk capacity", info, err)
	}
	if err := b.Suspend(ctx, vm.Name, vm.UID); err != nil {
		t.Fatal(err)
	}
	spec.Name = "restored"
	spec.Image = vm.Name
	restored, err := b.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	owned[restored.Name] = restored.UID
	// Verify free space survives a normal suspended-template clone and restore.
	check := "sudo -n /bin/bash <<'PULSE_DISK_TEST'\nset -euo pipefail\n" + script[start:end] + "\nmacos_free_space\ndf -h /System/Volumes/Data /nix/store\nPULSE_DISK_TEST\n"
	t.Logf("restored worker:\n%s", exec(restored, check))
}
