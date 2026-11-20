package servercli

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-provider-api/plugin"
)

// Exercise the signed executable, embedded runner check, and real VZ backend
// with zero capacity. This test never creates a VM or launches an actual job.
func TestLiveVZServer(t *testing.T) {
	binary := os.Getenv("PULSE_TEST_SERVER_BINARY")
	if binary == "" {
		t.Skip("set PULSE_TEST_SERVER_BINARY to an absolute signed server executable")
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" || !filepath.IsAbs(binary) {
		t.Fatal("requires an absolute signed binary on Apple Silicon macOS")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	hostTLS, flags := fixtureTrust(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	collector, err := plugin.StartCollector(ctx, listener, plugin.CollectorOptions{TLSConfig: hostTLS, Authorize: func(_ context.Context, state tls.ConnectionState) (string, error) {
		if len(state.VerifiedChains) == 0 {
			return "", errors.New("unverified client")
		}
		return "worker", nil
	}})
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	defer collector.Close()
	dir := t.TempDir()
	t.Setenv("HOME", dir) // Installed user settings must not affect this isolated smoke test.
	home := filepath.Join(dir, "home")
	args := append([]string{"server", "--collector", listener.Addr().String(), "--name", "worker", "--owner", "cli-smoke", "--vm-home", home, "--state-dir", filepath.Join(dir, "state"), "--macos-capacity", "0", "--linux-capacity", "0"}, flags...)
	cmd := exec.CommandContext(ctx, binary, args...)
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			<-done
		}
		if t.Failed() {
			t.Log(logs.String())
		}
	}()
	var provider api.Provider
	for provider == nil {
		snapshot, err := collector.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Providers) == 1 {
			provider = snapshot.Providers[0].Provider
			break
		}
		if _, err := collector.WaitForChange(ctx, snapshot.Revision); err != nil {
			t.Fatal(err)
		}
	}
	caps, err := provider.GetCapabilities(ctx)
	if err != nil || len(caps.Bases) != 2 {
		t.Fatalf("discovery: %v", err)
	}
	list, err := provider.List(ctx, api.ListRequest{})
	if err != nil || len(list.Items) != 0 {
		t.Fatalf("unexpected active work: %v", err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		waited = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("server did not stop after SIGTERM")
	}
	entries, err := os.ReadDir(filepath.Join(home, "vms"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("zero-capacity server created %d VM entries", len(entries))
	}
	t.Log("VZ_SERVER_PASS: signed executable, mTLS registration, discovery/list, SIGTERM, zero VMs")
}
