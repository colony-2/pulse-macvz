package tart

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNativeVolumeUUID(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	a, err := nativeVolumeUUID(root)
	if err != nil || len(a) != 32 {
		t.Fatal("read volume UUID", a, err)
	}
	b, err := nativeVolumeUUID(child)
	if err != nil || a != b {
		t.Fatal("same volume has different UUIDs", a, b, err)
	}
	if _, err := nativeVolumeUUID(filepath.Join(root, "absent")); err == nil {
		t.Fatal("absent path accepted")
	}
}
