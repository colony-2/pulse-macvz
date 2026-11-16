package tart

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colony-2/pulse-tart/internal/config"
)

func reopenProperties(t *testing.T, b *Client, m *mockNative, legacy string) *Client {
	t.Helper()
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := open(b.cfg, config.Config{StateDir: legacy, BootstrapTimeout: time.Minute, Bases: b.bases})
	if err != nil {
		t.Fatal(err)
	}
	next.command = m.run
	t.Cleanup(func() { next.Close() })
	return next
}

func TestRestartBetweenNativeDispatchAndResultSave(t *testing.T) {
	b, m, spec := fixture(t)
	release := filepath.Join(b.cfg.Home, "release")
	binary := filepath.Join(t.TempDir(), "tart-stub")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nwhile [ ! -f \"$TART_HOME/release\" ]; do sleep 0.02; done\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
	b.cfg.Binary = binary
	b.launch = b.start
	if _, err := b.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	// Model a crash after cmd.Start but before persisting its returned PID.
	r := b.records[spec.Name]
	r.Phase, r.PID = "starting", 0
	if err := b.save(r); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	h := m.vms[spec.Name]
	h.State = "suspended"
	m.vms[spec.Name] = h
	m.mu.Unlock()
	b = reopenProperties(t, b, m, "")
	v, err := b.Get(t.Context(), spec.Name)
	if err != nil || v.Status == "failed" {
		t.Fatal("live detached launch classified as terminal", v.Status, err)
	}
	if err = b.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	phase, delivering := b.records[spec.Name].Phase, b.workers[spec.Name]
	b.mu.Unlock()
	if phase != "started" || !delivering {
		t.Fatal("delivery not recovered", phase, delivering)
	}
	if err = os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		v, err = b.Get(t.Context(), spec.Name)
		if err != nil {
			t.Fatal(err)
		}
		if v.Status == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("exited launch retained occupancy")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPropertiesTravelWithVMAndRecoverWithoutLegacyDirectory(t *testing.T) {
	b, m, spec := fixture(t)
	props := json.RawMessage(`{"role":"job","launch_id":"one"}`)
	if err := b.WriteVMProperties(map[string]json.RawMessage{spec.Name: props}); err != nil {
		t.Fatal(err)
	}
	// Runtime recovery may observe inventory between provider staging and
	// native Create. It must not erase the properties attached before dispatch.
	if all, err := b.ReadVMProperties(); err != nil || len(all) != 0 {
		t.Fatal(all, err)
	}
	v, err := b.Create(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	path := b.artifact(spec.Name, "json")
	data, err := privateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var attached vmFile
	if err = json.Unmarshal(data, &attached); err != nil || attached.Native.UID != v.UID || !bytes.Equal(attached.Properties, props) {
		t.Fatal("missing identity/properties", err)
	}
	legacy := b.legacyDir
	b = reopenProperties(t, b, m, "")
	if err = os.RemoveAll(legacy); err != nil {
		t.Fatal(err)
	}
	all, err := b.ReadVMProperties()
	if err != nil || !bytes.Equal(all[spec.Name], props) {
		t.Fatal("restart lost properties", all, err)
	}
	list, err := b.List(t.Context())
	if err != nil || len(list) != 1 || list[0].UID != v.UID {
		t.Fatal("restart lost VM", list, err)
	}
	if len(b.workers) != 0 {
		t.Fatal("read started delivery")
	}
	if err = os.RemoveAll(b.path(spec.Name)); err != nil {
		t.Fatal(err)
	}
	all, err = b.ReadVMProperties()
	if err != nil || len(all) != 0 || len(b.records) != 0 {
		t.Fatal("deleted VM retained metadata", all, err)
	}
}

func TestExitedSuspendedJobIsTerminalButTemplateIsReusable(t *testing.T) {
	b, m, spec := fixture(t)
	if err := b.WriteVMProperties(map[string]json.RawMessage{spec.Name: json.RawMessage(`{"slot":{}}`)}); err != nil {
		t.Fatal(err)
	}
	v, err := b.Create(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Suspend(t.Context(), spec.Name, v.UID); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(b.artifact(spec.Name, "exit"), []byte("0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	v, err = b.Get(t.Context(), spec.Name)
	if err != nil || v.Status == "failed" {
		t.Fatal("successful template suspension became failure", v.Status, err)
	}
	if err = b.RetainSource(t.Context(), spec.Name, v.UID); err != nil {
		t.Fatal(err)
	}
	b.launch = func(string) (int, error) { return os.Getpid(), nil }
	spec.Name, spec.Image = "failed-job", spec.Name
	if err = b.WriteVMProperties(map[string]json.RawMessage{spec.Name: json.RawMessage(`{"job":{}}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(b.artifact(spec.Name, "exit"), []byte("1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(b.artifact(spec.Name, "run.log"), []byte("snapshot restore: permission denied\n"), 0600); err != nil {
		t.Fatal(err)
	}
	b = reopenProperties(t, b, m, "")
	v, err = b.Get(t.Context(), spec.Name)
	if err != nil || v.PowerState != "suspended" || v.Status != "failed" || !v.Stopped() {
		t.Fatal("failed restore remains active", v.Status, err)
	}
	if err = b.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(b.workers) != 0 {
		t.Fatal("recovery retried a finished native run")
	}
	if err = b.Delete(t.Context(), spec.Name); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(b.artifact(spec.Name, "json")); !os.IsNotExist(err) {
		t.Fatal("deleted VM retained metadata", err)
	}
	v, err = b.Get(t.Context(), "template")
	if err != nil || v.Status == "failed" {
		t.Fatal("cleanup touched suspended source", err)
	}
}

func TestLegacyNativeMigrationIsScopedAndReadOnly(t *testing.T) {
	b, m, spec := fixture(t)
	v, err := b.Create(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Suspend(t.Context(), spec.Name, v.UID); err != nil {
		t.Fatal(err)
	}
	if err = b.Close(); err != nil {
		t.Fatal(err)
	}
	legacy := b.legacyDir
	oldDir := filepath.Join(legacy, "native")
	if err = os.MkdirAll(oldDir, 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(b.records[spec.Name])
	if err = os.WriteFile(filepath.Join(oldDir, spec.Name+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(oldDir, spec.Name+".exit"), []byte("1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	header, _ := json.Marshal(map[string]any{"version": 3, "scope": b.scope})
	if err = os.WriteFile(filepath.Join(legacy, "state.json"), header, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(b.artifact(spec.Name, "json")); err != nil {
		t.Fatal(err)
	}
	wrong, err := open(b.cfg, config.Config{OwnerID: "different", StateDir: legacy})
	if err == nil {
		wrong.Close()
		t.Fatal("migrated another owner")
	}
	b = reopenProperties(t, b, m, legacy)
	v, err = b.Get(t.Context(), spec.Name)
	if err != nil || v.UID == "" || v.Status != "failed" {
		t.Fatal("migration lost failed launch", v.Status, err)
	}
	after, _ := os.ReadFile(filepath.Join(oldDir, spec.Name+".json"))
	if !bytes.Equal(raw, after) {
		t.Fatal("migration modified legacy native file")
	}
	if _, err = privateFile(b.artifact(spec.Name, "json")); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(legacy, "state.json")); err != nil {
		t.Fatal(err)
	}
	// Once all native files are attached, an obsolete import directory cannot
	// become a dependency again on a subsequent restart.
	b = reopenProperties(t, b, m, legacy)
}

func TestPoolLockIndependentOfLegacyStateDirectory(t *testing.T) {
	b, _, _ := fixture(t)
	other, err := open(b.cfg, config.Config{StateDir: t.TempDir()})
	if err == nil {
		other.Close()
		t.Fatal("two runtimes acquired same Tart owner with different StateDirs")
	}
}

func TestAsynchronousSuspendIsNotRepeatedAcrossRestart(t *testing.T) {
	b, m, spec := fixture(t)
	v, err := b.Create(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	b.command = func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] == "suspend" {
			requests++
			// Actual Tart returns after signalling. While saving the snapshot,
			// its inventory continues to report running for many polling cycles.
			return nil, nil
		}
		return m.run(ctx, args...)
	}
	for range 3 {
		if err = b.Suspend(t.Context(), spec.Name, v.UID); err != nil {
			t.Fatal(err)
		}
	}
	if requests != 1 {
		t.Fatal("snapshot interrupted by repeated suspend", requests)
	}
	b = reopenProperties(t, b, m, "")
	b.command = func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] == "suspend" {
			requests++
			return nil, nil
		}
		return m.run(ctx, args...)
	}
	for range 3 {
		if err = b.Suspend(t.Context(), spec.Name, v.UID); err != nil {
			t.Fatal(err)
		}
	}
	if requests != 1 {
		t.Fatal("restart replayed suspend", requests)
	}
	m.mu.Lock()
	h := m.vms[spec.Name]
	h.State = "suspended"
	m.vms[spec.Name] = h
	m.mu.Unlock()
	if err = b.Suspend(t.Context(), spec.Name, v.UID); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatal("completed snapshot signalled again")
	}
}

func TestIncompleteCloneRecoversForCleanup(t *testing.T) {
	b, m, spec := fixture(t)
	m.ambiguous = true // clone exists, but command returns an error before launch.
	if _, err := b.Create(t.Context(), spec); err == nil {
		t.Fatal("expected clone error")
	}
	// No run was dispatched. The partial clone carries creation properties,
	// so restart can remove it without reissuing clone or run.
	b = reopenProperties(t, b, m, "")
	props, err := b.ReadVMProperties()
	if err != nil || len(props) != 1 {
		t.Fatal(props, err)
	}
	v, err := b.Get(t.Context(), spec.Name)
	if err != nil || v.Status != "failed" || !v.Stopped() {
		t.Fatal(v.Status, err)
	}
	if err = b.Delete(t.Context(), spec.Name); err != nil {
		t.Fatal(err)
	}
}
