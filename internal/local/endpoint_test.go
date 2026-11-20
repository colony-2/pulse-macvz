package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEngineEndpointRequiresUpstreamSSH(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		wantError   bool
	}{
		{"reachable engine", "SSH-2.0-OpenSSH_9.6\r\n", false},
		{"bridge closes denied upstream", "", true},
		{"wrong service", "HTTP/1.1 200 OK\r\n", true},
		{"incomplete banner", "SSH-2.0-incomplete", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			go func() {
				conn, err := listener.Accept()
				if err == nil {
					defer conn.Close()
					io.WriteString(conn, tc.reply)
				}
			}()
			err = probeEngineEndpoint(t.Context(), listener.Addr().(*net.TCPAddr).Port)
			if (err != nil) != tc.wantError {
				t.Fatalf("probe = %v; want error %t", err, tc.wantError)
			}
		})
	}
}

func TestEngineEndpointProbeHonorsCancellation(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			cancel()
			io.Copy(io.Discard, conn)
		}
	}()
	start := time.Now()
	if err := probeEngineEndpoint(ctx, listener.Addr().(*net.TCPAddr).Port); err == nil {
		t.Fatal("stalled upstream was ready")
	}
	if time.Since(start) > time.Second {
		t.Fatal("probe did not honor cancellation")
	}
}

func missingService(t *testing.T) error {
	t.Helper()
	return exec.Command("/bin/sh", "-c", "exit 113").Run()
}

func TestEndpointBootstrapFailureIsActionableAndArchived(t *testing.T) {
	b, _, _ := fixture(t)
	missing := missingService(t)
	calls := 0
	b.service = func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		switch args[0] {
		case "print":
			return nil, missing
		case "bootstrap":
			data, err := os.ReadFile(args[2])
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "<key>LimitLoadToSessionType</key><string>Background</string>") {
				t.Fatal("user-domain helper lacks Background session type")
			}
			return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
		}
		t.Fatalf("unexpected launchctl: %v", args)
		return nil, nil
	}
	ready, err := b.ensureEndpoint(t.Context(), "owned", "127.0.0.1:2222", 45100)
	if ready || !errors.Is(err, Err) || !strings.Contains(err.Error(), "Bootstrap failed: 5") {
		t.Fatal(ready, err)
	}
	dir := filepath.Join(b.dir, "endpoints", "owned")
	if calls != 2 {
		t.Fatal("unexpected service mutations", calls)
	}
	if e := os.WriteFile(filepath.Join(dir, "private-key"), []byte("do-not-export"), 0600); e != nil {
		t.Fatal(e)
	}
	b.service = func(context.Context, ...string) ([]byte, error) {
		return []byte("\tstate = waiting\n\tenvironment = {\n\t\tTOKEN = do-not-export\n\t\tstate = do-not-export\n\t}\n\tlast exit code = 78\n"), nil
	}
	diagnostics, err := b.EndpointDiagnostics(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(diagnostics)
	if strings.Contains(string(encoded), "do-not-export") || !strings.Contains(diagnostics["owned"]["last-error.log"], "Bootstrap failed: 5") {
		t.Fatal("diagnostics missing failure or leaked private data")
	}
}

func TestEndpointObservationFailureDoesNotBootstrapOrRelease(t *testing.T) {
	b, _, _ := fixture(t)
	missing := missingService(t)
	calls := []string{}
	b.service = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args[0])
		return nil, context.Canceled
	}
	if _, err := b.ensureEndpoint(t.Context(), "owned", "127.0.0.1:2222", 45100); err == nil {
		t.Fatal("observation failure treated as absence")
	}
	if err := b.RemoveEndpoint(t.Context(), "engine", "owned"); err == nil {
		t.Fatal("observation failure released helper")
	}
	if strings.Join(calls, ",") != "print,print" {
		t.Fatal(calls)
	}
	if _, err := os.Stat(filepath.Join(b.dir, "endpoints", "owned")); err != nil {
		t.Fatal("uncertain service state removed")
	}
	b.service = func(context.Context, ...string) ([]byte, error) { return nil, missing }
	if err := b.RemoveEndpoint(t.Context(), "engine", "owned"); err != nil {
		t.Fatal(err)
	}
}

