package provider

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/colony-2/pulse-macvz/internal/backend"
	"github.com/colony-2/pulse-macvz/internal/config"
	api "github.com/colony-2/pulse-provider-api"
	"golang.org/x/crypto/ssh"
)

type dockerBackend interface {
	Exec(context.Context, string, string, []byte) ([]byte, error)
	Stop(context.Context, string, string) error
	Endpoint(context.Context, string, string, int) (bool, error)
}

// PairRecord owns unused warm capacity as well as accepted allocations. No lease
// is stored here: handoff travels over management SSH stdin, only to Primary.
type PairRecord struct {
	ID, SlotID, ConfigHash, Base, Profile, Phase   string
	Primary, Engine                                backend.Spec
	PrimaryUID, EngineUID, PrimaryHash, EngineHash string
	PrimaryDispatched, EngineDispatched            bool
	Port                                           int
	EngineID, LaunchID, DeliveryHash               string
	DockerRequested                                bool
	LinuxNixVersion                                int    // Idle repair revision; never changes the VM/config identity.
	RequestID                                      string // Preparation hint only; never an accepted launch or lease.
	Created                                        time.Time
}

func (p *Provider) dockerGroup(slot string) config.SlotGroup {
	for _, g := range p.Config.Slots {
		if strings.HasPrefix(slot, g.Name+"-") {
			suffix := strings.TrimPrefix(slot, g.Name+"-")
			if len(suffix) == 4 {
				if _, e := strconv.Atoi(suffix); e == nil {
					return g
				}
			}
		}
	}
	return config.SlotGroup{}
}
func (p *Provider) dockerHash(g config.SlotGroup) string {
	// Pool growth does not change an existing slot's execution contract.
	g.Count = 0
	// Refresh unused eager-Docker allocations; active jobs retain their contract.
	preparationVersion := 2
	return p.Store.hash(struct {
		Base               config.Base
		Group              config.SlotGroup
		Engine             config.EngineProfile
		Endpoint           config.Endpoint
		PreparationVersion int
	}{p.base(g.Base), g, p.Config.Engine(g.DockerProfile), p.Config.DockerEndpoint, preparationVersion})
}
func (p *Provider) dockerMatch(g config.SlotGroup, l api.Launch) bool {
	if g.DockerProfile == "" {
		return false
	}
	b := p.base(g.Base)
	b.Capabilities = append(slices.Clone(b.Capabilities), "docker")
	if !p.baseFits(b, l) {
		return false
	}
	p.observe("Docker group %s configured CPU millicores request=%d limit=%d", g.Name, l.CPUMillis, g.CPU*1000)
	if g.CPU*1000 < l.CPUMillis {
		return false
	}
	p.observe("Docker group %s configured memory bytes request=%d limit=%d", g.Name, l.MemoryBytes, (g.MemoryMiB-512)*(1<<20))
	return (g.MemoryMiB-512)*(1<<20) >= l.MemoryBytes
}
func shell(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func fileScript(path string, data []byte, linux bool) string {
	decode := "-D"
	if linux {
		decode = "-d"
	}
	return fmt.Sprintf("printf '%%s' %s | base64 %s | sudo -n tee %s >/dev/null\nsudo -n chmod 600 %s\n", shell(base64.StdEncoding.EncodeToString(data)), decode, shell(path), shell(path))
}
func keyPair() ([]byte, []byte, error) {
	pub, priv, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		return nil, nil, e
	}
	block, e := ssh.MarshalPrivateKey(priv, "")
	if e != nil {
		return nil, nil, e
	}
	key, e := ssh.NewPublicKey(pub)
	if e != nil {
		return nil, nil, e
	}
	return pem.EncodeToMemory(block), ssh.MarshalAuthorizedKey(key), nil
}
func (p *Provider) asset(name string) ([]byte, error) { return fs.ReadFile(p.Assets, "scripts/"+name) }
func (p *Provider) engineStartup(profile config.EngineProfile, host, client []byte) (string, error) {
	return p.engineScript(profile, host, client, true)
}

