package servercli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse-macvz/internal/config"
	"github.com/colony-2/pulse-macvz/internal/local"
	"golang.org/x/sys/unix"
)

func TestStructuredImagesShowsVersionForEveryPowerState(t *testing.T) {
	var out bytes.Buffer
	r := poolStatus{Owner: "server", Slots: []*slotStatus{{ID: "linux-0001", Base: "ubuntu", State: "idle", Allocations: []statusAllocation{{Primary: &statusVM{Name: "worker", Power: "running", C2J: &local.ImageVersion{Version: "v1.2.3", Revision: "123abcdef0123456789"}}}}}, {ID: "mac-0001", State: "ready", Template: &statusVM{Name: "template", Power: "suspended", C2J: &local.ImageVersion{Version: "v2", Revision: "abcdef0123456789"}}}}, Images: []imageStatus{{Kind: "base", VM: &statusVM{Name: "base", Power: "stopped", C2J: &local.ImageVersion{Version: "v3", Revision: "deadbeef123456789"}}}, {Kind: "slot", VM: &statusVM{Name: "older-template", Power: "suspended"}}}}
	if err := printPoolImages(&out, r); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Slot VMs", "Base images", "Other cached images and templates", "v1.2.3 (commit 123abcd)", "v2 (commit abcdef0)", "v3 (commit deadbee)", "unknown (commit unknown)"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in %s", want, out.String())
		}
	}
	if strings.Count(out.String(), "VM:      worker") != 1 || strings.Contains(out.String(), "phase=-") {
		t.Fatal(out.String())
	}
}

func TestStatusTableUsesBackingWorkerVersionAndOmitsInventory(t *testing.T) {
	vm := func(version, revision string) *statusVM {
		return &statusVM{Name: "detail-only-vm-name", C2J: &local.ImageVersion{Version: version, Revision: revision}}
	}
	r := poolStatus{Slots: []*slotStatus{
		{ID: "assigned", Jobs: []statusJob{{InstanceID: "hidden-instance", JobID: "job-1", VM: vm("v1", "123abcdef")}}, Template: vm("wrong-template-version", "")},
		{ID: "idle", Allocations: []statusAllocation{{Primary: vm("v2", "abcdef012"), Companion: vm("wrong-companion-version", "")}}},
		{ID: "ready", Template: vm("v3", "fedcba987")},
		{ID: "missing-worker-version", Allocations: []statusAllocation{{Primary: &statusVM{}}}, Template: vm("wrong-fallback-version", "")},
	}}
	var out bytes.Buffer
	if err := printPoolStatus(&out, r); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"C2J VERSION", "v1 (123abcd)", "v2 (abcdef0)", "v3 (fedcba9)", "unknown", "job-1", "pulse-macvz images"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, out.String())
		}
	}
	for _, unwanted := range []string{"JOBDB INSTANCE", "hidden-instance", "Slot VMs", "Base images", "detail-only-vm-name", "wrong-"} {
		if strings.Contains(out.String(), unwanted) {
			t.Fatalf("unexpected %q: %s", unwanted, out.String())
		}
	}
}

func TestStatusReadsVersionWithoutStartingVM(t *testing.T) {
	o := statusOptions(t)
	scope := (config.Settings{Config: o.config, Kind: "vz", Endpoint: o.vmHome}).Scope()
	dir := writeStatusFixture(t, o.vmHome, scope, "base", map[string]any{"image": map[string]any{"name": "base", "uid": "base-uid", "kind": "base", "phase": "ready"}}, false)
	data := []byte(`{"version":"v1.2.3","revision":"123abcdef0123456789012345678901234567890"}`)
	if err := os.WriteFile(filepath.Join(dir, local.ImageVersionFile), data, 0600); err != nil {
		t.Fatal(err)
	}
	r, err := readPoolStatus(t.Context(), o)
	if err != nil || len(r.Images) != 1 {
		t.Fatal(r, err)
	}
	vm := r.Images[0].VM
	if vm.Power != "stopped" || vm.C2J == nil || vm.C2J.Version != "v1.2.3" {
		t.Fatal(vm)
	}
}

