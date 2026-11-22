package vz

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPreparedCheckpointMoveAndRestorePreserveDiskAndIdentity(t *testing.T) {
	s, src := fixture(t)
	dir := filepath.Join(s.Home, ".pulse", "test", "previous-images")
	original, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.MovePrepared(t.Context(), "source", dir, false); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("old name remains")
	}
	if err = s.MovePrepared(t.Context(), "source", dir, true); err != nil {
		t.Fatal(err)
	}
	restored, err := os.Stat(src)
	if err != nil || !os.SameFile(original, restored) {
		t.Fatal("changed directory identity", err)
	}
	for _, file := range []string{"disk.img", "nvram.bin", "pulse.json", ".pulse-owner"} {
		data, err := os.ReadFile(filepath.Join(src, file))
		if err != nil || string(data) != "private fixture" {
			t.Fatal(file, err)
		}
	}
}
func TestPreparedCheckpointRejectsLiveRunnerAndExistingDestination(t *testing.T) {
	s, src := fixture(t)
	dir := filepath.Join(s.Home, "previous")
	lock, err := lockFile(t.Context(), filepath.Join(src, "pulse.run.lock"), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.MovePrepared(t.Context(), "source", dir, false); err == nil {
		t.Fatal("moved live runner")
	}
	lock.Close()
	if err = os.MkdirAll(filepath.Join(dir, "source"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = s.MovePrepared(t.Context(), "source", dir, false); err == nil {
		t.Fatal("replaced destination")
	}
	if _, err = os.Stat(src); err != nil {
		t.Fatal("lost source", err)
	}
}
