package local

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type checkpointHypervisor struct {
	Hypervisor
	b             *Client
	m             *mockNative
	failMove      bool
	failAfterMove bool
}

func (h *checkpointHypervisor) MovePrepared(ctx context.Context, name, dir string, restore bool) error {
	if h.failMove {
		h.failMove = false
		return errors.New("injected interruption")
	}
	src, dst := h.b.path(name), filepath.Join(dir, name)
	if restore {
		src, dst = dst, src
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		return errors.New("destination exists")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	if h.m != nil {
		h.m.mu.Lock()
		if restore {
			data, err := os.ReadFile(filepath.Join(dst, "pulse.json"))
			if err != nil {
				h.m.mu.Unlock()
				return err
			}
			var v vmFile
			if err = json.Unmarshal(data, &v); err != nil {
				h.m.mu.Unlock()
				return err
			}
			r := v.Native
			h.m.vms[name] = hardware{Name: name, CPU: r.Spec.CPU, Memory: r.Spec.Memory, MACAddress: r.MAC, State: "stopped"}
		} else {
			delete(h.m.vms, name)
		}
		h.m.mu.Unlock()
	}
	if h.failAfterMove {
		h.failAfterMove = false
		return errors.New("injected post-rename interruption")
	}
	return nil
}
func checkpointFixture(t *testing.T) (*Client, *mockNative, *checkpointHypervisor) {
	b, m, _ := fixture(t)
	h := &checkpointHypervisor{Hypervisor: b.native, b: b, m: m}
	b.native = h
	preparedFixture(t, b, m, "base", "original", "ready")
	if err := os.WriteFile(filepath.Join(b.path("base"), "disk.img"), []byte("working image"), 0600); err != nil {
		t.Fatal(err)
	}
	return b, m, h
}
func TestUpgradeCheckpointInterruptedStagingAndRollback(t *testing.T) {
	b, m, h := checkpointFixture(t)
	h.failMove = true
	if err := b.BeginImageUpgrade(t.Context(), []string{"base"}); err == nil {
		t.Fatal("expected interruption")
	}
	if phase, _ := b.ImageUpgradeState(); phase != "staging" {
		t.Fatal(phase)
	}
	if err := b.BeginImageUpgrade(t.Context(), []string{"base"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(b.path("base")); !os.IsNotExist(err) {
		t.Fatal("base not staged")
	}
	if key, err := b.UpgradePropertyKey(); err != nil || string(key) != strings.Repeat("k", 32) {
		t.Fatal("lost property key", err)
	}
	// Re-open the process from only persisted checkpoint and VM metadata.
	b = reopenProperties(t, b, m, "")
	h = &checkpointHypervisor{Hypervisor: b.native, b: b, m: m}
	b.native = h
	preparedFixture(t, b, m, "base", "replacement", "failed")
	if err := b.CommitImageUpgrade(t.Context()); err == nil {
		t.Fatal("committed failed image")
	}
	h.failMove = true
	if _, err := b.RollbackImageUpgrade(t.Context()); err == nil {
		t.Fatal("expected rollback interruption")
	}
	if phase, _ := b.ImageUpgradeState(); phase != "rollback" {
		t.Fatal(phase)
	}
	done, err := b.RollbackImageUpgrade(t.Context())
	if err != nil || !done {
		t.Fatal(done, err)
	}
	if b.records["base"].UID != "original" {
		t.Fatal("did not restore identity")
	}
	disk, err := os.ReadFile(filepath.Join(b.path("base"), "disk.img"))
	if err != nil || string(disk) != "working image" {
		t.Fatal("lost previous disk", err)
	}
	if phase, err := b.ImageUpgradeState(); err != nil || phase != "" {
		t.Fatal(phase, err)
	}
}
func TestUpgradeCommitKeepsReplacementAndRemovesCheckpoint(t *testing.T) {
	b, m, _ := checkpointFixture(t)
	if err := b.BeginImageUpgrade(t.Context(), []string{"base"}); err != nil {
		t.Fatal(err)
	}
	if err := b.ActivateImageUpgrade(t.Context()); err != nil {
		t.Fatal(err)
	}
	preparedFixture(t, b, m, "base", "replacement", "ready")
	if err := b.CommitImageUpgrade(t.Context()); err != nil {
		t.Fatal(err)
	}
	if b.records["base"].UID != "replacement" {
		t.Fatal("replacement changed")
	}
	if _, err := os.Stat(b.upgradeDir()); !os.IsNotExist(err) {
		t.Fatal("checkpoint not removed")
	}
}
func TestUpgradeRollbackRejectsAcceptedReplacement(t *testing.T) {
	b, m, _ := checkpointFixture(t)
	if err := b.BeginImageUpgrade(t.Context(), []string{"base"}); err != nil {
		t.Fatal(err)
	}
	preparedFixture(t, b, m, "base", "replacement", "failed")
	var p map[string]any
	_ = json.Unmarshal(b.properties["base"], &p)
	p["job"] = map[string]string{"id": "accepted"}
	b.properties["base"], _ = json.Marshal(p)
	if err := b.save(b.records["base"]); err != nil {
		t.Fatal(err)
	}
	if _, err := b.RollbackImageUpgrade(t.Context()); err == nil {
		t.Fatal("rollback removed accepted work")
	}
	if b.records["base"].UID != "replacement" {
		t.Fatal("accepted VM mutated")
	}
	if err := b.checkpointIdentity(upgradeImage{Name: "base", UID: "original"}); err != nil {
		t.Fatal("checkpoint lost", err)
	}
}

func TestUpgradeResumesAfterRenameBeforeStateUpdate(t *testing.T) {
	b, m, h := checkpointFixture(t)
	h.failAfterMove = true
	if err := b.BeginImageUpgrade(t.Context(), []string{"base"}); err == nil {
		t.Fatal("expected post-rename failure")
	}
	if err := b.BeginImageUpgrade(t.Context(), []string{"base"}); err != nil {
		t.Fatal("could not resume staged rename", err)
	}
	if _, ok := b.records["base"]; ok {
		t.Fatal("retained moved source in inventory")
	}
	preparedFixture(t, b, m, "base", "replacement", "failed")
	h.failAfterMove = true
	if _, err := b.RollbackImageUpgrade(t.Context()); err == nil {
		t.Fatal("expected post-restore failure")
	}
	done, err := b.RollbackImageUpgrade(t.Context())
	if err != nil || !done {
		t.Fatal("could not resume restored rename", done, err)
	}
	if b.records["base"].UID != "original" {
		t.Fatal("wrong recovered identity")
	}
}
func TestUpgradeOnlyCheckpointsConfiguredBases(t *testing.T) {
	b, m, _ := checkpointFixture(t)
	preparedFixture(t, b, m, "obsolete", "old", "ready")
	if err := b.BeginImageUpgrade(t.Context(), []string{"base"}); err != nil {
		t.Fatal(err)
	}
	u, err := b.readUpgrade()
	if err != nil || len(u.Images) != 1 || u.Images[0].Name != "base" {
		t.Fatal("checkpoint requires obsolete replacement", u, err)
	}
	if _, err := os.Stat(b.path("obsolete")); err != nil {
		t.Fatal("moved obsolete base", err)
	}
}

func TestNewUpgradeResetsBudgetOnlyAfterRetirement(t *testing.T) {
	b, _, _ := checkpointFixture(t)
	dir := filepath.Join(b.dir, "preparation-retries")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(dir, "base.json")
	if err := atomicPrivateFile(ledger, []byte(`{"count":2,"uid":"previous"}`)); err != nil {
		t.Fatal(err)
	}
	if err := b.BeginImageUpgrade(t.Context(), []string{"base"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ledger); err != nil {
		t.Fatal("reset before retirement", err)
	}
	if err := b.ActivateImageUpgrade(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ledger); !os.IsNotExist(err) {
		t.Fatal("fresh upgrade has exhausted budget", err)
	}
	if err := atomicPrivateFile(ledger, []byte(`{"count":1,"uid":"current"}`)); err != nil {
		t.Fatal(err)
	}
	if err := b.BeginImageUpgrade(t.Context(), []string{"base"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ledger); err != nil {
		t.Fatal("resume reset budget", err)
	}
}
