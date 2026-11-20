//go:build darwin && arm64

package vz

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse-tart/internal/backend"
	"github.com/colony-2/pulse-tart/internal/config"
	"golang.org/x/sys/unix"
)

// Explicit opt-in. Originals are read-only; artifacts remain for diagnosis.
func TestLiveLinuxLifecycle(t *testing.T) {
	base, runner, root := os.Getenv("PULSE_VZ_LINUX_BASE"), os.Getenv("PULSE_VZ_RUNNER"), os.Getenv("PULSE_VZ_RESULTS")
	if base == "" || runner == "" || root == "" {
		t.Skip("set PULSE_VZ_LINUX_BASE, PULSE_VZ_RUNNER and PULSE_VZ_RESULTS")
	}
	home, err := os.MkdirTemp(root, "vz-provider-")
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
	if c.Arch != "arm64" || c.OS != "linux" {
		t.Fatal("requires arm64 Linux base")
	}
	if err = atomicJSON(filepath.Join(dir, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	common := config.Config{OwnerID: "live-vz", BootstrapTimeout: time.Minute}
	options := config.VZ{Home: home, Runner: runner, CommandTimeout: 15 * time.Second, DeliveryTimeout: 45 * time.Second, SSHTimeout: 5 * time.Second}
	b, err := New(options, common)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if b != nil {
			_ = b.Close()
		}
	}()
	limit := 2 * time.Minute
	if os.Getenv("PULSE_TEST_VZ_KVM") == "1" {
		limit = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	if err = b.Info(ctx); err != nil {
		t.Fatal(err)
	}
	source := backend.Spec{Name: "source", OS: "linux", Arch: "arm64", CPU: int64(c.CPU), Memory: int64(c.Memory >> 20), Username: "admin", Password: "admin"}
	if _, err = b.AdoptSource(ctx, source, nil); err != nil {
		t.Fatal(err)
	}
	job := source
	job.Name = "job"
	job.Image = "source"
	job.StartupScript.Content = "echo PULSE_HANDOFF_OK\n"
	v, err := b.Create(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if b == nil {
			return
		}
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		if _, err := b.Get(cleanup, "job"); err != nil {
			return // The successful test has already deleted the job.
		}
		if err := b.Stop(cleanup, "job", v.UID); err != nil {
			t.Errorf("cleanup stop: %v", err)
			return
		}
		for {
			got, err := b.Get(cleanup, "job")
			if err != nil || got.Stopped() {
				return
			}
			select {
			case <-cleanup.Done():
				t.Error("cleanup: runner did not stop")
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
	for {
		events, e := b.Events(ctx, "job")
		if e != nil {
			t.Fatal(e)
		}
		ready := false
		for _, event := range events {
			if strings.Contains(event.Payload, "PULSE_HANDOFF_OK") {
				ready = true
			}
		}
		if ready {
			break
		}
		if data, e := os.ReadFile(filepath.Join(home, "vms", "job", "pulse.exit")); e == nil {
			log, _ := os.ReadFile(filepath.Join(home, "vms", "job", "pulse.run.log"))
			t.Fatalf("runner exited %s: %s", data, log)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
	before, err := os.ReadFile(filepath.Join(home, "vms", "job", "pulse.json"))
	if err != nil {
		t.Fatal(err)
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
	got, err := b.Get(ctx, "job")
	if err != nil || got.UID != v.UID || got.PowerState != "running" {
		t.Fatal(got, err)
	}
	after, err := os.ReadFile(filepath.Join(home, "vms", "job", "pulse.json"))
	if err != nil {
		t.Fatal(err)
	}
	var first, last struct {
		Native struct {
			UID      string
			Delivery struct {
				ExecutionPossible bool `json:"execution_possible"`
			}
		}
	}
	if err := json.Unmarshal(before, &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &last); err != nil {
		t.Fatal(err)
	}
	if first.Native.UID != last.Native.UID || !last.Native.Delivery.ExecutionPossible {
		t.Fatal("recovery lost dispatch fence")
	}
	output, err := b.Exec(ctx, "job", v.UID, []byte("uname -s; uname -m"))
	if err != nil || !strings.Contains(string(output), "Linux\naarch64") {
		t.Fatal(string(output), err)
	}
	if os.Getenv("PULSE_TEST_VZ_KVM") == "1" {
		verifyLiveKVM(t, ctx, b, v)
	}
	output, err = b.Exec(ctx, "job", v.UID, []byte(`set -eu
mkdir -p /var/tmp/pulse-storage-check
cd /var/tmp/pulse-storage-check
dd if=/dev/urandom of=data bs=1M count=128 status=none
sha256sum data > expected
sync
sha256sum -c expected
`))
	if err != nil || !strings.Contains(string(output), "data: OK") {
		t.Fatal("guest disk write verification", string(output), err)
	}
	if err = b.Stop(ctx, "job", v.UID); err != nil {
		t.Fatal(err)
	}
	for {
		got, err = b.Get(ctx, "job")
		if err != nil {
			t.Fatal(err)
		}
		if got.Stopped() {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err = b.RetainSource(ctx, "job", v.UID); err != nil {
		t.Fatal(err)
	}
	verify := job
	verify.Name, verify.Image = "verify", "job"
	verify.StartupScript.Content = "set -eu\ncd /var/tmp/pulse-storage-check\nsha256sum -c expected\necho PULSE_HANDOFF_OK\n"
	check, err := b.Create(ctx, verify)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		if err := b.Stop(cleanup, check.Name, check.UID); err != nil {
			t.Errorf("storage verification cleanup: %v", err)
		}
	}()
	for {
		events, err := b.Events(ctx, check.Name)
		if err != nil {
			t.Fatal(err)
		}
		verified := false
		for _, event := range events {
			verified = verified || strings.Contains(event.Payload, "PULSE_HANDOFF_OK")
		}
		if verified {
			t.Log("guest disk checksum survived stop, clone, and cold boot")
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("disk verification after cold boot:", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
	if err = b.Delete(ctx, "job"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(home, "vms", "job")); !os.IsNotExist(err) {
		t.Fatal("job remains after deletion")
	}
}
