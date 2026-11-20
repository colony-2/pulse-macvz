package local

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse-tart/internal/config"
)

// No Tart or VM is needed. Exercise the real worker supervisor before expensive
// image builds, including Close/reopen, launchd restart and fixed-port cleanup.
func TestLiveDockerEndpoint(t *testing.T) {
	if os.Getenv("PULSE_TEST_DOCKER_ENDPOINT") != "1" {
		t.Skip("set PULSE_TEST_DOCKER_ENDPOINT=1 for the Mac launchd preflight")
	}
	testLiveDockerEndpoint(t, "tart", "")
}

func TestLiveVZDockerEndpoint(t *testing.T) {
	runner := os.Getenv("PULSE_VZ_RUNNER")
	if os.Getenv("PULSE_TEST_VZ_DOCKER_ENDPOINT") != "1" || runner == "" {
		t.Skip("set PULSE_TEST_VZ_DOCKER_ENDPOINT=1 and PULSE_VZ_RUNNER for the Go launchd preflight")
	}
	if !filepath.IsAbs(runner) {
		t.Fatal("runner must be absolute")
	}
	testLiveDockerEndpoint(t, "vz", runner)
}

// Exercise the exact launchd helper against a live guest, not loopback. The
// driver owns the disposable guest and keeps it alive through service restart.
func TestLiveVZNetworkEndpoint(t *testing.T) {
	if os.Getenv("PULSE_TEST_VZ_NETWORK_ENDPOINT") != "1" {
		t.Skip("opt-in launchd-to-guest network gate")
	}
	runner := os.Getenv("PULSE_VZ_RUNNER")
	if !filepath.IsAbs(runner) {
		t.Fatal("requires an absolute signed app runner")
	}
	host, _, err := net.SplitHostPort(os.Getenv("PULSE_TEST_VZ_NETWORK_TARGET"))
	if err != nil || net.ParseIP(host) == nil || net.ParseIP(host).IsLoopback() {
		t.Fatal("requires a non-loopback guest SSH endpoint")
	}
	testLiveDockerEndpoint(t, "vz", runner)
}

func testLiveDockerEndpoint(t *testing.T, kind, runner string) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Fatal("launchd preflight requires macOS")
	}
	networkGate := t.Name() == "TestLiveVZNetworkEndpoint"
	limit := 45 * time.Second
	if networkGate {
		limit = 90 * time.Second
		if value := os.Getenv("PULSE_TEST_VZ_NETWORK_TIMEOUT"); value != "" {
			seconds, err := strconv.Atoi(value)
			if err != nil || seconds < 5 || seconds > 120 {
				t.Fatal("network gate timeout must be 5–120 seconds")
			}
			limit = time.Duration(seconds) * time.Second
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), limit)
	defer cancel()
	common := config.Config{StateDir: t.TempDir()}
	factory := func() *Client {
		b, err := openNative(config.Tart{Home: common.StateDir}, common, kind, nil)
		if err != nil {
			t.Fatal(err)
		}
		if runner != "" {
			b.endpointArgs = func(target, ready string, port int) []string {
				return []string{runner, "__pulse-vm", "bridge", "--target", target, "--ready", ready, "--port", strconv.Itoa(port)}
			}
		}
		return b
	}
	b := factory()
	uidBytes := make([]byte, 16)
	if _, err := rand.Read(uidBytes); err != nil {
		t.Fatal(err)
	}
	uid := hex.EncodeToString(uidBytes)
	if directory := os.Getenv("PULSE_TEST_OUTPUT_DIR"); directory != "" {
		label := "user/" + strconv.Itoa(os.Getuid()) + "/" + b.endpointLabel(uid)
		if err := os.WriteFile(filepath.Join(directory, "endpoint-service.txt"), []byte(label+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "endpoint-ready") }))
	defer target.Close()
	address := strings.TrimPrefix(target.URL, "http://")
	if networkGate {
		address = os.Getenv("PULSE_TEST_VZ_NETWORK_TARGET")
	}
	reserve, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reserve.Addr().(*net.TCPAddr).Port
	reserve.Close()
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	var snapshot map[string]map[string]string
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		d, err := b.EndpointDiagnostics(cleanup)
		if err != nil {
			t.Errorf("endpoint diagnostics: %v", err)
		}
		if len(d) == 0 {
			d = snapshot
		}
		if directory := os.Getenv("PULSE_TEST_OUTPUT_DIR"); directory != "" {
			data, _ := json.MarshalIndent(d, "", "  ")
			if err := os.WriteFile(filepath.Join(directory, "endpoint-preflight.json"), append(data, '\n'), 0600); err != nil {
				t.Error(err)
			}
		}
		if err := b.RemoveEndpoint(cleanup, "preflight", uid); err != nil {
			t.Errorf("endpoint cleanup: %v", err)
		}
		if err := b.RemoveEndpoint(cleanup, "preflight", uid); err != nil {
			t.Errorf("repeated endpoint cleanup: %v", err)
		}
		b.Close()
	}()
	pid := func() int {
		var ready struct{ PID int }
		data, _ := os.ReadFile(filepath.Join(b.dir, "endpoints", uid, "ready.json"))
		json.Unmarshal(data, &ready)
		return ready.PID
	}
	wait := func(oldPID int) {
		t.Helper()
		progress := time.Now()
		for ctx.Err() == nil {
			ready, err := b.ensureEndpoint(ctx, uid, address, port)
			if err != nil {
				t.Fatal(err)
			}
			if ready && pid() != 0 && pid() != oldPID {
				if networkGate {
					if err := probeEngineEndpoint(ctx, port); err == nil {
						t.Log("guest SSH reachable through launchd bridge")
						return
					} else if time.Since(progress) >= 5*time.Second {
						progress = time.Now()
						t.Logf("waiting for guest SSH through launchd: %v (check the Pulse VZ Local Network prompt)", err)
					}
				} else {
					response, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port))
					if err == nil {
						data, _ := io.ReadAll(response.Body)
						response.Body.Close()
						if string(data) == "endpoint-ready" {
							return
						}
					}
				}
			}
			select {
			case <-ctx.Done():
			case <-time.After(100 * time.Millisecond):
			}
		}
		t.Fatal("worker endpoint did not become reachable", ctx.Err())
	}
	wait(0)
	original := pid()
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b = factory()
	wait(0)
	if pid() != original {
		t.Fatal("Close/reopen restarted the host service")
	}
	label := "user/" + strconv.Itoa(os.Getuid()) + "/" + b.endpointLabel(uid)
	if out, err := b.service(ctx, "kill", "SIGKILL", label); err != nil {
		t.Fatalf("restart injection: %v %s", err, out)
	}
	wait(original)
	snapshot, err = b.EndpointDiagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.RemoveEndpoint(ctx, "preflight", uid); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port)); err == nil {
		t.Fatal("endpoint still accepts requests after removal")
	}
	if networkGate {
		t.Log("DOCKER_NETWORK_ENDPOINT_PASS: guest SSH through launchd; Close/reopen; same-port restart; removal")
	} else {
		t.Log("DOCKER_ENDPOINT_SERVICE_PASS: Background launchd; forwarding; Close/reopen; same-port restart; removal")
	}
}
