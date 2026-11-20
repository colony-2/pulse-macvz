package vz

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse-tart/internal/local"
)

func fixture(t *testing.T) (*Store, string) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "vms", "source")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	c := diskConfig{OS: "linux", Arch: "arm64", CPU: 4, Memory: 8 << 30, MAC: "02:00:00:00:00:01", Version: layoutVersion}
	if err := atomicJSON(filepath.Join(dir, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"disk.img", "nvram.bin", "pulse.json", ".pulse-owner", "pulse.exit", "vz.started", "vz-control.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("private fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return &Store{Home: home, Timeout: time.Second, PullTimeout: time.Minute}, dir
}
func TestColdCloneIsolationAndIdentity(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("APFS clone")
	}
	s, source := fixture(t)
	ctx := context.Background()
	if err := s.Clone(ctx, "source", "job"); err != nil {
		t.Fatal(err)
	}
	dir, _ := s.path("job")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("cloned non-VM files: %v", entries)
	}
	before, err := os.ReadFile(filepath.Join(source, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Configure(ctx, "job", local.MachineOptions{CPU: 4, Memory: 8192, RandomMAC: true}); err != nil {
		t.Fatal(err)
	}
	c, err := readConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.MAC == "02:00:00:00:00:01" {
		t.Fatal("cold clone retained MAC")
	}
	after, _ := os.ReadFile(filepath.Join(source, "config.json"))
	if string(before) != string(after) {
		t.Fatal("source changed")
	}
	if err = s.Clone(ctx, "source", "job"); err == nil {
		t.Fatal("replaced existing clone")
	}
	if err = os.WriteFile(filepath.Join(dir, "disk.img"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	disk, _ := os.ReadFile(filepath.Join(source, "disk.img"))
	if string(disk) != "private fixture" {
		t.Fatal("source disk changed")
	}
}
func TestCloneRejectsRunningSourceAndRollsBack(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("APFS clone")
	}
	for _, kind := range []string{"vz.lock", "pulse.run.lock", "symlink", "missing"} {
		t.Run(kind, func(t *testing.T) {
			s, dir := fixture(t)
			switch kind {
			case "symlink":
				os.Remove(filepath.Join(dir, "nvram.bin"))
				if err := os.Symlink("disk.img", filepath.Join(dir, "nvram.bin")); err != nil {
					t.Fatal(err)
				}
			case "missing":
				os.Remove(filepath.Join(dir, "nvram.bin"))
			default:
				lock, err := lockFile(context.Background(), filepath.Join(dir, kind), false, false)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			}
			if err := s.Clone(context.Background(), "source", "job"); err == nil {
				t.Fatal("clone accepted unsafe source")
			}
			if _, err := os.Stat(filepath.Join(s.Home, "vms", "job")); !os.IsNotExist(err) {
				t.Fatal("partial clone published")
			}
			entries, _ := os.ReadDir(filepath.Join(s.Home, "vms"))
			if len(entries) != 1 {
				t.Fatal("temporary clone leaked")
			}
		})
	}
}
func TestSnapshotRequiresStateAndCompatibleIdentity(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("host build and APFS")
	}
	s, dir := fixture(t)
	c, _ := readConfig(dir)
	host, err := hostBuild()
	if err != nil {
		t.Fatal(err)
	}
	c.OS = "darwin"
	c.Hardware = []byte("hardware")
	c.Identity = []byte("identity")
	c.RestoreRequired = true
	c.Snapshot = &snapshot{Host: host, Layout: deviceLayout, ConfigHash: configHash(c)}
	if err = atomicJSON(filepath.Join(dir, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	if err = s.Clone(context.Background(), "source", "job"); err == nil {
		t.Fatal("missing state became cold clone")
	}
	if err = os.WriteFile(filepath.Join(dir, "state.vzvmsave"), []byte("state"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.Clone(context.Background(), "source", "job"); err != nil {
		t.Fatal(err)
	}
	cloned, _ := readConfig(filepath.Join(s.Home, "vms", "job"))
	if configHash(c) != configHash(cloned) {
		t.Fatal("snapshot identity changed")
	}
	if err = s.Configure(context.Background(), "job", local.MachineOptions{CPU: 4, Memory: 8192, RandomMAC: true}); err == nil {
		t.Fatal("snapshot reconfigured")
	}
	c.MAC = "02:00:00:00:00:02"
	if err = atomicJSON(filepath.Join(dir, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	if err = s.Clone(context.Background(), "source", "bad"); err == nil {
		t.Fatal("changed identity accepted")
	}
	c.MAC = "02:00:00:00:00:01"
	c.Snapshot.Host = "other"
	atomicJSON(filepath.Join(dir, "config.json"), c)
	if err = s.Clone(context.Background(), "source", "bad"); err == nil {
		t.Fatal("different host build accepted")
	}
}
func TestInventoryAndMutationLocks(t *testing.T) {
	s, dir := fixture(t)
	ctx := context.Background()
	lock, err := lockFile(ctx, filepath.Join(dir, "pulse.run.lock"), false, false)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.Inspect(ctx, "source")
	if err != nil || h.State != "running" {
		t.Fatal(h, err)
	}
	if err = s.Delete(ctx, "source"); err == nil {
		t.Fatal("deleted active runner")
	}
	if err = s.Configure(ctx, "source", local.MachineOptions{CPU: 4, Memory: 8192}); err == nil {
		t.Fatal("configured active runner")
	}
	lock.Close()
	h, err = s.Inspect(ctx, "source")
	if err != nil || h.State != "stopped" {
		t.Fatal(h, err)
	}
	if _, err = s.path("../source"); err == nil {
		t.Fatal("accepted path traversal")
	}
	if err = os.Symlink(dir, filepath.Join(s.Home, "vms", "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Inspect(ctx, "alias"); err == nil {
		t.Fatal("accepted symlink VM")
	}
}
func TestDHCPNormalization(t *testing.T) {
	ips := leaseIPs("{\nip_address=192.168.64.2\nhw_address=1,2:0:0:0:0:1\n}\n{\nip_address=bad\nhw_address=1,2:0:0:0:0:1\n}", "02:00:00:00:00:01")
	if len(ips) != 1 || ips[0] != "192.168.64.2" {
		t.Fatal(ips)
	}
}
func TestRunRefusesUnownedDirectory(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("native runner")
	}
	_, dir := fixture(t)
	if err := runVM(dir); err == nil || !strings.Contains(err.Error(), "Pulse-owned") {
		t.Fatal(err)
	}
}
func TestConfigHashIgnoresStateButIncludesHardware(t *testing.T) {
	c := diskConfig{CPU: 4, Memory: 8 << 30, Identity: []byte("id"), MAC: "02:00:00:00:00:01"}
	original := configHash(c)
	c.RestoreRequired = true
	c.Snapshot = &snapshot{Host: "host"}
	if configHash(c) != original {
		t.Fatal("metadata changes identity")
	}
	c.Identity = []byte("new")
	if configHash(c) == original {
		t.Fatal("identity omitted from stamp")
	}
	data, _ := json.Marshal(c)
	if !json.Valid(data) {
		t.Fatal("invalid config")
	}
}