func (p *Provider) engineScript(profile config.EngineProfile, host, client []byte, wait bool) (string, error) {
	script, e := p.asset("docker-engine.sh")
	if e != nil {
		return "", e
	}
	s := "#!/bin/bash\nset -eu\nset +x\numask 077\nsudo -n install -d -m 700 /var/lib/pulse-docker\n"
	s += "if ! sudo -n systemctl cat pulse-docker-prepare.service >/dev/null 2>&1; then\n"
	s += fileScript("/var/lib/pulse-docker/prepare.sh", script, true)
	if len(host) > 0 {
		s += fileScript("/var/lib/pulse-docker/host-key", host, true) + fileScript("/var/lib/pulse-docker/authorized_keys", client, true)
	}
	// systemd owns preparation across management-process disconnection. No lease.
	s += fmt.Sprintf("sudo -n systemd-run --no-block --unit=pulse-docker-prepare --service-type=oneshot --property=RemainAfterExit=yes /bin/bash /var/lib/pulse-docker/prepare.sh %s %d; fi\n", shell(profile.EngineVersion), profile.DiskGiB)
	if wait {
		s += "for ((i=0;i<900;i++)); do\n if sudo -n systemctl is-failed --quiet pulse-docker-prepare; then exit 1; fi\n if sudo -n journalctl -u pulse-docker-prepare --no-pager -o cat | grep -q '^PULSE_DOCKER_ENGINE_READY '; then sudo -n journalctl -u pulse-docker-prepare --no-pager -o cat | grep '^PULSE_DOCKER_ENGINE_READY '; exit 0; fi\n sleep 1\ndone\nexit 1\n"
	} else {
		s += "if sudo -n systemctl is-failed --quiet pulse-docker-prepare; then exit 1; fi\nsudo -n journalctl -u pulse-docker-prepare --no-pager -o cat | grep '^PULSE_DOCKER_ENGINE_READY ' || true\n"
	}
	// Replace the login shell: Ubuntu's .bash_logout can fail without a TTY.
	return "exec /bin/bash <<'PULSE_DOCKER_STARTUP'\n" + s + "PULSE_DOCKER_STARTUP\n", nil
}
func (p *Provider) newPair(g config.SlotGroup, slot string, template *SlotRecord) (*PairRecord, error) {
	b := p.base(g.Base)
	ep := p.Config.Engine(g.DockerProfile)
	pair := &PairRecord{ID: newID(), SlotID: slot, ConfigHash: p.dockerHash(g), Base: g.Base, Profile: g.DockerProfile, Phase: "reserved", Created: time.Now().UTC()}
	pair.Primary = p.sourceSpec(b, g.CPU, g.MemoryMiB)
	pair.Primary.Name = "pulse-" + p.Config.Scope()[:12] + "-" + pair.ID + "-primary"
	pair.Primary.StartupScript.Content = "#!/bin/bash\ntrue\n"
	if b.OS == "linux" {
		image := p.Store.data.Images[p.baseImageName(b.Name)]
		if image == nil || image.Phase != "ready" {
			return nil, nil
		}
		pair.Primary.Image = image.LocalName
	} else {
		if template == nil || template.Phase != "ready" {
			return nil, nil
		}
		engineImage := p.Store.data.Images[p.engineBaseName(ep)]
		if engineImage == nil || engineImage.Phase != "ready" {
			return nil, nil
		}
		pair.DockerRequested = true
		pair.Primary.Image = template.LocalName
		used := map[int]bool{}
		for _, v := range p.Store.data.Pairs {
			if v.Phase != "gone" {
				used[v.Port] = true
			}
		}
		for port := p.Config.DockerEndpoint.PortMin; port <= p.Config.DockerEndpoint.PortMax; port++ {
			if !used[port] {
				pair.Port = port
				break
			}
		}
		if pair.Port == 0 {
			return nil, nil
		}
		client, clientPub, e := keyPair()
		if e != nil {
			return nil, e
		}
		host, hostPub, e := keyPair()
		if e != nil {
			return nil, e
		}
		pair.Engine = p.sourceSpec(config.Base{OS: "linux", Image: engineImage.LocalName, SSHUsername: ep.SSHUsername, SSHPassword: ep.SSHPassword}, ep.CPU, ep.MemoryMiB)
		pair.Engine.Name = "pulse-" + p.Config.Scope()[:12] + "-" + pair.ID + "-engine"
		pair.Engine.StartupScript.Content, e = p.engineStartup(ep, host, clientPub)
		if e != nil {
			return nil, e
		}
		conf, _ := json.Marshal(map[string]string{"Address": net.JoinHostPort(p.Config.DockerEndpoint.Address, strconv.Itoa(pair.Port)), "User": "pulseengine", "KeyFile": "/var/lib/pulse-docker/client-key", "HostKeyFile": "/var/lib/pulse-docker/host-key.pub", "Socket": "/var/lib/pulse-docker/docker.sock"})
		pair.Primary.StartupScript.Content = "#!/bin/bash\nset -eu\nset +x\numask 077\nsudo -n install -d -m 700 /var/lib/pulse-docker\n" + fileScript("/var/lib/pulse-docker/client-key", client, false) + fileScript("/var/lib/pulse-docker/host-key.pub", hostPub, false) + fileScript("/var/lib/pulse-docker/relay.json", conf, false)
	}
	pair.PrimaryHash = p.Store.hash(normalized(pair.Primary))
	pair.EngineHash = p.Store.hash(normalized(pair.Engine))
	return pair, nil
}
func (p *Provider) pairMatches(pair *PairRecord, v backend.VM, primary bool) bool {
	spec, uid, hash := pair.Engine, pair.EngineUID, pair.EngineHash
	if primary {
		spec, uid, hash = pair.Primary, pair.PrimaryUID, pair.PrimaryHash
	}
	return v.Name == spec.Name && v.UID != "" && (uid == "" || v.UID == uid) && p.Store.hash(normalized(v.Spec)) == hash && (v.Worker == "" || v.Worker == p.Config.Worker)
}
func (p *Provider) pairVM(ctx context.Context, pair *PairRecord, primary bool) (backend.VM, bool, error) {
	name := pair.Engine.Name
	if primary {
		name = pair.Primary.Name
	}
	if name == "" {
		return backend.VM{}, false, nil
	}
	v, e := p.Backend.Get(ctx, name)
	if backend.IsStatus(e, 404) {
		p.observeError("Docker pair missing candidate", e)
		return v, false, nil
	}
	if e != nil {
		return v, false, backendError("Docker pair Get", e)
	}
	if !p.pairMatches(pair, v, primary) {
		pair.Phase = "quarantine"
		cause := fmt.Errorf("Docker allocation identity mismatch")
		if p.diagnostic == nil {
			p.pairPreparation(pair, primary, cause)
		}
		saveErr := p.Store.save()
		return v, false, combine("Docker pair identity", cause, "save Docker quarantine", saveErr)
	}
	p.pairPreparation(pair, primary, nil)
	if primary {
		pair.PrimaryUID = v.UID
	} else {
		pair.EngineUID = v.UID
	}
	return v, true, nil
}
func (p *Provider) pairEngineReady(ctx context.Context, pair *PairRecord) (bool, error) {
	name := pair.Engine.Name
	if name == "" {
		name = pair.Primary.Name
	}
	ev, e := p.Backend.(templateBackend).Events(ctx, name)
	if e != nil {
		return false, backendError("Docker engine events", e)
	}
	for _, v := range ev {
		for _, line := range strings.Split(v.Payload+"\n"+v.Message, "\n") {
			if strings.HasPrefix(line, "PULSE_DOCKER_ENGINE_READY ") {
				id := strings.TrimSpace(strings.TrimPrefix(line, "PULSE_DOCKER_ENGINE_READY "))
				if id != "" {
					pair.EngineID = id
					return true, nil
				}
			}
		}
	}
	return false, nil
}
func (p *Provider) configurePrimary(ctx context.Context, pair *PairRecord) error {
	b := p.base(pair.Base)
	linux := b.OS == "linux"
	script, e := p.asset("docker-primary.sh")
	if e != nil {
		return e
	}
	mode := "docker"
	if !pair.DockerRequested {
		mode = "plain"
	}
	content := "set -eu\nset +x\nsudo -n install -d -m 700 /var/lib/pulse-docker\n"
	if linux {
		setup, err := p.asset("setup-linux-nix.sh")
		if err != nil {
			return err
		}
		content += fileScript("/var/lib/pulse-docker/setup-linux-nix.sh", setup, true)
	}
	content += fileScript("/var/lib/pulse-docker/primary.sh", script, linux) + "sudo -n /bin/bash /var/lib/pulse-docker/primary.sh " + mode + "\n"
	_, e = p.Backend.(dockerBackend).Exec(ctx, pair.Primary.Name, pair.PrimaryUID, []byte(content))
	if e == nil && linux {
		pair.LinuxNixVersion = linuxNixVersion
	}
	return e
}