func writeStatusFixture(t *testing.T, home, scope, name string, props map[string]any, running bool) string {
	t.Helper()
	dir := filepath.Join(home, "vms", name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	props["version"] = 1
	props["scope"] = scope
	props["key"] = "SECRET-KEY"
	data := map[string]any{"version": 1, "scope": scope, "native": map[string]any{"uid": name + "-uid", "created": "2026-11-20T20:00:00Z", "spec": map[string]any{"name": name, "os": "linux", "password": "SECRET-PASSWORD", "startup_script": map[string]any{"script_content": "SECRET-LEASE"}}}, "properties": props}
	for filename, value := range map[string]any{"pulse.json": data, "config.json": map[string]any{"os": "linux", "arch": "arm64", "cpuCount": 4, "memorySize": 8 << 30, "macAddress": "02:00:00:00:00:01", "vzVersion": 1}} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, filename), encoded, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if running {
		f, err := os.OpenFile(filepath.Join(dir, "pulse.run.lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
	}
	return dir
}
func statusOptions(t *testing.T) serverOptions {
	t.Helper()
	o := serverOptions{owner: "server", backend: "vz", mode: "outbound", vmHome: t.TempDir(), capacity: capacityOptions{macOS: 2, linux: 2, macDocker: -1, macBase: "all"}}
	o.endpoint.Address = "192.168.64.1"
	o.endpoint.PortMin, o.endpoint.PortMax = 45000, 45099
	if err := o.resolvePool(); err != nil {
		t.Fatal(err)
	}
	return o
}
func TestStatusReadsOwnedSlotsAndAssignmentsWithoutSecrets(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	o := statusOptions(t)
	scope := (config.Settings{Config: o.config, Kind: "vz", Endpoint: o.vmHome}).Scope()
	job := map[string]any{"launch_id": "launch-1", "slot_id": "standard-0001", "pair_id": "pair-1", "name": "primary", "uid": "primary-uid", "created": "2026-11-20T20:01:00Z", "phase": "active", "outcome": "accepted", "job": map[string]any{"instance_id": "db-1", "tenant_id": "tenant-a", "job_id": "job-1", "jobdb": "https://private.example/tenant"}}
	pair := map[string]any{"ID": "pair-1", "SlotID": "standard-0001", "Base": "macos-26-arm64", "Phase": "running", "LaunchID": "launch-1", "Primary": map[string]any{"Name": "primary", "Password": "SECRET-PAIR"}, "PrimaryUID": "primary-uid", "Engine": map[string]any{"Name": "companion"}, "EngineUID": "companion-uid"}
	writeStatusFixture(t, o.vmHome, scope, "primary", map[string]any{"revision": 2, "pair": pair, "job": job}, true)
	stale := map[string]any{"ID": "pair-1", "SlotID": "standard-0001", "Phase": "ready", "Primary": map[string]any{"Name": "primary"}, "PrimaryUID": "primary-uid", "Engine": map[string]any{"Name": "companion"}, "EngineUID": "companion-uid"}
	writeStatusFixture(t, o.vmHome, scope, "companion", map[string]any{"revision": 1, "pair": stale}, true)
	idle := map[string]any{"ID": "idle-1", "SlotID": "linux-0001", "Base": "ubuntu-24.04-arm64", "Phase": "ready", "Primary": map[string]any{"Name": "idle"}, "PrimaryUID": "idle-uid"}
	writeStatusFixture(t, o.vmHome, scope, "idle", map[string]any{"pair": idle}, true)
	writeStatusFixture(t, o.vmHome, "foreign", "foreign", map[string]any{"job": job}, true)
	report, err := readPoolStatus(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	slots := map[string]*slotStatus{}
	for _, s := range report.Slots {
		slots[s.ID] = s
	}
	busy := slots["standard-0001"]
	if busy.State != "assigned" || len(busy.Jobs) != 1 || busy.Jobs[0].TenantID != "tenant-a" || busy.Jobs[0].InstanceID != "db-1" || busy.Jobs[0].JobID != "job-1" || len(busy.Allocations) != 1 {
		t.Fatalf("incorrect assignment: %+v", busy)
	}
	if busy.LastRecordedSubmission == nil || !busy.LastRecordedSubmission.Equal(time.Date(2026, 11, 20, 20, 1, 0, 0, time.UTC)) {
		t.Fatal("incorrect submission timestamp")
	}
	if slots["linux-0001"].State != "idle" || slots["linux-0002"].State != "not prepared" || slots["linux-0001"].LastRecordedSubmission != nil {
		t.Fatal("incorrect idle/missing state")
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = printPoolStatus(&out, report); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SECRET", "private.example", "foreign"} {
		if strings.Contains(string(data), secret) || strings.Contains(out.String(), secret) {
			t.Fatalf("status leaked %s", secret)
		}
	}
	for _, want := range []string{"tenant-a", "job-1", "launch-1"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal("missing text identity", want)
		}
	}
}

func TestStatusPreservesUncertainHandoffWithJobAttached(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	o := statusOptions(t)
	scope := (config.Settings{Config: o.config, Kind: "vz", Endpoint: o.vmHome}).Scope()
	job := map[string]any{"launch_id": "launch", "slot_id": "linux-0001", "pair_id": "pair", "name": "primary", "uid": "primary-uid", "job": map[string]any{"job_id": "job"}}
	pair := map[string]any{"ID": "pair", "SlotID": "linux-0001", "Base": "ubuntu-24.04-arm64", "Phase": "handoff-intent", "LaunchID": "launch", "Primary": map[string]any{"Name": "primary"}, "PrimaryUID": "primary-uid"}
	writeStatusFixture(t, o.vmHome, scope, "primary", map[string]any{"pair": pair, "job": job}, true)
	r, err := readPoolStatus(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range r.Slots {
		if s.ID == "linux-0001" {
			if s.State != "handoff uncertain" || len(s.Jobs) != 1 {
				t.Fatal("assignment hid uncertainty", s.State)
			}
			return
		}
	}
	t.Fatal("missing assigned slot")
}

func TestStatusCommandWorksWithControllerLockAndDoesNotSave(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	vmHome := filepath.Join(home, "vms-home")
	legacy := filepath.Join(home, ".pulse", "provider.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"version":1,"flags":{"owner":"server","vm-home":"../vms-home","macos-capacity":"0","linux-capacity":"1","collector":"unconfigured.example:8443"}}`)
	if err := os.WriteFile(legacy, original, 0600); err != nil {
		t.Fatal(err)
	}
	scope := (config.Settings{Config: config.Config{OwnerID: "server"}, Kind: "vz", Endpoint: vmHome}).Scope()
	lockDir := filepath.Join(vmHome, ".pulse", scope)
	if err := os.MkdirAll(lockDir, 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(lockDir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	cmd := NewCommand("test")
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"status", "--json"})
	if err = cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	var report poolStatus
	if err = json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Slots) != 1 || report.Slots[0].ID != "linux-0001" || report.Home != vmHome {
		t.Fatal("saved pool options not loaded", out.String())
	}
	if !strings.Contains(stderr.String(), "legacy") {
		t.Fatal("missing legacy warning")
	}
	images := NewCommand("test")
	out.Reset()
	images.SetOut(&out)
	images.SetErr(&stderr)
	images.SetArgs([]string{"images", "--json"})
	if err = images.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Slots) != 1 || report.Home != vmHome {
		t.Fatal("images did not load pool settings")
	}
	now, err := os.ReadFile(legacy)
	if err != nil || !bytes.Equal(original, now) {
		t.Fatal("legacy configuration changed")
	}
	if _, err = os.Stat(filepath.Join(home, ".pulse", "vz", "provider.json")); !os.IsNotExist(err) {
		t.Fatal("status saved configuration")
	}
	if _, err = os.Stat(filepath.Join(vmHome, "vms")); !os.IsNotExist(err) {
		t.Fatal("status created VM storage")
	}
}

func TestStatusUnknownNativeStateAndCanceledContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	o := statusOptions(t)
	scope := (config.Settings{Config: o.config, Kind: "vz", Endpoint: o.vmHome}).Scope()
	dir := writeStatusFixture(t, o.vmHome, scope, "bad", map[string]any{"pair": map[string]any{"ID": "bad", "SlotID": "linux-0001", "Phase": "ready", "Primary": map[string]any{"Name": "bad"}, "PrimaryUID": "bad-uid"}}, false)
	if err := os.Remove(filepath.Join(dir, "config.json")); err != nil {
		t.Fatal(err)
	}
	r, err := readPoolStatus(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Warnings) == 0 {
		t.Fatal("unknown state concealed")
	}
	for _, s := range r.Slots {
		if s.ID == "linux-0001" && s.State == "idle" {
			t.Fatal("unknown VM treated as idle")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = readPoolStatus(ctx, o); err == nil {
		t.Fatal("ignored cancellation")
	}
}
