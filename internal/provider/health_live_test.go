//go:build darwin && arm64

package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse-macvz/internal/backend"
	"github.com/colony-2/pulse-macvz/internal/config"
	"github.com/colony-2/pulse-macvz/internal/local"
	"github.com/colony-2/pulse-macvz/internal/vz"
	"github.com/colony-2/pulse-macvz/internal/workerhealth"
	api "github.com/colony-2/pulse-provider-api"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

// Uses a cold prepared Ubuntu source and a real Linux ARM64 c2j binary. The
// supplied synthetic lease is sent exclusively to a loopback fixture, never
// JobDB. Tests the normal guest helper and production host probe/stop/reconcile.
func TestLiveWorkerHealthReaping(t *testing.T) {
	base, runner, root, binary := os.Getenv("PULSE_VZ_LINUX_BASE"), os.Getenv("PULSE_VZ_RUNNER"), os.Getenv("PULSE_VZ_RESULTS"), os.Getenv("PULSE_HEALTH_C2J_BINARY")
	if base == "" || runner == "" || root == "" || binary == "" {
		t.Skip("set PULSE_VZ_LINUX_BASE, PULSE_VZ_RUNNER, PULSE_VZ_RESULTS and PULSE_HEALTH_C2J_BINARY")
	}
	home, err := os.MkdirTemp(root, "health-vm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("disposable home:", home)
	disk, err := os.Open(filepath.Join(base, "disk.img"))
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	if err = unix.Flock(int(disk.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		t.Fatal("base disk busy", err)
	}
	if _, err = os.Lstat(filepath.Join(base, "state.vzvmsave")); !os.IsNotExist(err) {
		t.Fatal("requires cold source")
	}
	dir := filepath.Join(home, "vms", "source")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config.json", "disk.img", "nvram.bin"} {
		if err = unix.Clonefile(filepath.Join(base, name), filepath.Join(dir, name), 0); err != nil {
			t.Fatal(err)
		}
	}
	var hardware struct {
		OS     string `json:"os"`
		CPU    int64  `json:"cpuCount"`
		Memory int64  `json:"memorySize"`
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil || json.Unmarshal(data, &hardware) != nil || hardware.OS != "linux" {
		t.Fatal("requires Linux source", err)
	}
	common := config.Config{OwnerID: "health-qualification", BootstrapTimeout: time.Minute}
	b, err := vz.New(config.VZ{Home: home, Runner: runner, SSHTimeout: 5 * time.Second}, common)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	source := backend.Spec{Name: "source", OS: "linux", Arch: "arm64", CPU: hardware.CPU, Memory: hardware.Memory >> 20, Username: "admin", Password: "admin"}
	if _, err = b.AdoptSource(ctx, source, nil); err != nil {
		t.Fatal(err)
	}
	spec := source
	spec.Name = "health-job"
	spec.Image = "source"
	spec.CPU = 2
	spec.Memory = 4096
	spec.StartupScript.Content = "echo PULSE_HANDOFF_OK\n"
	v, err := b.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		if current, e := b.Get(cleanup, v.Name); e == nil && current.UID == v.UID {
			if e = b.Stop(cleanup, v.Name, v.UID); e != nil {
				t.Error("cleanup", e)
			}
		}
	}()
	for {
		v, err = b.Get(ctx, spec.Name)
		if err != nil {
			t.Fatal(err)
		}
		if v.Status == "running" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Second):
		}
	}
	s := &vz.Store{Home: home}
	ip, err := s.IP(ctx, v.Name)
	if err != nil {
		t.Fatal(err)
	}
	// Stream the executable over management SSH; do not turn large binaries into
	// shell arguments. Host-key material comes from this disposable VM's delivery.
	data, err = os.ReadFile(filepath.Join(home, "vms", v.Name, "pulse.json"))
	if err != nil {
		t.Fatal(err)
	}
	var native struct {
		Native struct {
			HostKey []byte `json:"host_key"`
		}
	}
	if err = json.Unmarshal(data, &native); err != nil {
		t.Fatal(err)
	}
	callback := ssh.InsecureIgnoreHostKey() // Same explicit disposable-source policy as local.Client.
	if len(native.Native.HostKey) > 0 {
		key, _, _, _, err := ssh.ParseAuthorizedKey(native.Native.HostKey)
		if err != nil {
			t.Fatal(err)
		}
		callback = ssh.FixedHostKey(key)
	}
	for {
		if _, err = b.Exec(ctx, v.Name, v.UID, []byte("true\n")); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Second):
		}
	}
	conn, err := ssh.Dial("tcp", net.JoinHostPort(ip, "22"), &ssh.ClientConfig{User: v.Username, Auth: []ssh.AuthMethod{ssh.Password(v.Password)}, HostKeyCallback: callback, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	session, err := conn.NewSession()
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	f, err := os.Open(binary)
	if err != nil {
		t.Fatal(err)
	}
	session.Stdin = f
	err = session.Run("sudo -n tee /var/tmp/c2j-health >/dev/null && sudo -n chmod 755 /var/tmp/c2j-health")
	f.Close()
	session.Close()
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	out, err := b.Exec(ctx, v.Name, v.UID, []byte("/var/tmp/c2j-health run with-lease --help | grep -- --health-port\n"))
	if err != nil || !strings.Contains(string(out), "--health-port") {
		t.Fatal("binary lacks health probe", err)
	}
	// The production delivery observer writes this same capability marker from
	// --help. Here the executable was uploaded after initial VM preparation.
	// Wait for the initial native delivery observer to finish before recording
	// the capability of the subsequently uploaded test executable.
	for {
		raw, e := os.ReadFile(filepath.Join(home, "vms", v.Name, "pulse.json"))
		if e != nil {
			t.Fatal(e)
		}
		var receipt struct{ Native struct{ Delivered bool } }
		if json.Unmarshal(raw, &receipt) == nil && receipt.Native.Delivered {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	data, _ = json.Marshal(local.ImageVersion{Version: "health-qualification", HealthProbe: true})
	if err = os.WriteFile(filepath.Join(home, "vms", v.Name, local.ImageVersionFile), data, 0600); err != nil {
		t.Fatal(err)
	}
	fixture := `import http.server,time
class Handler(http.server.BaseHTTPRequestHandler):
 def do_POST(self): time.sleep(600)
 def do_GET(self): time.sleep(600)
 def log_message(self,*args): pass
http.server.ThreadingHTTPServer(('127.0.0.1',18081),Handler).serve_forever()
`
	setup := "set -eu\nprintf '%s' '" + base64.StdEncoding.EncodeToString([]byte(fixture)) + "' | base64 -d | sudo -n tee /var/tmp/health-jobdb.py >/dev/null\nsudo -n systemd-run --quiet --collect --unit=health-jobdb /usr/bin/python3 /var/tmp/health-jobdb.py\n"
	setup += "for i in $(seq 1 50); do python3 -c 'import socket; socket.create_connection((\"127.0.0.1\",18081),1).close()' 2>/dev/null && exit 0; sleep .1; done; exit 1\n"
	if _, err = b.Exec(ctx, v.Name, v.UID, []byte(setup)); err != nil {
		t.Fatal(err)
	}
	e := envelope{Version: 1, LaunchID: "health-qualification", Process: process{Command: []string{"/var/tmp/c2j-health"}, Args: []string{"run", "with-lease", "--jobdb", "http://127.0.0.1:18081/health-test", "--job-id", "startup-test", "--lease-file", "-", "--ci", "--input-mode", "fail"}, WorkingDir: "/home/pulseworker", StdinBytes: []byte(`{"version":2,"tenantId":"health-test","jobId":"startup-test","leaseId":"fixture","leaseToken":"fixture"}`)}}
	data, _ = json.Marshal(e)
	handoff := "printf '%s' '" + base64.StdEncoding.EncodeToString(data) + "' | base64 -d | sudo -n /opt/pulse-tart/bin/pulse-tart-guest ingest\nsudo -n /bin/bash /opt/pulse-tart-image/bootstrap-guest.sh\n"
	started := time.Now()
	if _, err = b.Exec(ctx, v.Name, v.UID, []byte(handoff)); err != nil {
		t.Fatal(err)
	}
	p := fixtureProviderForHealth(t, b, home)
	r := &Record{LaunchID: e.LaunchID, Name: v.Name, UID: v.UID, NativeHash: p.Store.hash(normalized(v.Spec)), Fingerprint: "qualification", Outcome: api.Accepted, Phase: "active", Job: api.JobRef{TenantID: "health-test", JobID: "startup-test"}, Health: &workerhealth.State{}}
	p.Store.data.Records[r.LaunchID] = r
	seen := map[string]bool{}
	for r.Health.LastSuccess == nil {
		if err = p.checkLaunchHealth(ctx, time.Now().UTC(), seen); err != nil {
			t.Fatal(err)
		}
		if time.Since(started) > 10*time.Second {
			t.Fatal("real c2j did not become healthy", r.Health)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Log("normal helper handoff to healthy probe:", time.Since(started))
	signal := func(sig string) {
		t.Helper()
		_, err = b.Exec(ctx, v.Name, v.UID, []byte("sudo -n sh -c 'kill -"+sig+" $(cat /var/db/pulse-macvz/pid)'\n"))
		if err != nil {
			t.Fatal(err)
		}
	}
	signal("STOP")
	if err = p.checkLaunchHealth(ctx, time.Now().UTC(), seen); err != nil {
		t.Fatal(err)
	}
	if r.Health.Failures != 1 {
		t.Fatal("hung process was not detected")
	}
	signal("CONT")
	time.Sleep(time.Second)
	if err = p.checkLaunchHealth(ctx, time.Now().UTC(), seen); err != nil {
		t.Fatal(err)
	}
	if r.Health.Status != "healthy" {
		t.Fatal("transient outage did not recover", r.Health)
	}
	signal("STOP")
	failed := time.Now()
	for r.Health.ReapRequestedAt == nil {
		if err = p.checkLaunchHealth(ctx, time.Now().UTC(), seen); err != nil {
			t.Fatal(err)
		}
		if time.Since(failed) > time.Minute {
			t.Fatal("hung worker not reaped", r.Health)
		}
		time.Sleep(workerhealth.Interval)
	}
	t.Log("sustained outage to VM stop:", time.Since(failed))
	for {
		vms, err := b.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = p.reconcileRecords(ctx, vms); err != nil {
			t.Fatal(err)
		}
		if r.Phase == "done" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Second):
		}
	}
	if _, err = b.Get(ctx, v.Name); !backend.IsStatus(err, 404) {
		t.Fatal("VM not reclaimed", err)
	}
	t.Log("HEALTH_REAP_PASS: actual c2j; normal guest helper; transient recovery; frozen worker; native stop and deletion")
}
func fixtureProviderForHealth(t *testing.T, b Backend, home string) *Provider {
	t.Helper()
	s, err := OpenStore(filepath.Join(home, "health-state"), "health-qualification")
	if err != nil {
		t.Fatal(err)
	}
	p := Attach(config.Settings{}, b, s)
	t.Cleanup(func() { p.Close() })
	return p
}