const linuxNixVersion = 1

func (p *Provider) needsLinuxNixSetup(pair *PairRecord) bool {
	return pair.LaunchID == "" && p.base(pair.Base).OS == "linux" && pair.LinuxNixVersion != linuxNixVersion
}
func (p *Provider) primaryReadyDetail(ctx context.Context, pair *PairRecord) error {
	b := p.base(pair.Base)
	g := p.dockerGroup(pair.SlotID)
	// Both the OS/resource facts and the exact engine identity must be observed.
	code := `import json,os,platform,subprocess
r=lambda *a:subprocess.check_output(a,text=True).strip()
assert r('/opt/pulse-tart/bin/c2j','version')
`
	if pair.DockerRequested {
		code += fmt.Sprintf("assert r('sudo','-n','-u','pulseworker','/opt/pulse-tart/bin/docker','--host','unix:///var/run/docker.sock','info','--format','{{.ID}}') == %s\n", strconv.Quote(pair.EngineID))
	} else {
		code += "assert not os.path.exists('/var/run/docker.sock')\nassert subprocess.call(['systemctl','is-active','--quiet','docker.service','docker.socket','containerd.service']) != 0\n"
	}
	if b.Architecture() == "amd64" {
		code += "assert platform.machine() == 'x86_64'\n"
	} else {
		code += "assert platform.machine() in ('arm64','aarch64')\n"
	}
	if b.OS == "linux" {
		code += fmt.Sprintf("assert platform.system() == 'Linux'\nassert os.cpu_count() >= %d\nassert int(next(l.split()[1] for l in open('/proc/meminfo') if l.startswith('MemTotal:')))*1024 >= %d\nassert 'VERSION_ID=\"%s\"' in open('/etc/os-release').read()\n", g.CPU, (g.MemoryMiB-512)*(1<<20), b.OSVersion)
		// A root version check cannot establish that the job user can reach the
		// daemon. Ping returns no stdout on success; subprocess failure is enough.
		code += "r('sudo','-n','-H','-u','pulseworker','/usr/bin/nix','--extra-experimental-features','nix-command','store','ping','--store','daemon')\n"
	} else {
		code += fmt.Sprintf("assert r('/usr/bin/sw_vers','-productVersion') == '%s'\nassert int(r('/usr/sbin/sysctl','-n','hw.ncpu')) == %d\nassert int(r('/usr/sbin/sysctl','-n','hw.memsize')) == %d\nsaved=json.load(open('/var/db/pulse-tart-template/ready'))\nassert r('/usr/sbin/sysctl','-n','kern.bootsessionuuid') == saved['boot_id']\nassert saved['mac'] in r('/sbin/ifconfig','en0')\n", b.OSVersion, g.CPU, g.MemoryMiB*(1<<20))
	}
	for _, cap := range b.Capabilities {
		if cap == "kvm" {
			// Test the actual job user's access and the KVM API, not merely a
			// device-node name or a privileged root open. No guest is launched.
			code += "assert r('sudo','-n','-H','-u','pulseworker','/opt/pulse-tart/bin/python3','-c'," + strconv.Quote(kvmReadinessProbe) + ") == 'PULSE_KVM_READY'\n"
			continue
		}
		tool := cap
		if tool == "python" {
			tool = "python3"
		}
		path := "/opt/pulse-tart/bin/" + tool
		if tool == "nix" {
			path = "/nix/var/nix/profiles/default/bin/nix"
			if b.OS == "linux" {
				path = "/usr/bin/nix"
			}
		}
		code += "assert r(" + strconv.Quote(path) + ",'--version')\n"
	}
	code += "print('PULSE_DOCKER_PRIMARY_READY')\n"
	out, e := p.Backend.(dockerBackend).Exec(ctx, pair.Primary.Name, pair.PrimaryUID, []byte("set -eu\ncd /tmp\nsudo -n /opt/pulse-tart/bin/python3 - <<'PULSE_CHECK'\n"+code+"PULSE_CHECK\n"))
	if e != nil {
		return backendError("Docker primary readiness Exec", e)
	}
	if !strings.Contains(string(out), "PULSE_DOCKER_PRIMARY_READY") {
		return errors.New("Docker primary readiness missing PULSE_DOCKER_PRIMARY_READY marker")
	}
	return nil
}

const kvmReadinessProbe = `import fcntl,os
kvm=os.open('/dev/kvm',os.O_RDWR|os.O_CLOEXEC)
try:
    assert fcntl.ioctl(kvm,0xae00,0) == 12, 'unsupported KVM API'
    vm=fcntl.ioctl(kvm,0xae01,0)
    os.close(vm)
finally:
    os.close(kvm)
print('PULSE_KVM_READY')
`

