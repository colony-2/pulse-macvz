package local

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse-macvz/internal/config"
)

// Executed by an actual child process, with a protocol on argv containing no
// launch data. Writes much more than a pipe buffer to prove complete drainage.
func TestDiagnosticCommandHelper(t *testing.T) {
	if os.Getenv("PULSE_DIAGNOSTIC_HELPER") != "1" {
		return
	}
	var size, code int
	fmt.Sscanf(os.Args[len(os.Args)-2], "%d", &size)
	fmt.Sscanf(os.Args[len(os.Args)-1], "%d", &code)
	if size < 0 {
		http.Get(os.Getenv("PULSE_DIAGNOSTIC_NOTIFY"))
		select {}
	}
	os.Stderr.WriteString(strings.Repeat("e", size))
	os.Exit(code)
}
func TestDiagnosticCommandCapture(t *testing.T) {
	t.Setenv("PULSE_DIAGNOSTIC_HELPER", "1")
	b := &Client{native: &fixtureHypervisor{binary: os.Args[0]}, cfg: config.Local{CommandTimeout: 10 * time.Second}, timeout: 10 * time.Second}
	for _, size := range []int{16383, 16384, 16385, 1 << 20} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			_, err := b.execute(t.Context(), "-test.run=^TestDiagnosticCommandHelper$", "--", fmt.Sprint(size), "7")
			var exit *exec.ExitError
			want := strings.Repeat("e", min(size, 16384))
			if size > 16384 {
				want += "[truncated after 16384 bytes]"
			}
			if !errors.Is(err, Err) || !errors.As(err, &exit) || exit.ExitCode() != 7 || !strings.HasSuffix(err.Error(), want) || strings.Contains(err.Error(), "[truncated") != (size > 16384) {
				t.Fatalf("capture size %d, error bytes=%d", size, len(err.Error()))
			}
			if _, err = b.execute(t.Context(), "-test.run=^TestDiagnosticCommandHelper$", "--", fmt.Sprint(size), "0"); err != nil {
				t.Fatal("stderr broke successful command", err)
			}
		})
	}
	b.native.(*fixtureHypervisor).binary = filepath.Join(t.TempDir(), "absent")
	if _, err := b.execute(t.Context(), "list"); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	b.native.(*fixtureHypervisor).binary = os.Args[0]
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := b.execute(ctx, "list"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestDiagnosticNativeAdapterCauses(t *testing.T) {
	b, _, s := fixture(t)
	raw := errors.New("native fixture\n\"original\"")
	b.native.(*fixtureHypervisor).command = func(context.Context, ...string) ([]byte, error) { return nil, raw }
	if _, err := b.List(t.Context()); !errors.Is(err, raw) {
		t.Fatal(err)
	}
	if _, err := b.Create(t.Context(), s); !errors.Is(err, raw) {
		t.Fatal(err)
	}
}

func TestDiagnosticExecGetIPAndSSHCauses(t *testing.T) {
	b, _, s := fixture(t)
	v, e := b.Create(t.Context(), s)
	if e != nil {
		t.Fatal(e)
	}
	// Fault injection must wait for the synthetic delivery worker to exit.
	// Exec observations remain independent of the background delivery loop.
	if e := b.Close(); e != nil {
		t.Fatal(e)
	}
	original := b.native.(*fixtureHypervisor).command
	raw := errors.New("IP fixture\n\"original\"")
	b.native.(*fixtureHypervisor).command = func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] == "ip" {
			return nil, raw
		}
		return original(ctx, args...)
	}
	if _, e = b.Exec(t.Context(), s.Name, v.UID, []byte("PRIVATE-SCRIPT")); !errors.Is(e, raw) {
		t.Fatal(e)
	}
	b.native.(*fixtureHypervisor).command = func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] == "ip" {
			return []byte("bad IP"), nil
		}
		return original(ctx, args...)
	}
	if _, e = b.Exec(t.Context(), s.Name, v.UID, []byte("PRIVATE-SCRIPT")); e == nil || !strings.Contains(e.Error(), "IP is invalid") {
		t.Fatal(e)
	}
	b.native.(*fixtureHypervisor).command = func(context.Context, ...string) ([]byte, error) { return nil, raw }
	if _, e = b.Exec(t.Context(), s.Name, v.UID, []byte("PRIVATE-SCRIPT")); !errors.Is(e, raw) {
		t.Fatal(e)
	}
}

func TestDiagnosticRunningCommandCancellation(t *testing.T) {
	t.Setenv("PULSE_DIAGNOSTIC_HELPER", "1")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	notified := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { notified = true; cancel() }))
	defer server.Close()
	t.Setenv("PULSE_DIAGNOSTIC_NOTIFY", server.URL)
	b := &Client{native: &fixtureHypervisor{binary: os.Args[0]}, cfg: config.Local{CommandTimeout: 10 * time.Second}}
	_, e := b.execute(ctx, "-test.run=^TestDiagnosticCommandHelper$", "--", "-1", "0")
	if !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	server.Close()
	if !notified {
		t.Fatal("command did not reach running state")
	}
}
