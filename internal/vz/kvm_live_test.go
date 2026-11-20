//go:build darwin && arm64

package vz

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/colony-2/pulse-tart/internal/backend"
	"github.com/colony-2/pulse-tart/internal/local"
)

// Called only by the opted-in Linux lifecycle test, on its private clone of a
// prepared Ubuntu base. Production images and allocations are never modified.
func verifyLiveKVM(t *testing.T, ctx context.Context, b *local.Client, vm backend.VM) {
	t.Helper()
	primary, err := os.ReadFile("../../scripts/docker-primary.sh")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := b.Exec(ctx, vm.Name, vm.UID, []byte("sudo -n /bin/bash -s -- plain <<'PULSE_PRIMARY'\n"+string(primary)+"\nPULSE_PRIMARY\n")); err != nil {
		t.Fatal("configure ordinary Linux worker", string(out), err)
	}
	script, err := os.ReadFile("../../tests/integration/kvm-guest.sh")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("testing unprivileged KVM API and booting a nested ARM guest (may download QEMU)")
	out, err := b.Exec(ctx, vm.Name, vm.UID, []byte("sudo -n /bin/bash <<'PULSE_KVM_TEST' 2>&1\n"+string(script)+"\nPULSE_KVM_TEST\nstatus=$?\necho PULSE_KVM_TEST_EXIT=$status\nexit 0\n"))
	if err != nil || !strings.Contains(string(out), "PULSE_NESTED_KVM_BOOT_PASS") || !strings.Contains(string(out), "PULSE_KVM_TEST_EXIT=0") {
		t.Fatal("nested KVM boot", string(out), err)
	}
	t.Log(strings.TrimSpace(string(out)))
}