func (p *Provider) reclaimPair(ctx context.Context, pair *PairRecord) error {
	b := p.Backend.(dockerBackend)
	// In properties mode the last VM deletion also removes the recovery data.
	// Stop the host helper first so a crash cannot leave it without an owner.
	if p.Store.properties != nil && pair.EngineUID != "" {
		if endpoint, ok := p.Backend.(interface {
			RemoveEndpoint(context.Context, string, string) error
		}); ok {
			if err := endpoint.RemoveEndpoint(ctx, pair.Engine.Name, pair.EngineUID); err != nil {
				return err
			}
		}
	}
	uncertainMissing := false
	// No new allocation can reuse this slot or port until BOTH deletions confirm.
	for _, primary := range []bool{true, false} {
		spec, uid := pair.Engine, pair.EngineUID
		if primary {
			spec, uid = pair.Primary, pair.PrimaryUID
		}
		if spec.Name == "" {
			continue
		}
		v, exists, e := p.pairVM(ctx, pair, primary)
		if e != nil {
			return e
		}
		if !exists {
			if uid == "" { // A dispatched create with no response remains uncertain.
				dispatched := pair.PrimaryDispatched
				if !primary {
					dispatched = pair.EngineDispatched
				}
				if dispatched && p.Store.properties == nil {
					uncertainMissing = true
				}
			}
			continue
		}
		if !v.Stopped() {
			return b.Stop(ctx, v.Name, v.UID)
		}
		if e = p.Backend.Delete(ctx, v.Name); e != nil && !backend.IsStatus(e, 404) {
			if errors.Is(e, backend.ErrBusy) {
				return nil // Keep the slot and companion port reserved until deletion.
			}
			return backendError(fmt.Sprintf("Docker delete allocation VM %s launch=%s", v.Name, pair.LaunchID), e)
		}
		if _, exists, e = p.pairVM(ctx, pair, primary); e != nil {
			return e
		} else if exists {
			return nil
		}
	}
	if p.Store.properties == nil && pair.EngineUID != "" {
		if b, ok := p.Backend.(interface {
			RemoveEndpoint(context.Context, string, string) error
		}); ok {
			if e := b.RemoveEndpoint(ctx, pair.Engine.Name, pair.EngineUID); e != nil {
				return e
			}
		}
	}
	if uncertainMissing {
		return p.Store.save()
	}
	pair.Phase = "gone"
	if r := p.Store.data.Records[pair.LaunchID]; r != nil {
		r.Phase = "done"
		r.diagnostic = ""
	}
	// Release stored transport credentials only after all native resources are gone.
	pair.Primary.StartupScript.Content = ""
	pair.Engine.StartupScript.Content = ""
	return p.Store.save()
}
func (p *Provider) reconcilePairs(ctx context.Context) error {
	if p.Config.Docker == nil && len(p.Store.data.Pairs) == 0 {
		return nil
	}
	b, ok := p.Backend.(dockerBackend)
	if !ok {
		return fmt.Errorf("backend does not support Docker allocations")
	}
	ids := []string{}
	for id := range p.Store.data.Pairs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	// Idle macOS pairs must not occupy the physical VM positions needed to
	// prepare another configured OS version. Accepted jobs keep their VMs.
	templatesPending := p.macTemplatesPending()
	releaseMacPosition := false
	if templatesPending {
		used, err := p.vmReservations(ctx)
		if err != nil {
			return err
		}
		preparing := false
		for name, os := range used {
			if os == "linux" {
				continue
			}
			if image := p.Store.data.Images[name]; image != nil && image.Phase != "ready" {
				preparing = true
			}
			for _, slot := range p.Store.data.Slots {
				if slot.Name == name && slot.Phase != "ready" {
					preparing = true
				}
			}
		}
		releaseMacPosition = !preparing && macOSCount(used) >= 2
		for _, pair := range p.Store.data.Pairs {
			if pair.Primary.OS == "darwin" && pair.Phase == "reclaiming" {
				releaseMacPosition = false
			}
		}
	}
	// Lower limits retire idle workers before accepting more work. Active jobs
	// keep their reservations until cleanup, even if the new limit is smaller.
	if !p.fitsJobCounts(p.jobReservations()) {
		for _, id := range ids {
			if p.Store.data.Pairs[id].Phase == "reclaiming" {
				if e := p.reclaimPair(ctx, p.Store.data.Pairs[id]); e != nil {
					return e
				}
				return nil
			}
		}
		for _, id := range ids {
			pair := p.Store.data.Pairs[id]
			if pair.LaunchID == "" && pair.Phase != "gone" && pair.Phase != "quarantine" {
				pair.Phase = "reclaiming"
				return p.Store.save()
			}
		}
	}
	for _, id := range ids {
		pair := p.Store.data.Pairs[id]
		if pair.Phase == "gone" || pair.Phase == "quarantine" {
			continue
		}
		if pair.Phase == "reclaiming" {
			if e := p.reclaimPair(ctx, pair); e != nil {
				return e
			}
			continue
		}
		g := p.dockerGroup(pair.SlotID)
		// Configuration changes drain unused warm capacity; never kill an active job.
		if pair.LaunchID == "" && (g.Count == 0 || pair.ConfigHash != p.dockerHash(g) || releaseMacPosition && pair.Primary.OS == "darwin" || time.Since(pair.Created) > p.dockerReadyTimeout() && pair.Phase != "ready") {
			if pair.Primary.OS == "darwin" {
				releaseMacPosition = false
			}
			pair.Phase = "reclaiming"
			if e := p.Store.save(); e != nil {
				return e
			}
			continue
		}
		if pair.Phase == "reserved" {
			available, e := p.canStartVMs(ctx)
			if e != nil {
				return e
			}
			if !available {
				continue
			}
			primary := pair.Engine.Name == ""
			spec := pair.Engine
			if primary {
				spec = pair.Primary
				pair.PrimaryDispatched = true
				pair.Phase = "primary-dispatch"
			} else {
				pair.EngineDispatched = true
				pair.Phase = "engine-dispatch"
			}
			if e := p.Store.save(); e != nil {
				return e
			}
			v, e := p.Backend.Create(ctx, spec)
			if e == nil && p.pairMatches(pair, v, primary) {
				if primary {
					pair.PrimaryUID = v.UID
				} else {
					pair.EngineUID = v.UID
				}
			}
			cause := e
			if cause == nil && ((primary && pair.PrimaryUID == "") || (!primary && pair.EngineUID == "")) {
				cause = errors.New("Docker preparation create identity mismatch")
			}
			p.pairPreparation(pair, primary, cause)
			if saveErr := p.Store.save(); saveErr != nil {
				return combine("Docker preparation create", cause, "save Docker preparation result", saveErr)
			}
			continue
		}
		primary, hasPrimary, e := p.pairVM(ctx, pair, true)
		if e != nil {
			return e
		}
		if pair.PrimaryDispatched && ((!hasPrimary && (pair.PrimaryUID != "" || p.Store.properties != nil)) || (hasPrimary && primary.Stopped() && (primary.Status == "failed" || primary.PowerState != "suspended" && (pair.Phase == "ready" || pair.Phase == "running" || pair.Phase == "handoff-intent")))) {
			pair.Phase = "reclaiming"
			if e = p.Store.save(); e != nil {
				return e
			}
			continue
		}
		if pair.Engine.Name != "" {
			engine, exists, e := p.pairVM(ctx, pair, false)
			if e != nil {
				return e
			}
			if !exists || (engine.Stopped() && engine.Status == "failed") {
				if pair.LaunchID == "" && (pair.EngineUID != "" || p.Store.properties != nil) {
					pair.Phase = "reclaiming"
					if e = p.Store.save(); e != nil {
						return e
					}
				}
				continue
			}
			if pair.Phase == "engine-dispatch" {
				// Both VMs were reserved together. Restore the primary while the
				// engine boots; only configuration/readiness needs the endpoint.
				if !pair.PrimaryDispatched {
					available, err := p.canStartVMs(ctx)
					if err != nil {
						return err
					}
					if !available {
						continue
					}
					pair.PrimaryDispatched = true
					if err := p.Store.save(); err != nil {
						return err
					}
					v, err := p.Backend.Create(ctx, pair.Primary)
					if err == nil && p.pairMatches(pair, v, true) {
						pair.PrimaryUID = v.UID
					}
					cause := err
					if cause == nil && pair.PrimaryUID == "" {
						cause = errors.New("Docker primary create identity mismatch")
					}
					p.pairPreparation(pair, true, cause)
					if saveErr := p.Store.save(); saveErr != nil {
						return combine("Docker primary create", cause, "save Docker preparation result", saveErr)
					}
					continue
				}
				// An ambiguous primary Create must first resolve to the owned VM.
				// Keep its phase and diagnostic intact while waiting for that result.
				if !hasPrimary {
					continue
				}
				ready, e := p.pairEngineReady(ctx, pair)
				if e != nil {
					return e
				}
				if !ready {
					continue
				}
				ready, e = b.Endpoint(ctx, pair.Engine.Name, pair.EngineUID, pair.Port)
				if e != nil {
					return e
				}
				if !ready {
					continue
				}
				pair.Phase = "primary-dispatch"
				if e = p.Store.save(); e != nil {
					return e
				}
				continue
			}
			// Ensure the same endpoint survives worker/helper restart, including after handoff.
			if _, e = b.Endpoint(ctx, pair.Engine.Name, pair.EngineUID, pair.Port); e != nil {
				return e
			}
		}
		if !hasPrimary {
			continue
		}
		if pair.Phase == "ready" && p.needsLinuxNixSetup(pair) {
			// Repair reused idle allocations in place, without invalidating bases,
			// changing VM identity, or disturbing an accepted launch.
			pair.Phase = "configuring"
			if e = p.Store.save(); e != nil {
				return e
			}
		}
		var observationCause error
		switch pair.Phase {
		case "primary-dispatch", "configuring", "docker-starting":
			if pair.Engine.Name == "" && pair.DockerRequested {
				script, err := p.engineScript(p.Config.Engine(pair.Profile), nil, nil, false)
				if err != nil {
					return err
				}
				out, err := b.Exec(ctx, pair.Primary.Name, pair.PrimaryUID, []byte(script))
				if err != nil {
					p.preparation("pair:"+pair.ID, backendError("start Linux Docker", err))
					continue
				}
				pair.EngineID = engineIdentity(string(out))
				if pair.EngineID == "" {
					continue
				}
			}
			pair.Phase = "configuring"
			if e = p.Store.save(); e != nil {
				return e
			}
			if e = p.configurePrimary(ctx, pair); e != nil {
				p.preparation("pair:"+pair.ID, backendError("configure Docker primary", e))
				continue
			}
			if e := p.primaryReadyDetail(ctx, pair); e != nil {
				p.preparation("pair:"+pair.ID, e)
				continue
			}
			p.preparation("pair:"+pair.ID, nil)
			pair.Phase = "ready"
		case "handoff-intent":
			// Observe the exact durable receipt. Never resubmit the envelope after ambiguity.
			script := "set -eu\n" + guestStateScript + "test \"$(sudo -n cat $state/fingerprint)\" = " + shell(pair.DeliveryHash) + "\nsudo -n /bin/bash /opt/pulse-tart-image/bootstrap-guest.sh\necho PULSE_HANDOFF_OK\n"
			out, e := b.Exec(ctx, pair.Primary.Name, pair.PrimaryUID, []byte(script))
			if e != nil {
				observationCause = backendError("observe Docker handoff receipt Exec", e)
			} else if !strings.Contains(string(out), "PULSE_HANDOFF_OK") {
				observationCause = errors.New("Docker handoff receipt missing PULSE_HANDOFF_OK marker")
			}
			p.preparation("pair:"+pair.ID, observationCause)
			if e == nil && strings.Contains(string(out), "PULSE_HANDOFF_OK") {
				pair.Phase = "running"
				r := p.Store.data.Records[pair.LaunchID]
				r.Outcome = api.Accepted
				r.diagnostic = ""
				r.Phase = "active"
			}
		}
		if saveErr := p.Store.save(); saveErr != nil {
			return combine("Docker handoff receipt", observationCause, "save Docker reconciliation result", saveErr)
		}
	}
	if p.Config.Docker == nil {
		return nil
	}
	// Only Linux primaries warm automatically, with Docker stopped. macOS
	// Docker pairs are reserved by an explicit capability request.
	for _, g := range p.Config.Slots {
		if p.preferredGroup != "" && time.Now().Before(p.preferUntil) && g.Name != p.preferredGroup {
			continue
		}
		if g.DockerProfile == "" || p.base(g.Base).OS != "linux" {
			continue
		}
		for i := 1; i <= g.Count; i++ {
			slot := fmt.Sprintf("%s-%04d", g.Name, i)
			if p.slotBusy(slot) {
				continue
			}
			pair, e := p.newPair(g, slot, p.Store.data.Slots[slot])
			if e != nil {
				return e
			}
			if pair == nil {
				continue
			}
			available, e := p.canReserveJobs(ctx, pair.Primary)
			if e != nil {
				return e
			}
			if !available {
				continue
			}
			p.Store.data.Pairs[pair.ID] = pair
			if e := p.Store.save(); e != nil {
				return e
			}
		}
	}
	return nil
}

