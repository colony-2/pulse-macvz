// Test-only fixture source. Copy into Pulse's package providers by changing
// only the package declaration. Local runtime tests import this helper directly.
package diagnosticfixture

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/colony-2/pulse-provider-api"
	tart "github.com/colony-2/pulse-tart"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/websocket"
)

const TartDiagnosticInventory = "FIXTURE inventory: backend shard unavailable\nrequest=\"fixture-01\""
const TartDiagnosticPreparation = "FIXTURE preparation: source read denied"
const TartDiagnosticCreate = "FIXTURE create: response lost after dispatch"

// TartDiagnosticFixture exposes real runtime operations; no production test seam
// or journal mutation is used. Do not call Runtime.Run: setup owns reconciliation.
type TartDiagnosticFixture struct {
	Runtime      *tart.Runtime
	Config       tart.Config
	Base         tart.Base
	Capabilities api.Capabilities
	mu           sync.Mutex
	scenario     string
	objects      map[string]map[string]any
	roles        map[string]string
	templates    []string
	baseName     string
	counts       map[string]int
	transcript   []string
	server       *httptest.Server
	sshConfig    *ssh.ServerConfig
	connections  map[net.Conn]bool
	wg           sync.WaitGroup
	closed       bool
}

func tartDiagnosticToken(value string) string {
	if len(value) <= 24 && regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`).MatchString(value) {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("id-%x", sum[:10])
}
func tartDiagnosticBase(owner, base string) string {
	return "pulse-" + tartDiagnosticToken(owner) + "-" + tartDiagnosticToken(base) + "-base"
}
func tartDiagnosticSlot(owner string, g tart.SlotGroup, i int) string {
	return fmt.Sprintf("pulse-%s-%s-%s-%dcpu-%dm-slot-%04d", tartDiagnosticToken(owner), tartDiagnosticToken(g.Base), tartDiagnosticToken(g.Name), g.CPU, g.MemoryMiB, i)
}

// NewTartDiagnosticFixture does not reconcile. Select "preparation" before Ready
// to test its real error return, or call Ready first and activate a submit fault.
func NewTartDiagnosticFixture(t testing.TB, docker bool) *TartDiagnosticFixture {
	t.Helper()
	_, key, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	signer, e := ssh.NewSignerFromKey(key)
	if e != nil {
		t.Fatal(e)
	}
	f := &TartDiagnosticFixture{objects: map[string]map[string]any{}, roles: map[string]string{}, counts: map[string]int{}, connections: map[net.Conn]bool{}}
	f.sshConfig = &ssh.ServerConfig{PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) { return nil, nil }}
	f.sshConfig.AddHostKey(signer)
	dir := t.TempDir()
	if e = os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	c := tart.Config{OwnerID: "diagnostic-fixture", StateDir: dir, Bases: []tart.Base{{Name: "macos-sequoia-15.7.7-arm64", OSVersion: "15.7.7", Image: "example.invalid/macos@sha256:" + strings.Repeat("a", 64), SSHUsername: "fixture", SSHPassword: "fixture-password", SSHHostKey: ssh.MarshalAuthorizedKey(signer.PublicKey()), Capabilities: []string{"nix", "git"}}}, Slots: []tart.SlotGroup{{Name: "small", Base: "macos-sequoia-15.7.7-arm64", Count: 3}}, VMCounts: tart.VMCounts{MacOS: tart.VMCount(2), Linux: tart.VMCount(2), Total: tart.VMCount(4)}}
	if docker {
		c.Slots[0].Count = 1
		c.Slots[0].DockerProfile = "fixture-engine"
		c.Docker = &tart.DockerConfig{ReadyTimeout: time.Minute, Profiles: []tart.DockerEngineProfile{{Name: "fixture-engine", Image: "example.invalid/linux@sha256:" + strings.Repeat("b", 64), SSHUsername: "fixture", SSHPassword: "fixture-password", CPU: 4, MemoryMiB: 8192, DiskGiB: 32, EngineVersion: "29.1.3"}}}
	}
	c, e = tart.ResolveConfig(c)
	if e != nil {
		t.Fatal(e)
	}
	f.Config = c
	f.Base = c.Bases[0]
	baseLabel := f.Base.Name
	if docker {
		baseLabel += "-docker"
	}
	f.baseName = tartDiagnosticBase(c.OwnerID, baseLabel)
	f.objects[f.baseName] = f.source(f.baseName, f.Base.Image, "stopped")
	f.roles[f.baseName] = "base"
	for i := 1; i <= c.Slots[0].Count; i++ {
		g := c.Slots[0]
		if docker {
			g.Name += "-docker-" + g.DockerProfile
		}
		name := tartDiagnosticSlot(c.OwnerID, g, i)
		f.templates = append(f.templates, name)
		f.objects[name] = f.source(name, "local-"+f.baseName, "suspended")
		f.roles[name] = "template"
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	f.Runtime, e = tart.NewOrchard(c, tart.OrchardConfig{URL: f.server.URL, AllowInsecureHTTP: true, Worker: "fixture-mac", RequestTimeout: 5 * time.Second, DockerEndpoint: tart.DockerEndpoint{Address: "127.0.0.1", PortMin: 45101, PortMax: 45102}})
	if e != nil {
		f.server.Close()
		t.Fatal(e)
	}
	f.Capabilities, e = f.Runtime.GetCapabilities(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := f.Close(); e != nil {
			t.Error(e)
		}
	})
	return f
}
func (f *TartDiagnosticFixture) source(name, image, power string) map[string]any {
	return map[string]any{"name": name, "uid": "uid-" + name, "image": image, "os": "darwin", "arch": "arm64", "runtime": "tart", "cpu": 4, "memory": 8192, "username": "fixture", "password": "fixture-password", "worker": "fixture-mac", "localName": "local-" + name, "powerState": power, "generation": 1, "observedGeneration": 1, "status": "running", "conditions": []any{map[string]any{"type": "running", "state": "false"}}}
}
func (f *TartDiagnosticFixture) Scenario(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scenario = name
}
func (f *TartDiagnosticFixture) Snapshot() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for k, v := range f.counts {
		out[k] = v
	}
	return out
}
func (f *TartDiagnosticFixture) Transcript() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.transcript...)
}
func (f *TartDiagnosticFixture) record(method, role string) {
	key := method + " " + role
	f.counts[key]++
	f.transcript = append(f.transcript, fmt.Sprintf("%s count=%d", key, f.counts[key]))
}
func (f *TartDiagnosticFixture) Ready(ctx context.Context) error {
	for i := 0; i < 16; i++ {
		if e := f.Runtime.Reconcile(ctx); e != nil {
			return e
		}
		slots, e := f.Runtime.Slots()
		if e != nil {
			return e
		}
		images, e := f.Runtime.Images()
		if e != nil {
			return e
		}
		ready := len(slots) == len(f.templates)
		for _, s := range slots {
			ready = ready && s.Phase == "ready"
		}
		hasBase := false
		for _, im := range images {
			if im.Name == f.baseName && im.Phase == "ready" {
				hasBase = true
			}
		}
		if f.Config.Docker != nil {
			pairs, e := f.Runtime.DockerAllocations()
			if e != nil {
				return e
			}
			ready = ready && len(pairs) == 1 && pairs[0].Phase == "ready"
		}
		if ready && hasBase {
			return nil
		}
	}
	return fmt.Errorf("fixture setup did not reach ready phases within 16 reconciliations")
}
func (f *TartDiagnosticFixture) Launch(id string) api.Launch {
	caps := append([]string{}, f.Base.Capabilities...)
	memory := int64(8192 << 20)
	if f.Config.Docker != nil {
		caps = append(caps, "docker")
		memory = 7680 << 20
	}
	return api.Launch{LaunchID: id, Allocation: api.Allocation{Base: f.Base.Name, CPUMillis: 4000, MemoryBytes: memory, Capabilities: caps}, Job: api.JobRef{JobDB: "http://fixture.invalid/jobdb", InstanceID: "fixture-db", TenantID: "fixture-tenant", JobID: "fixture-job"}, Lease: "FIXTURE-LEASE-DO-NOT-CAPTURE"}
}

// Mixed establishes the first reservation through Submit, then supplies a
// source-referencing foreign clone and an invalid third suspension observation.
func (f *TartDiagnosticFixture) Mixed(ctx context.Context) error {
	if f.Config.Docker != nil {
		return fmt.Errorf("mixed requires ordinary fixture")
	}
	out, e := f.Runtime.Submit(ctx, []api.Launch{f.Launch("fixture-prior")})
	if e != nil {
		return e
	}
	if len(out) != 1 || out[0].Status != api.Accepted {
		return fmt.Errorf("prior placement was not accepted")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects["fixture-foreign"] = f.source("fixture-foreign", "local-"+f.templates[1], "stopped")
	f.roles["fixture-foreign"] = "foreign"
	f.objects[f.templates[2]]["powerState"] = "running"
	return nil
}
func (f *TartDiagnosticFixture) serve(w http.ResponseWriter, r *http.Request) {
	r.URL.Path = strings.TrimPrefix(r.URL.Path, "/v1")
	if strings.HasSuffix(r.URL.Path, "/port-forward") {
		f.forward(w, r)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/controller/info" {
		f.record(r.Method, "info")
		io.WriteString(w, `{"version":"fixture"}`)
		return
	}
	if r.URL.Path == "/vms" && r.Method == "GET" {
		f.record("GET", "inventory")
		if f.scenario == "inventory" {
			w.WriteHeader(503)
			io.WriteString(w, TartDiagnosticInventory)
			return
		}
		names := []string{}
		for name := range f.objects {
			names = append(names, name)
		}
		sort.Strings(names)
		objects := []map[string]any{}
		for _, n := range names {
			objects = append(objects, f.objects[n])
		}
		json.NewEncoder(w).Encode(objects)
		return
	}
	if r.URL.Path == "/vms" && r.Method == "POST" {
		var v map[string]any
		if e := json.NewDecoder(r.Body).Decode(&v); e != nil {
			w.WriteHeader(400)
			return
		}
		name, _ := v["name"].(string)
		role := "job"
		if strings.HasSuffix(name, "-base") {
			role = "engine-base"
		}
		if strings.HasSuffix(name, "-engine") {
			role = "engine"
		}
		if strings.HasSuffix(name, "-primary") {
			role = "primary"
		}
		f.record("POST", role)
		v["uid"] = "uid-" + name
		v["worker"] = "fixture-mac"
		v["localName"] = "local-" + name
		v["powerState"] = "running"
		v["status"] = "running"
		v["conditions"] = []any{map[string]any{"type": "running", "state": "true"}}
		if role == "engine-base" {
			v["powerState"], v["status"] = "stopped", "failed"
			v["conditions"] = []any{map[string]any{"type": "running", "state": "false"}}
		}
		f.objects[name] = v
		f.roles[name] = role
		if f.scenario == "create" && role == "job" {
			w.WriteHeader(503)
			io.WriteString(w, TartDiagnosticCreate)
			return
		}
		json.NewEncoder(w).Encode(v)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/vms/")
	name := strings.TrimSuffix(path, "/events")
	role := f.roles[name]
	if role == "" {
		role = "missing"
	}
	if strings.HasSuffix(path, "/events") {
		f.record("GET", "events-"+role)
		if role == "engine-base" {
			json.NewEncoder(w).Encode([]map[string]string{{"payload": "PULSE_BASE_READY"}})
			return
		}
		json.NewEncoder(w).Encode([]map[string]string{{"payload": "PULSE_DOCKER_ENGINE_READY fixture-engine-id"}})
		return
	}
	f.record(r.Method, role)
	if f.scenario == "preparation" && r.Method == "GET" && name == f.baseName {
		w.WriteHeader(503)
		io.WriteString(w, TartDiagnosticPreparation)
		return
	}
	v, ok := f.objects[name]
	if !ok {
		w.WriteHeader(404)
		io.WriteString(w, "fixture missing object")
		return
	}
	if r.Method == "DELETE" {
		delete(f.objects, name)
		io.WriteString(w, `{}`)
		return
	}
	if r.Method == "PUT" {
		var updated map[string]any
		if e := json.NewDecoder(r.Body).Decode(&updated); e != nil {
			w.WriteHeader(400)
			return
		}
		if ep, ok := updated["endpoints"].([]any); ok {
			for _, e := range ep {
				entry := e.(map[string]any)
				if entry["name"] == "pulse-engine-ssh" {
					pr := entry["workerPortRange"].(map[string]any)
					updated["observedEndpoints"] = []any{map[string]any{"name": "pulse-engine-ssh", "state": "listening", "workerPort": pr["min"]}}
				}
			}
		}
		f.objects[name] = updated
		v = updated
	}
	json.NewEncoder(w).Encode(v)
}
func (f *TartDiagnosticFixture) forward(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	scenario := f.scenario
	f.record("WS", "management")
	f.mu.Unlock()
	if scenario == "websocket" {
		http.Error(w, "fixture WebSocket denied", 503)
		return
	}
	websocket.Handler(func(ws *websocket.Conn) {
		ws.PayloadType = websocket.BinaryFrame
		f.mu.Lock()
		if f.closed {
			f.mu.Unlock()
			ws.Close()
			return
		}
		f.connections[ws] = true
		f.wg.Add(1)
		f.mu.Unlock()
		defer func() { ws.Close(); f.mu.Lock(); delete(f.connections, ws); f.mu.Unlock(); f.wg.Done() }()
		ws.SetDeadline(time.Now().Add(10 * time.Second))
		if scenario == "handshake" {
			io.WriteString(ws, "SSH-2.0-fixture-invalid\r\n")
			return
		}
		conn, channels, requests, e := ssh.NewServerConn(ws, f.sshConfig)
		if e != nil {
			return
		}
		defer conn.Close()
		requestsDone := make(chan struct{})
		go func() { ssh.DiscardRequests(requests); close(requestsDone) }()
		defer func() { conn.Close(); <-requestsDone }()
		for incoming := range channels {
			if scenario == "session" {
				incoming.Reject(ssh.Prohibited, "fixture session denied")
				continue
			}
			ch, reqs, e := incoming.Accept()
			if e != nil {
				return
			}
			for req := range reqs {
				if req.Type != "exec" {
					req.Reply(false, nil)
					continue
				}
				req.Reply(true, nil)
				script, e := io.ReadAll(io.LimitReader(ch, 4<<20))
				if e != nil {
					ch.Close()
					break
				}
				ready := strings.Contains(string(script), "PULSE_DOCKER_PRIMARY_READY")
				handoff := strings.Contains(string(script), "pulse-tart-guest ingest")
				script = nil
				role := "configure"
				if ready {
					role = "readiness"
				}
				if handoff {
					role = "handoff"
				}
				f.mu.Lock()
				f.record("SSH", role)
				f.mu.Unlock()
				code := uint32(0)
				output := ""
				if ready {
					output = "PULSE_DOCKER_PRIMARY_READY\n"
				}
				if handoff {
					output = "PULSE_HANDOFF_OK\n"
				}
				if scenario == "readiness-error" && ready || scenario == "handoff-error" && handoff {
					code = 23
					output = ""
				}
				if scenario == "readiness-marker" && ready || scenario == "handoff-marker" && handoff {
					output = "fixture unrelated output must not be captured\n"
				}
				io.WriteString(ch, output)
				ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
				ch.Close()
				break
			}
		}
	}).ServeHTTP(w, r)
}
func (f *TartDiagnosticFixture) Close() error {
	if f.Runtime == nil {
		return nil
	}
	e := f.Runtime.Close()
	f.mu.Lock()
	f.closed = true
	for conn := range f.connections {
		conn.Close()
	}
	f.mu.Unlock()
	f.server.Close()
	f.wg.Wait()
	return e
}
