package provider

import (
	api "github.com/colony-2/pulse-provider-api"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func warmPlain(t *testing.T, p *Provider) {
	t.Helper()
	for range 40 {
		if err := p.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMacDockerStartsOnlyForRequestedCapability(t *testing.T) {
	p, f := dockerFixture(t, false)
	warmPlain(t, p)
	if len(p.Store.data.Pairs) != 0 {
		t.Fatal("setup created an unrequested companion")
	}
	l := launch("plain-mac")
	out, err := p.Submit(t.Context(), []api.Launch{l})
	if err != nil || out[0].Status != api.Accepted {
		t.Fatal("Docker-eligible slot rejected ordinary launch", out, err)
	}
	if len(p.Store.data.Pairs) != 0 {
		t.Fatal("ordinary job created a companion")
	}
	for _, vm := range f.vms {
		if strings.HasSuffix(vm.Name, "-engine") {
			t.Fatal("unrequested engine", vm.Name)
		}
	}
}

func TestMacDockerDemandAndRetryAdmission(t *testing.T) {
	p, _ := dockerFixture(t, false)
	warmPlain(t, p)
	l := dockerLaunch()
	got, err := p.Submit(t.Context(), []api.Launch{l})
	if err != nil || got[0].Status != api.NoCapacity || len(p.Store.data.Records) != 0 {
		t.Fatal("admitted before requested companion ready", got, err)
	}
	if len(p.Store.data.Pairs) != 1 {
		t.Fatal("request did not reserve a pair")
	}
	for range 30 {
		if err := p.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		got, err = p.Submit(t.Context(), []api.Launch{l})
		if err != nil {
			t.Fatal(err)
		}
		if got[0].Status == api.Accepted {
			return
		}
	}
	t.Fatal("requested Docker never became available", got)
}

func TestLinuxPlainWorkerDoesNotActivateDocker(t *testing.T) {
	p, f := dockerFixture(t, true)
	warmPlain(t, p)
	for _, pair := range p.Store.data.Pairs {
		if pair.Phase != "ready" || pair.DockerRequested || strings.Contains(pair.Primary.StartupScript.Content, "pulse-docker-prepare") {
			t.Fatal("warm worker started Docker")
		}
	}
	if f.engineStarts != 0 {
		t.Fatal("Linux warmup started Docker")
	}
	l := launch("plain-linux")
	got, err := p.Submit(t.Context(), []api.Launch{l})
	if err != nil || got[0].Status != api.Accepted {
		t.Fatal(got, err)
	}
	if f.engineStarts != 0 {
		t.Fatal("ordinary admission started Docker")
	}
	for _, key := range []string{"DOCKER_HOST", "TESTCONTAINERS_HOST_OVERRIDE", "TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE"} {
		if _, ok := f.payload.Process.Env[key]; ok {
			t.Fatal("ordinary job received Docker environment", key)
		}
	}
	for _, pair := range p.Store.data.Pairs {
		if pair.DockerRequested {
			t.Fatal("ordinary launch activated Docker")
		}
	}
}

func TestLinuxDockerActivationSurvivesReopen(t *testing.T) {
	p, f := dockerFixture(t, true)
	warmPlain(t, p)
	got, err := p.Submit(t.Context(), []api.Launch{dockerLaunch()})
	if err != nil || got[0].Status != api.NoCapacity || f.deliveries != 0 {
		t.Fatal(got, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(p.Config.StateDir, p.Config.Scope())
	if err != nil {
		t.Fatal(err)
	}
	p.Store = store
	for _, pair := range p.Store.data.Pairs {
		if pair.Phase != "docker-starting" || !pair.DockerRequested || pair.LaunchID != "" {
			t.Fatal("lost durable activation intent")
		}
	}
	f.engineBlocked = true
	for range 2 {
		if err := p.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		out, err := p.Submit(t.Context(), []api.Launch{dockerLaunch()})
		if err != nil || out[0].Status != api.NoCapacity || f.deliveries != 0 {
			t.Fatal("admitted before engine readiness", out, err)
		}
	}
	f.engineBlocked = false
	for range 10 {
		if err := p.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		got, err = p.Submit(t.Context(), []api.Launch{dockerLaunch()})
		if err != nil {
			t.Fatal(err)
		}
		if got[0].Status == api.Accepted {
			if f.payload.Process.Env["DOCKER_HOST"] != "unix:///var/run/docker.sock" || f.deliveries != 1 {
				t.Fatal("incorrect Docker handoff")
			}
			return
		}
	}
	t.Fatal("activation did not recover", got)
}

func TestOrdinaryRequestReclaimsUnusedDockerCapacity(t *testing.T) {
	for _, linux := range []bool{false, true} {
		t.Run(map[bool]string{false: "macOS", true: "Linux"}[linux], func(t *testing.T) {
			p, _ := dockerFixture(t, linux)
			pair := readyDocker(t, p)
			l := launch("ordinary-after-docker-demand")
			if !linux {
				l.MemoryBytes = 8 << 30
			} // Ordinary macOS jobs retain full memory.
			got, err := p.Submit(t.Context(), []api.Launch{l})
			if err != nil || got[0].Status != api.NoCapacity || pair.Phase != "reclaiming" {
				t.Fatal("idle Docker capacity did not yield", got, err, pair.Phase)
			}
			for range 20 {
				if err := p.Reconcile(t.Context()); err != nil {
					t.Fatal(err)
				}
				got, err = p.Submit(t.Context(), []api.Launch{l})
				if err != nil {
					t.Fatal(err)
				}
				if got[0].Status == api.Accepted {
					return
				}
			}
			t.Fatal("ordinary request never admitted", got)
		})
	}
}

func TestDistinctDockerRequestsPrepareConcurrently(t *testing.T) {
	for _, linux := range []bool{false, true} {
		t.Run(map[bool]string{false: "macOS", true: "Linux"}[linux], func(t *testing.T) {
			p, _ := dockerFixture(t, linux)
			p.Config.Slots[0].Count = 2
			warmPlain(t, p)
			a, b := dockerLaunch(), dockerLaunch()
			a.LaunchID, b.LaunchID = "request-a", "request-b"
			for range 2 { // Retry the batch while preparation is still pending.
				out, err := p.Submit(t.Context(), []api.Launch{a, b})
				if err != nil || len(out) != 2 || out[0].Status != api.NoCapacity || out[1].Status != api.NoCapacity {
					t.Fatal(out, err)
				}
			}
			hints := map[string]bool{}
			for _, pair := range p.Store.data.Pairs {
				if pair.DockerRequested {
					hints[pair.RequestID] = true
				}
			}
			if len(hints) != 2 || !hints[a.LaunchID] || !hints[b.LaunchID] || len(p.Store.data.Records) != 0 {
				t.Fatal("requests were serialized or prematurely admitted", hints)
			}
		})
	}
}

func TestLinuxEnginePollingDoesNotRewriteRunningPreparation(t *testing.T) {
	p, _ := dockerFixture(t, true)
	script, err := p.engineScript(p.Config.Engine("engine"), nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// Execute the generated shell against a simulated systemd host. The mock
	// never writes /var/lib; it records installation and daemon-start requests.
	sudo := `#!/bin/bash
set -eu
shift
case "$1 $2" in
 'systemctl cat') test -e "$PULSE_TEST_STATE/started" ;;
 'systemctl is-failed') exit 1 ;;
 'systemd-run --no-block') echo start >> "$PULSE_TEST_STATE/operations"; touch "$PULSE_TEST_STATE/started" ;;
 'tee /var/lib/pulse-docker/prepare.sh') cat >/dev/null; echo write >> "$PULSE_TEST_STATE/operations" ;;
 'journalctl -u') echo 'PULSE_DOCKER_ENGINE_READY test-engine' ;;
 'install -d'|'chmod 600') exit 0 ;;
 *) echo "unexpected command" >&2; exit 23 ;;
esac
`
	for name, content := range map[string]string{"sudo": sudo, "base64": "#!/bin/sh\ncat\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		cmd := exec.CommandContext(t.Context(), "/bin/bash")
		cmd.Stdin = strings.NewReader(script)
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "PULSE_TEST_STATE="+dir)
		out, err := cmd.CombinedOutput()
		if err != nil || engineIdentity(string(out)) != "test-engine" {
			t.Fatalf("poll: %v %s", err, out)
		}
	}
	operations, err := os.ReadFile(filepath.Join(dir, "operations"))
	if err != nil || string(operations) != "write\nstart\n" {
		t.Fatalf("poll rewrote or restarted preparation: %q %v", operations, err)
	}
}