func (p *Provider) macTemplatesPending() bool {
	for _, g := range p.Config.Slots {
		if p.base(g.Base).OS == "linux" {
			continue
		}
		for i := 1; i <= g.Count; i++ {
			s := p.Store.data.Slots[fmt.Sprintf("%s-%04d", g.Name, i)]
			if s == nil || s.Phase != "ready" {
				return true
			}
		}
	}
	return false
}
func (p *Provider) submitDocker(ctx context.Context, l api.Launch, hash string) (api.Submission, bool) {
	wants := slices.Contains(l.Capabilities, "docker")
	compatible := false
	compatibleGroups := []config.SlotGroup{}
	ordinary := false
	for _, g := range p.Config.Slots {
		if p.dockerMatch(g, l) {
			compatible = true
			compatibleGroups = append(compatibleGroups, g)
		}
		if (g.DockerProfile == "" || p.base(g.Base).OS != "linux") && baseMatches(p.base(g.Base), l) && g.CPU*1000 >= l.CPUMillis && g.MemoryMiB*(1<<20) >= l.MemoryBytes {
			ordinary = true
		}
	}
	if !wants && (!compatible || ordinary) {
		return api.Submission{}, false
	}
	if !compatible {
		p.observe("no Docker group passed configured request fit")
		return p.submission(l.LaunchID, api.Unsupported), true
	}
	if condition := unsupportedSemantics(l); condition != "" {
		p.observe("%s", condition)
		return p.submission(l.LaunchID, api.Unsupported), true
	}
	if !p.fitsJobCounts(p.jobReservations()) {
		return p.submission(l.LaunchID, api.NoCapacity), true
	}
	// Preserve configured base preference; stable smallest fitting slot within it.
	candidates := []*PairRecord{}
	for _, pair := range p.Store.data.Pairs {
		g := p.dockerGroup(pair.SlotID)
		if pair.Phase != "ready" {
			p.observe("Docker slot %s recorded readiness phase=%s; occupancy unknown", pair.SlotID, pair.Phase)
			p.earlierPreparation("pair:" + pair.ID)
			continue
		}
		if pair.LaunchID != "" {
			p.observe("Docker slot %s reservation occupied", pair.SlotID)
			continue
		}
		if g.Count <= 0 {
			p.observe("Docker slot %s configured count disabled", pair.SlotID)
			continue
		}
		if pair.ConfigHash != p.dockerHash(g) {
			p.observe("Docker slot %s stale configuration", pair.SlotID)
			continue
		}
		if p.dockerMatch(g, l) && pair.DockerRequested == wants {
			candidates = append(candidates, pair)
		} else {
			p.observe("Docker slot %s configured request fit failed", pair.SlotID)
		}
	}
	pref := map[string]int{}
	for i, b := range p.Config.Bases {
		pref[b.Name] = i
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if pref[a.Base] != pref[b.Base] {
			return pref[a.Base] < pref[b.Base]
		}
		if a.Primary.CPU != b.Primary.CPU {
			return a.Primary.CPU < b.Primary.CPU
		}
		if a.Primary.Memory != b.Primary.Memory {
			return a.Primary.Memory < b.Primary.Memory
		}
		return a.SlotID < b.SlotID
	})
	for _, pair := range candidates {
		v, exists, e := p.pairVM(ctx, pair, true)
		if e != nil {
			p.observeError("Docker primary observation", e)
			return p.submission(l.LaunchID, api.Unavailable), true
		}
		if !exists || v.Stopped() {
			p.observe("Docker primary missing or stopped")
			continue
		}
		if pair.Engine.Name != "" {
			v, exists, e = p.pairVM(ctx, pair, false)
			if e != nil {
				p.observeError("Docker engine observation", e)
				return p.submission(l.LaunchID, api.Unavailable), true
			}
			if !exists || v.Stopped() {
				p.observe("Docker engine missing or stopped")
				continue
			}
		}
		if p.needsLinuxNixSetup(pair) {
			if e := p.configurePrimary(ctx, pair); e != nil {
				p.observeError("configure idle Linux Nix", e)
				return p.submission(l.LaunchID, api.Unavailable), true
			}
			if e := p.Store.save(); e != nil {
				p.observeError("save idle Linux Nix setup", e)
				return p.submission(l.LaunchID, api.Unavailable), true
			}
		}
		if e := p.primaryReadyDetail(ctx, pair); e != nil {
			p.observeError("Docker primary readiness", e)
			return p.submission(l.LaunchID, api.Unavailable), true
		}
		b := p.base(pair.Base)
		if wants {
			b.Capabilities = append(slices.Clone(b.Capabilities), "docker")
		}
		proc := p.workerProcess(l, b, pair.Primary.CPU, pair.Primary.Memory-512)
		if wants {
			proc.Env["DOCKER_HOST"] = "unix:///var/run/docker.sock"
			proc.Env["TESTCONTAINERS_HOST_OVERRIDE"] = "127.0.0.1"
			proc.Env["TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE"] = "/var/run/docker.sock"
		}
		proc.Env["TMPDIR"] = proc.WorkingDir + "/work/tmp"
		payload, _ := json.Marshal(envelope{1, l.LaunchID, proc})
		sum := sha256.Sum256(payload)
		pair.DeliveryHash = hex.EncodeToString(sum[:])
		pair.LaunchID = l.LaunchID
		pair.Phase = "handoff-intent"
		r := &Record{LaunchID: l.LaunchID, Fingerprint: hash, Name: pair.Primary.Name, UID: pair.PrimaryUID, NativeHash: pair.PrimaryHash, Job: l.Job, Created: time.Now().UTC(), Outcome: api.Unknown, Phase: "dispatch", SlotID: pair.SlotID, PairID: pair.ID, Base: pair.Base}
		p.Store.data.Records[l.LaunchID] = r
		p.preferredGroup = ""
		if e := p.Store.save(); e != nil {
			p.observeError("save Docker handoff intent", e)
			r.diagnostic = p.diagnosticText()
			return p.submission(l.LaunchID, api.Unknown), true
		}
		decode := "-D"
		if b.OS == "linux" {
			decode = "-d"
		}
		script := "set -eu\n" + guestStateScript + fmt.Sprintf("set +x\numask 077\nprintf '%%s' %s | base64 %s | sudo -n /opt/pulse-tart/bin/pulse-tart-guest ingest\nsudo -n /bin/bash /opt/pulse-tart-image/bootstrap-guest.sh\necho PULSE_HANDOFF_OK\n", shell(base64.StdEncoding.EncodeToString(payload)), decode)
		out, e := p.Backend.(dockerBackend).Exec(ctx, pair.Primary.Name, pair.PrimaryUID, []byte(script))
		if e == nil && strings.Contains(string(out), "PULSE_HANDOFF_OK") {
			r.Outcome = api.Accepted
			r.Phase = "active"
			pair.Phase = "running"
		}
		if e != nil {
			p.observeError("Docker handoff Exec", e)
		} else if r.Outcome != api.Accepted {
			p.observe("Docker handoff missing PULSE_HANDOFF_OK marker")
		} else {
			p.observe("Docker handoff observed PULSE_HANDOFF_OK; result durability not yet confirmed")
		}
		if e := p.Store.save(); e != nil {
			p.observeError("save Docker handoff result", e)
			r.diagnostic = p.diagnosticText()
			return p.submission(l.LaunchID, api.Unknown), true
		}
		if r.Outcome != api.Accepted {
			r.diagnostic = p.diagnosticText()
		}
		return p.submission(l.LaunchID, r.Outcome), true
	}
	// No pair was admitted. Cite only retained preparation evidence for missing
	// pairs in groups whose configured request fit was already checked above.
	// This does not probe capacity or infer reservation occupancy/readiness.
	prepared := map[string]bool{}
	for _, pair := range p.Store.data.Pairs {
		if pair.Phase != "gone" {
			prepared[pair.SlotID] = true
		}
	}
	for _, g := range compatibleGroups {
		for i := 1; i <= g.Count; i++ {
			slot := fmt.Sprintf("%s-%04d", g.Name, i)
			if prepared[slot] {
				continue
			}
			p.observe("Docker slot %s readiness unknown: no prepared pair; occupancy unknown", slot)
			image := p.Store.data.Images[p.baseImageName(g.Base)]
			if image == nil || image.Phase != "ready" {
				p.earlierPreparation("image:" + p.baseImageName(g.Base))
			} else if p.base(g.Base).OS != "linux" {
				p.earlierPreparation("slot:" + slot)
			}
		}
	}
	if err := p.requestJobRoom(ctx, l); err != nil {
		p.observeError("Docker room request", err)
		return p.submission(l.LaunchID, api.Unavailable), true
	}
	if wants {
		if err := p.requestDocker(ctx, l); err != nil {
			p.observeError("prepare requested Docker capacity", err)
			return p.submission(l.LaunchID, api.Unavailable), true
		}
	}
	return p.submission(l.LaunchID, api.NoCapacity), true
}

