package local

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
)

// These commands only observe receipt state. They never ingest a launch or
// restart preparation. The provider verifies that the VM is an unused builder.
const observeMacTemplate = `set -eu
sudo -n /opt/pulse-tart/bin/python3 - "$expected" <<'PY'
import json,pathlib,re,subprocess,sys
root=pathlib.Path('/var/db/pulse-tart-template')
boot=subprocess.check_output(['/usr/sbin/sysctl','-n','kern.bootsessionuuid'],text=True).strip()
version=subprocess.check_output(['/usr/bin/sw_vers','-productVersion'],text=True).strip()
state='absent'; receipt=None
if version != sys.argv[1] or (root/'failed').exists(): state='failed'
elif (root/'ready').exists():
    try:
        receipt=json.loads((root/'ready').read_text())
        net=subprocess.check_output(['/sbin/ifconfig','en0'],text=True)
        mac=re.search(r'\bether\s+([0-9a-f:]{17})',net).group(1)
        state='ready' if receipt['boot_id']==boot and receipt['mac']==mac else 'failed'
    except (ValueError,KeyError,AttributeError): state='failed'
elif root.exists(): state='preparing'
log=pathlib.Path('/var/log/pulse-tart-template.log')
print('PULSE_BASE_OBSERVATION',boot,state,log.stat().st_size if log.exists() else 0)
if state=='ready': print('PULSE_TEMPLATE_READY',json.dumps({'boot_id':boot,'mac':mac}))
PY
`

const observeCompanionBase = `set -eu
boot=$(cat /proc/sys/kernel/random/boot_id)
state=absent
if sudo -n systemctl is-failed --quiet pulse-docker-image.service; then state=failed
elif sudo -n test -f /var/lib/pulse-docker-image/ready || sudo -n journalctl -u pulse-docker-image --no-pager -o cat | grep -q '^PULSE_DOCKER_ENGINE_BASE_READY$'; then
  state=ready
  command -v docker >/dev/null || state=failed
  test -x /usr/sbin/sshd || state=failed
elif sudo -n test -d /var/lib/pulse-docker-image; then state=preparing
fi
printf 'PULSE_BASE_OBSERVATION %s %s 0\n' "$boot" "$state"
`

const shutdownReadyCompanionBase = `set -eu
sudo -n test -f /var/lib/pulse-docker-image/ready || sudo -n journalctl -u pulse-docker-image --no-pager -o cat | grep -q '^PULSE_DOCKER_ENGINE_BASE_READY$'
sync
sudo -n /sbin/shutdown -h now
`

func (b *Client) recordTemplateReady(r *record, data []byte) error {
	observation, _, _ := strings.Cut(string(data), "\n")
	boot, state, _, err := parseBaseObservation([]byte(observation))
	if err != nil || state != "ready" {
		return errors.New("template readiness observation missing")
	}
	for _, line := range strings.Split(string(data), "\n") {
		payload, ok := strings.CutPrefix(line, "PULSE_TEMPLATE_READY ")
		if !ok {
			continue
		}
		var receipt struct {
			Boot string `json:"boot_id"`
			MAC  string `json:"mac"`
		}
		if json.Unmarshal([]byte(payload), &receipt) != nil || receipt.Boot != boot {
			return errors.New("template receipt boot identity mismatch")
		}
		mac, err := net.ParseMAC(receipt.MAC)
		if err != nil || len(mac) != 6 || !strings.EqualFold(mac.String(), r.MAC) {
			return errors.New("template receipt MAC identity mismatch")
		}
		// Persist exactly the verified fields, never arbitrary guest output.
		encoded, _ := json.Marshal(receipt)
		f, err := os.OpenFile(b.artifact(r.Spec.Name, "events"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
		if err != nil {
			return err
		}
		_, err = f.WriteString("PULSE_TEMPLATE_READY " + string(encoded) + "\n")
		if err == nil {
			err = f.Sync()
		}
		err = errors.Join(err, f.Close())
		if err == nil {
			b.deliverySucceeded(r)
			if !r.Delivered {
				return errors.New("could not persist template readiness")
			}
		}
		return err
	}
	return errors.New("template readiness receipt missing")
}
