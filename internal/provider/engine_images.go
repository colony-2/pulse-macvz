package provider

import (
	"fmt"
	"time"

	"github.com/colony-2/pulse-macvz/internal/config"
)

func (p *Provider) engineBaseName(ep config.EngineProfile) string {
	return config.EngineBaseImageName(p.Config.OwnerID, ep)
}

func (p *Provider) engineBases() map[string]config.EngineProfile {
	out := map[string]config.EngineProfile{}
	for _, g := range p.Config.Slots {
		if g.Count > 0 && g.DockerProfile != "" && p.base(g.Base).OS != "linux" {
			ep := p.Config.Engine(g.DockerProfile)
			out[p.engineBaseName(ep)] = ep
		}
	}
	return out
}

func engineBase(ep config.EngineProfile) config.Base {
	return config.Base{OS: "linux", Arch: "arm64", Image: ep.Image, SSHUsername: ep.SSHUsername, SSHPassword: ep.SSHPassword, SSHHostKey: ep.SSHHostKey}
}

// Build and stop a software-only source before any companion is allocated.
// systemd owns the installation across management SSH disconnects.
func (p *Provider) engineBaseStartup(ep config.EngineProfile) (string, error) {
	script, err := p.asset("prepare-docker-engine.sh")
	if err != nil {
		return "", err
	}
	seconds := int(p.Config.BootstrapTimeout / time.Second)
	return "exec /bin/bash <<'PULSE_ENGINE_BASE'\nset -eu\nset +x\numask 077\nsudo -n install -d -m 700 /var/lib/pulse-docker-image\n" +
		fileScript("/var/lib/pulse-docker-image/prepare.sh", script, true) +
		fmt.Sprintf(`if ! sudo -n systemctl cat pulse-docker-image.service >/dev/null 2>&1; then
 sudo -n systemd-run --no-block --unit=pulse-docker-image --service-type=oneshot --property=RemainAfterExit=yes /bin/bash /var/lib/pulse-docker-image/prepare.sh %s
fi
for ((i=0;i<%d;i++)); do
 if sudo -n systemctl is-failed --quiet pulse-docker-image; then
  echo PULSE_BASE_FAILED
  sudo -n journalctl -u pulse-docker-image --no-pager -o cat -n 100
  sudo -n shutdown -h now
  exit 1
 fi
 if sudo -n journalctl -u pulse-docker-image --no-pager -o cat | grep -q '^PULSE_DOCKER_ENGINE_BASE_READY$'; then
  echo PULSE_BASE_READY
  sync
  sudo -n shutdown -h now
  exit 0
 fi
 sleep 1
done
echo PULSE_BASE_FAILED
sudo -n shutdown -h now
exit 1
PULSE_ENGINE_BASE
`, shell(ep.EngineVersion), seconds), nil
}