// PrepareDocker explicitly requests unassigned Docker capacity without a job
// lease. Internal qualification uses this to inspect engines before handoff;
// normal runtime admission requests one allocation through Submit instead.
// It does not change the default reconcile/setup behavior.
func (p *Provider) PrepareDocker(ctx context.Context, allocation api.Allocation, count int) error {
	p.Store.mu.Lock()
	defer p.Store.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if p.Store.broken {
		return p.Store.stateError()
	}
	if count < 1 || count > p.Config.Capacity() || !slices.Contains(allocation.Capabilities, "docker") {
		return errors.New("invalid Docker capacity request")
	}
	return p.prepareDocker(ctx, api.Launch{Allocation: allocation}, count)
}

// No lease is retained while preparing requested capability. The controller
// retries admission after reconciliation has observed readiness.
func (p *Provider) requestDocker(ctx context.Context, l api.Launch) error {
	return p.prepareDocker(ctx, l, 1)
}

func (p *Provider) prepareDocker(ctx context.Context, l api.Launch, count int) error {
	for _, pair := range p.Store.data.Pairs {
		if pair.LaunchID == "" && pair.DockerRequested && (l.LaunchID == "" || pair.RequestID == l.LaunchID) && pair.Phase != "gone" && pair.Phase != "reclaiming" && pair.Phase != "quarantine" && pair.ConfigHash == p.dockerHash(p.dockerGroup(pair.SlotID)) && p.dockerMatch(p.dockerGroup(pair.SlotID), l) {
			count--
		}
	}
	if count <= 0 {
		return nil
	}
	for _, pair := range p.Store.data.Pairs {
		if pair.Phase == "ready" && pair.LaunchID == "" && !pair.DockerRequested && pair.ConfigHash == p.dockerHash(p.dockerGroup(pair.SlotID)) && p.dockerMatch(p.dockerGroup(pair.SlotID), l) {
			pair.DockerRequested, pair.Phase, pair.Created = true, "docker-starting", time.Now().UTC()
			pair.RequestID = l.LaunchID
			if err := p.Store.save(); err != nil {
				return err
			}
			count--
			if count == 0 {
				return nil
			}
		}
	}
	for _, g := range p.Config.Slots {
		if p.base(g.Base).OS == "linux" || !p.dockerMatch(g, l) {
			continue
		}
		for i := 1; i <= g.Count; i++ {
			created, err := p.reserveDockerPair(ctx, g, fmt.Sprintf("%s-%04d", g.Name, i), l.LaunchID)
			if err != nil {
				return err
			}
			if created {
				count--
			}
			if count == 0 {
				return nil
			}
		}
	}
	return nil
}