func TestEndpointMovesExistingHelperBesideVM(t *testing.T) {
	b, _, spec := fixture(t)
	v, err := b.Create(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	calls := []string{}
	missing := missingService(t)
	removed := false
	b.service = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args[0])
		if args[0] == "bootout" {
			removed = true
		}
		if args[0] == "print" && removed {
			return nil, missing
		}
		if args[0] == "bootstrap" {
			removed = false
			want := filepath.Join(b.path(spec.Name), "pulse-endpoint", "service.plist")
			if args[2] != want {
				t.Fatal("helper not moved into VM directory", args[2])
			}
		}
		return nil, nil
	}
	ready, err := b.ensureEndpoint(t.Context(), v.UID, "127.0.0.1:2222", 45100)
	if ready || err != nil || strings.Join(calls, ",") != "print,bootout,print,bootstrap" {
		t.Fatal(ready, err, calls)
	}
	// A new helper with its plist present may still be starting; do not restart
	// merely because it has not written readiness yet.
	calls = nil
	if _, err = b.ensureEndpoint(t.Context(), v.UID, "127.0.0.1:2222", 45100); err != nil || strings.Join(calls, ",") != "print" {
		t.Fatal(err, calls)
	}
}

func TestEndpointRemovalWaitsForConfirmedAbsence(t *testing.T) {
	missing := missingService(t)
	noProcess := exec.Command("/bin/sh", "-c", "exit 3").Run()
	for _, bootErr := range []error{nil, noProcess, missing} {
		t.Run(fmt.Sprint(bootErr), func(t *testing.T) {
			b, _, _ := fixture(t)
			dir := b.endpointDir("owned")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			prints, stops := 0, 0
			b.service = func(_ context.Context, args ...string) ([]byte, error) {
				if _, err := os.Stat(dir); err != nil {
					t.Fatal("released state before confirmed absence", err)
				}
				switch args[0] {
				case "print":
					prints++
					if prints >= 3 {
						return nil, missing
					}
					return []byte("state = exiting"), nil
				case "bootout":
					stops++
					return []byte("Boot-out failed: 3: No such process"), bootErr
				}
				t.Fatalf("unexpected launchctl %v", args)
				return nil, nil
			}
			if err := b.RemoveEndpoint(t.Context(), "engine", "owned"); err != nil {
				t.Fatal(err)
			}
			if stops != 1 || prints != 3 {
				t.Fatal("did not wait for disappearance", stops, prints)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("state retained after confirmed removal", err)
			}
			b.service = func(_ context.Context, args ...string) ([]byte, error) {
				if args[0] != "print" {
					t.Fatalf("repeated removal mutated service: %v", args)
				}
				return nil, missing
			}
			if err := b.RemoveEndpoint(t.Context(), "engine", "owned"); err != nil {
				t.Fatal("repeated removal", err)
			}
		})
	}
}

func TestEndpointRemovalRetainsUncertainState(t *testing.T) {
	noProcess := exec.Command("/bin/sh", "-c", "exit 3").Run()
	denied := exec.Command("/bin/sh", "-c", "exit 5").Run()
	for _, name := range []string{"still present", "observation denied", "bootout denied"} {
		t.Run(name, func(t *testing.T) {
			b, _, _ := fixture(t)
			dir := b.endpointDir("owned")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			prints := 0
			b.service = func(_ context.Context, args ...string) ([]byte, error) {
				if args[0] == "bootout" {
					if name == "bootout denied" {
						return nil, denied
					}
					return nil, noProcess
				}
				prints++
				if prints > 1 && name == "observation denied" {
					return nil, denied
				}
				return nil, nil
			}
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
			defer cancel()
			err := b.RemoveEndpoint(ctx, "engine", "owned")
			if err == nil {
				t.Fatal("uncertain removal succeeded")
			}
			if name == "still present" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			if _, err := os.Stat(dir); err != nil {
				t.Fatal("uncertain state deleted", err)
			}
		})
	}
}
