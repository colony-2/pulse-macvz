//go:build darwin && arm64

package vz

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse-macvz/internal/backend"
	"github.com/colony-2/pulse-macvz/internal/config"
	"golang.org/x/sys/unix"
)

// Times a single macOS VM, without Docker, from clone/dispatch through execution
// of a guest command over management SSH. This is not a c2j submission benchmark.
func TestLiveMacOSTiming(t *testing.T) {
	base, runner, root := os.Getenv("PULSE_VZ_MACOS_BASE"), os.Getenv("PULSE_VZ_RUNNER"), os.Getenv("PULSE_VZ_RESULTS")
	if os.Getenv("PULSE_TEST_VZ_TIMING") != "1" || base == "" || runner == "" || root == "" {
		t.Skip("set PULSE_TEST_VZ_TIMING=1 and VZ base/runner/results paths")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	home, err := os.MkdirTemp(root, "vz-mac-timing-")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("artifacts:", home)
	source := filepath.Join(home, "vms", "source")
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	disk, err := os.Open(filepath.Join(base, "disk.img"))
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	if err := unix.Flock(int(disk.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(base, "state.vzvmsave")); !os.IsNotExist(err) {
		t.Fatal("input must be cold")
	}
	for _, name := range []string{"config.json", "disk.img", "nvram.bin"} {
		if err := cloneFile(filepath.Join(base, name), filepath.Join(source, name)); err != nil {
			t.Fatal(err)
		}
	}
	c, err := readConfig(source)
	if err != nil {
		t.Fatal(err)
	}
	c.Version = layoutVersion
	if c.OS != "darwin" || c.Arch != "arm64" {
		t.Fatal("requires arm64 macOS source")
	}
	if err := atomicJSON(filepath.Join(source, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	b, err := New(config.VZ{Home: home, Runner: runner, SSHTimeout: 5 * time.Second}, config.Config{OwnerID: "mac-timing", BootstrapTimeout: 3 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	owned := map[string]string{}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		for name, uid := range owned {
			_ = b.Stop(cleanup, name, uid)
		}
		for name := range owned {
			for cleanup.Err() == nil {
				v, e := b.Get(cleanup, name)
				if e != nil || v.Stopped() {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()
	spec := backend.Spec{Name: "source", OS: "darwin", Arch: "arm64", CPU: int64(c.CPU), Memory: int64(c.Memory >> 20), Username: "admin", Password: "admin", Suspendable: true}
	if _, err := b.AdoptSource(ctx, spec, nil); err != nil {
		t.Fatal(err)
	}
	measure := func(name, image string) (backend.VM, float64) {
		t.Helper()
		s := spec
		s.Name = name
		s.Image = image
		s.StartupScript.Content = "echo PULSE_TIMING_READY\n"
		start := time.Now()
		v, e := b.Create(ctx, s)
		if e != nil {
			t.Fatal(e)
		}
		owned[name] = v.UID
		for ctx.Err() == nil {
			events, e := b.Events(ctx, name)
			if e != nil {
				t.Fatal(e)
			}
			for _, event := range events {
				if strings.Contains(event.Payload, "PULSE_TIMING_READY") {
					elapsed := float64(time.Since(start).Microseconds()) / 1000
					t.Logf("%s: %.1f ms clone to guest command", name, elapsed)
					return v, elapsed
				}
			}
			got, e := b.Get(ctx, name)
			if e != nil || got.Status == "failed" {
				t.Fatalf("VM startup failed: %v", e)
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal(ctx.Err())
		return backend.VM{}, 0
	}
	metrics := map[string]any{"cpu": c.CPU, "memory_bytes": c.Memory, "scope": "single macOS clone through management SSH command; no Docker or c2j"}
	template, cold := measure("template", "source")
	metrics["cold_start_ms"] = cold
	start := time.Now()
	if err := b.Suspend(ctx, template.Name, template.UID); err != nil {
		t.Fatal(err)
	}
	for ctx.Err() == nil {
		v, e := b.Get(ctx, template.Name)
		if e != nil {
			t.Fatal(e)
		}
		if v.PowerState == "suspended" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if ctx.Err() != nil {
		t.Fatal(ctx.Err())
	}
	metrics["suspend_ms"] = float64(time.Since(start).Microseconds()) / 1000
	if err := b.RetainSource(ctx, template.Name, template.UID); err != nil {
		t.Fatal(err)
	}
	var restores []float64
	for i := 1; i <= 3; i++ {
		v, elapsed := measure(fmt.Sprintf("restore-%d", i), template.Name)
		restores = append(restores, elapsed)
		if err := b.Stop(ctx, v.Name, v.UID); err != nil {
			t.Fatal(err)
		}
		for ctx.Err() == nil {
			got, e := b.Get(ctx, v.Name)
			if e != nil {
				t.Fatal(e)
			}
			if got.Stopped() {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err := b.Delete(ctx, v.Name); err != nil {
			t.Fatal(err)
		}
		delete(owned, v.Name)
	}
	metrics["restore_ms"] = restores
	raw, _ := json.MarshalIndent(metrics, "", "  ")
	if err := os.WriteFile(filepath.Join(home, "timings.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("MACOS_TIMING_PASS:", string(raw))
}