func (p *Provider) reserveDockerPair(ctx context.Context, g config.SlotGroup, slot, requestID string) (bool, error) {
	if p.slotBusy(slot) {
		return false, nil
	}
	s := p.Store.data.Slots[slot]
	if s == nil || s.Phase != "ready" {
		return false, nil
	}
	v, err := p.Backend.Get(ctx, s.Name)
	if err != nil {
		return false, backendError("Docker template Get", err)
	}
	if !p.matchesSlot(s, v) || !suspended(v) {
		return false, backendError("Docker template identity or suspension mismatch", nil)
	}
	pair, err := p.newPair(g, slot, s)
	if err != nil || pair == nil {
		return false, err
	}
	available, err := p.canReserveJobs(ctx, pair.Primary)
	if err != nil || !available {
		return false, err
	}
	pair.RequestID = requestID
	p.Store.data.Pairs[pair.ID] = pair
	return true, p.Store.save()
}

func engineIdentity(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if id, ok := strings.CutPrefix(line, "PULSE_DOCKER_ENGINE_READY "); ok {
			return strings.TrimSpace(id)
		}
	}
	return ""
}

func (p *Provider) dockerReadyTimeout() time.Duration {
	if p.Config.Docker != nil {
		return p.Config.Docker.ReadyTimeout
	}
	return p.Config.BootstrapTimeout
}

