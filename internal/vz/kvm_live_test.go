//go:build darwin && arm64

package vz

import (
	"context"
	"encoding/base64"
	"os"
	"strings"
	"testing"

	"github.com/colony-2/pulse-macvz/internal/backend"
	"github.com/colony-2/pulse-macvz/internal/local"
)

// Called only by the opted-in Linux lifecycle test, on its private clone of a
// prepared Ubuntu base. Production images and allocations are never modified.
func verifyLiveKVM(t *testing.T, ctx context.Context, b *local.Client, vm backend.VM) {
	t.Helper()
	setup := "set -eu\nsudo -n install -d -m 700 /var/lib/pulse-docker\n"
	for _, name := range []string{"setup-linux-nix.sh", "docker-primary.sh"} {
		data, err := os.ReadFile("../../scripts/" + name)
		if err != nil {
			t.Fatal(err)
		}
		setup += "printf '%s' '" + base64.StdEncoding.EncodeToString(data) + "' | base64 -d | sudo -n tee /var/lib/pulse-docker/" + name + " >/dev/null\n"
	}
	setup += "sudo -n /bin/bash /var/lib/pulse-docker/docker-primary.sh plain\n"
	if out, err := b.Exec(ctx, vm.Name, vm.UID, []byte(setup)); err != nil {
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
