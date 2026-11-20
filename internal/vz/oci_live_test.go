//go:build darwin && arm64

package vz

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in registry-to-guest gate, including the verified shared cache path.
func TestLiveLinuxOCI(t *testing.T) {
	reference, root, runner := os.Getenv("PULSE_VZ_OCI_IMAGE"), os.Getenv("PULSE_VZ_RESULTS"), os.Getenv("PULSE_VZ_RUNNER")
	if reference == "" || root == "" || runner == "" {
		t.Skip("set PULSE_VZ_OCI_IMAGE to a pinned Linux image, plus PULSE_VZ_RESULTS and PULSE_VZ_RUNNER")
	}
	home, err := os.MkdirTemp(root, "vz-oci-")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("registry import artifacts:", home)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()
	store := &Store{Home: home, PullTimeout: 15 * time.Minute}
	if err := store.Clone(ctx, reference, "source"); err != nil {
		t.Fatal(err)
	}
	if err := store.Clone(ctx, reference, "cache-reuse"); err != nil {
		t.Fatal("reuse verified image:", err)
	}
	if err := store.Delete(ctx, "cache-reuse"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PULSE_VZ_LINUX_BASE", filepath.Join(home, "vms", "source"))
	TestLiveLinuxLifecycle(t)
}