func (p *Provider) pairInstances(vs []backend.VM, q api.ListRequest, after string) ([]api.Instance, error) {
	byName := map[string]backend.VM{}
	for _, v := range vs {
		byName[v.Name] = v
	}
	out := []api.Instance{}
	for _, pair := range p.Store.data.Pairs {
		if pair.Phase == "gone" || pair.LaunchID == "" {
			continue
		}
		r := p.Store.data.Records[pair.LaunchID]
		if r == nil {
			return nil, stateError("Docker allocation references missing launch record", nil)
		}
		if q.LaunchID != "" && q.LaunchID != r.LaunchID {
			continue
		}
		for _, primary := range []bool{true, false} {
			name := pair.Engine.Name
			role := "engine"
			if primary {
				name = pair.Primary.Name
				role = "primary"
			}
			if name == "" || name <= after {
				continue
			}
			v, ok := byName[name]
			if !ok {
				continue
			}
			if !p.pairMatches(pair, v, primary) {
				return nil, fmt.Errorf("Docker resource identity mismatch")
			}
			state := "starting"
			if pair.Phase == "running" {
				state = "running"
			}
			if pair.Phase == "reclaiming" || v.Stopped() {
				state = "stopping"
			}
			created := r.Created
			out = append(out, api.Instance{ID: name, LaunchID: r.LaunchID, Job: r.Job, State: state, CreatedAt: &created, Refs: []string{p.Config.Kind + ":" + name, "role:" + role}})
		}
	}
	return out, nil
}

// DockerStatus excludes credentials, guest payloads and journal fingerprints.
type DockerStatus struct{ ID, Slot, Phase, Primary, PrimaryUID, Engine, EngineUID, LaunchID string }

func (p *Provider) DockerAllocations() ([]DockerStatus, error) {
	p.Store.mu.Lock()
	defer p.Store.mu.Unlock()
	if p.Store.broken {
		return nil, p.Store.stateError()
	}
	if err := p.Store.refresh(); err != nil {
		return nil, err
	}
	out := []DockerStatus{}
	for _, pair := range p.Store.data.Pairs {
		if pair.Phase != "gone" {
			out = append(out, DockerStatus{pair.ID, pair.SlotID, pair.Phase, pair.Primary.Name, pair.PrimaryUID, pair.Engine.Name, pair.EngineUID, pair.LaunchID})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slot < out[j].Slot })
	return out, nil
}

func (p *Provider) primaryReady(ctx context.Context, pair *PairRecord) bool {
	return p.primaryReadyDetail(ctx, pair) == nil
}

const guestStateScript = `version=$(sudo -n /opt/pulse-tart/bin/pulse-tart-guest version)
case "$version" in
 'pulse-tart-guest 0.1.0') state=/var/db/pulse-tart; sudo -n test ! -e /var/db/pulse-macvz ;;
 'pulse-macvz-guest 0.2.0') state=/var/db/pulse-macvz; sudo -n test ! -e /var/db/pulse-tart ;;
 *) exit 1 ;;
esac
`
