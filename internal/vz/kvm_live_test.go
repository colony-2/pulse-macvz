//go:build darwin && arm64

package vz

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse-macvz/internal/backend"
	"github.com/colony-2/pulse-macvz/internal/local"
)

// Called only by the opted-in Linux lifecycle test, on its private clone of a
// prepared Ubuntu base. Production images and allocations are never modified.
func verifyLiveKVM(t *testing.T, ctx context.Context, b *local.Client, vm backend.VM) {
	t.Helper()
	setup := "set -eu\nsudo -n install -d -m 700 /var/lib/pulse-docker\n"
	for _, name := range []string{"setup-linux-nix.sh", "setup-linux-kvm.sh", "linux-kvm-network.py", "docker-primary.sh", "docker-engine.sh"} {
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
	modes := []string{"plain"}
	if os.Getenv("PULSE_TEST_VZ_KVM_DOCKER") == "1" {
		modes = append(modes, "docker")
	}
	for _, mode := range modes {
		if mode == "docker" {
			setup := `sudo -n /bin/bash <<'PULSE_DOCKER_SETUP' 2>&1
set -eu
# Select the installed engine, not a possibly different Nix CLI in the login PATH.
version=$(/usr/bin/docker --version | awk '{gsub(/,/,"",$3); print $3}')
bash /var/lib/pulse-docker/docker-engine.sh "$version" 4
# Retain failing stage output before the disposable VM is stopped.
if ! bash /var/lib/pulse-docker/docker-primary.sh docker; then
 journalctl -u pulse-kvm-network -u pulse-kvm-ready --no-pager -n 60
 cat /etc/pulse-macvz/network.json
 ip -j -4 route show table all
 sync
 exit 1
fi
PULSE_DOCKER_SETUP
status=$?
echo PULSE_DOCKER_SETUP_EXIT=$status
exit 0
`
			if out, err := b.Exec(ctx, vm.Name, vm.UID, []byte(setup)); err != nil || !strings.Contains(string(out), "PULSE_DOCKER_SETUP_EXIT=0") {
				t.Fatal("configure Docker coexistence", string(out), err)
			}
		}
		t.Log("testing ordinary-user KVM, nested networking and supervision; mode=" + mode)
		// Management SSH commands are bounded to 30 seconds. Let systemd own
		// downloads and nested guest boot, then poll a finite result receipt.
		path := "/var/tmp/pulse-kvm-qualification-" + mode
		start := "set -eu\nprintf '%s' '" + base64.StdEncoding.EncodeToString(script) + "' | base64 -d | sudo -n tee " + path + ".sh >/dev/null\n"
		start += "sudo -n systemd-run --quiet --collect --unit=pulse-kvm-qualification-" + mode + " /bin/bash -c 'bash " + path + ".sh >" + path + ".log 2>&1; echo $? >" + path + ".exit'\n"
		if out, err := b.Exec(ctx, vm.Name, vm.UID, []byte(start)); err != nil {
			t.Fatal("start nested qualification", string(out), err)
		}
		var out []byte
		for {
			var err error
			out, err = b.Exec(ctx, vm.Name, vm.UID, []byte("if sudo -n test -f "+path+".exit; then sudo -n tail -c 20000 "+path+".log; printf 'PULSE_KVM_TEST_EXIT='; sudo -n cat "+path+".exit; else echo PENDING; fi\n"))
			if err != nil {
				t.Fatal("observe nested qualification", err)
			}
			if strings.Contains(string(out), "PULSE_KVM_TEST_EXIT=") {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(time.Second):
			}
		}
		if !strings.Contains(string(out), "PULSE_NESTED_KVM_BOOT_PASS") || !strings.Contains(string(out), "PULSE_NESTED_NETWORK_PASS") || !strings.Contains(string(out), "PULSE_KVM_TEST_EXIT=0") {
			t.Fatal("nested KVM networking", string(out))
		}
		t.Log(strings.TrimSpace(string(out)))
	}

}

// Exercise the installed launcher's real root-to-worker UID transition. The
// synthetic qualification envelope carries no production credentials or JobDB work.
func verifyLiveKVMHandoff(t *testing.T, ctx context.Context, b *local.Client, vm backend.VM) {
	t.Helper()
	command := `set -eu
 test "$(id -u)" = 55050
 test "$(id -g)" = 55050
 test "$XDG_RUNTIME_DIR" = /run/user/55050
 test "$DBUS_SESSION_BUS_ADDRESS" = unix:path=/run/user/55050/bus
 /usr/bin/python3 /usr/local/libexec/pulse-macvz/linux-kvm-network.py --probe
 grep '^CapEff:' /proc/self/status | grep -q 0000000000001000
 echo PULSE_KVM_HANDOFF_PASS > /home/pulseworker/work/kvm-handoff-ready
 sleep 120
`
	payload, err := json.Marshal(map[string]any{"version": 1, "launch_id": "kvm-network-qualification", "process": map[string]any{
		"command": []string{"/usr/local/bin/pulse-kvm-run", "/bin/sh"}, "args": []string{"-c", command},
		"env":         map[string]string{"XDG_RUNTIME_DIR": "/run/user/55050", "DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/55050/bus"},
		"working_dir": "/home/pulseworker/work",
	}})
	if err != nil {
		t.Fatal(err)
	}
	script := "set -eu\nprintf '%s' '" + base64.StdEncoding.EncodeToString(payload) + "' | base64 -d | sudo -n /opt/pulse-tart/bin/pulse-tart-guest ingest\nsudo -n bash /opt/pulse-tart-image/bootstrap-guest.sh\n"
	script += `for i in $(seq 1 30); do
 if sudo -n test -f /home/pulseworker/work/kvm-handoff-ready; then
  sudo -n cat /home/pulseworker/work/kvm-handoff-ready
  if sudo -n bash /var/lib/pulse-docker/setup-linux-kvm.sh; then
   echo 'setup unexpectedly mutated accepted allocation'; exit 1
  fi
  echo PULSE_KVM_ACCEPTED_GUARD_PASS
  exit 0
 fi
 sleep 1
done
exit 1
`
	out, err := b.Exec(ctx, vm.Name, vm.UID, []byte(script))
	if err != nil || !strings.Contains(string(out), "PULSE_KVM_HANDOFF_PASS") || !strings.Contains(string(out), "PULSE_KVM_ACCEPTED_GUARD_PASS") {
		t.Fatal("KVM launcher qualification", string(out), err)
	}
	t.Log(strings.TrimSpace(string(out)))
}
