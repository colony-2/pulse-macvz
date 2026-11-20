//go:build darwin && arm64

package vz

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse-tart/internal/backend"
	"github.com/colony-2/pulse-tart/internal/config"
	"golang.org/x/sys/unix"
)

func TestLiveMacOSRestoreLifecycle(t *testing.T) {
	base, runner, root := os.Getenv("PULSE_VZ_MACOS_BASE"), os.Getenv("PULSE_VZ_RUNNER"), os.Getenv("PULSE_VZ_RESULTS")
	if base == "" || runner == "" || root == "" {
		t.Skip("set PULSE_VZ_MACOS_BASE, PULSE_VZ_RUNNER and PULSE_VZ_RESULTS")
	}
	home, err := os.MkdirTemp(root, "vz-mac-provider-")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("artifacts:", home)
	dir := filepath.Join(home, "vms", "source")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	disk, err := os.Open(filepath.Join(base, "disk.img"))
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	if err = unix.Flock(int(disk.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		t.Fatal("base disk busy:", err)
	}
	if _, err = os.Lstat(filepath.Join(base, "state.vzvmsave")); !os.IsNotExist(err) {
		t.Fatal("base must be cold")
	}
	for _, file := range []string{"config.json", "disk.img", "nvram.bin"} {
		if err = cloneFile(filepath.Join(base, file), filepath.Join(dir, file)); err != nil {
			t.Fatal(err)
		}
	}
	c, err := readConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	c.Version = layoutVersion
	if c.Arch != "arm64" || c.OS != "darwin" {
		t.Fatal("requires arm64 macOS base")
	}
	if err = atomicJSON(filepath.Join(dir, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	common := config.Config{OwnerID: "live-vz-mac", BootstrapTimeout: 3 * time.Minute}
	options := config.VZ{Home: home, Runner: runner, CommandTimeout: 3 * time.Minute, DeliveryTimeout: 2 * time.Minute, SSHTimeout: 5 * time.Second}
	b, err := New(options, common)
	if err != nil {
		t.Fatal(err)
	}
	owned := map[string]string{}
	defer func() {
		if b == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for name, uid := range owned {
			_ = b.Stop(ctx, name, uid)
		}
		// Do not finish until every owned runner exits, including on failure.
		for name := range owned {
			for {
				v, e := b.Get(ctx, name)
				if e != nil || v.Stopped() {
					break
				}
				select {
				case <-ctx.Done():
					break
				case <-time.After(100 * time.Millisecond):
				}
				if ctx.Err() != nil {
					break
				}
			}
		}
		_ = b.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	if err = b.Info(ctx); err != nil {
		t.Fatal(err)
	}
	source := backend.Spec{Name: "source", OS: "darwin", Arch: "arm64", CPU: int64(c.CPU), Memory: int64(c.Memory >> 20), Username: "admin", Password: "admin", Suspendable: true}
	if _, err = b.AdoptSource(ctx, source, nil); err != nil {
		t.Fatal(err)
	}
	ready := func(name string) {
		t.Helper()
		deadline := time.NewTimer(2 * time.Minute)
		defer deadline.Stop()
		for {
			events, e := b.Events(ctx, name)
			if e != nil {
				t.Fatal(e)
			}
			for _, event := range events {
				if strings.Contains(event.Payload, "PULSE_HANDOFF_OK") {
					return
				}
			}
			if data, e := os.ReadFile(filepath.Join(home, "vms", name, "pulse.exit")); e == nil {
				log, _ := os.ReadFile(filepath.Join(home, "vms", name, "pulse.run.log"))
				t.Fatalf("%s exited %s: %s", name, data, log)
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-deadline.C:
				t.Fatalf("%s guest readiness timed out", name)
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	waitState := func(name, state string) {
		t.Helper()
		for {
			v, e := b.Get(ctx, name)
			if e != nil {
				t.Fatal(e)
			}
			if v.PowerState == state {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	for i := 1; i <= 2; i++ {
		spec := source
		spec.Name = fmt.Sprintf("template-%d", i)
		spec.Image = "source"
		spec.StartupScript.Content = "echo PULSE_HANDOFF_OK\n"
		v, e := b.Create(ctx, spec)
		if e != nil {
			t.Fatal(e)
		}
		owned[v.Name] = v.UID
		ready(v.Name)
		if e = b.Suspend(ctx, v.Name, v.UID); e != nil {
			t.Fatal(e)
		}
		waitState(v.Name, "suspended")
		if e = b.RetainSource(ctx, v.Name, v.UID); e != nil {
			t.Fatal(e)
		}
	}
	for round := 1; round <= 2; round++ {
		var jobs []backend.VM
		for slot := 1; slot <= 2; slot++ {
			spec := source
			spec.Name = fmt.Sprintf("round-%d-slot-%d", round, slot)
			spec.Image = fmt.Sprintf("template-%d", slot)
			spec.StartupScript.Content = "echo PULSE_HANDOFF_OK\n"
			v, e := b.Create(ctx, spec)
			if e != nil {
				t.Fatal(e)
			}
			owned[v.Name] = v.UID
			jobs = append(jobs, v)
		}
		for _, v := range jobs {
			ready(v.Name)
		}
		if err = b.Close(); err != nil {
			t.Fatal(err)
		}
		b, err = New(options, common)
		if err != nil {
			t.Fatal(err)
		}
		if err = b.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		for _, v := range jobs {
			got, e := b.Get(ctx, v.Name)
			if e != nil || got.UID != v.UID || got.PowerState != "running" {
				t.Fatal(got, e)
			}
			output, e := b.Exec(ctx, v.Name, v.UID, []byte("uname -s; uname -m"))
			if e != nil || !strings.Contains(string(output), "Darwin\narm64") {
				t.Fatal(string(output), e)
			}
			if e = b.Stop(ctx, v.Name, v.UID); e != nil {
				t.Fatal(e)
			}
			waitState(v.Name, "stopped")
			if e = b.Delete(ctx, v.Name); e != nil {
				t.Fatal(e)
			}
			delete(owned, v.Name)
		}
	}
	// A corrupt restore must exit unsuccessfully; it must never boot cold.
	store := &Store{Home: home, Runner: runner, Timeout: time.Minute, PullTimeout: time.Minute}
	if err = store.Clone(ctx, "template-1", "broken"); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(home, "vms", "broken")
	if err = os.WriteFile(filepath.Join(broken, ".pulse-owner"), []byte(strings.Repeat("f", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(broken, "state.vzvmsave"), []byte("corrupt snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Start("broken"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = store.Stop(cleanup, "broken")
	}()
	for {
		data, e := os.ReadFile(filepath.Join(broken, "pulse.exit"))
		if e == nil {
			if strings.TrimSpace(string(data)) == "0" {
				t.Fatal("corrupt restore succeeded")
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	log, _ := os.ReadFile(filepath.Join(broken, "pulse.run.log"))
	if strings.Contains(string(log), "native start requested") {
		t.Fatal("failed restore fell back to cold start")
	}
	if !strings.Contains(string(log), "restore") {
		t.Fatalf("failure did not exercise restore: %s", log)
	}
}
