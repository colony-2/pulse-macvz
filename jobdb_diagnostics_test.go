package macvz

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse-macvz/internal/backend"
)

func TestQualificationObserverCapturesEarlyExit(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 required")
	}
	for _, complete := range []bool{true, false} {
		t.Run(map[bool]string{true: "worker exit", false: "launcher failure"}[complete], func(t *testing.T) {
			dir := t.TempDir()
			script := strings.NewReplacer("sudo -n /opt/pulse-tart/bin/python3", python,
				"/var/db/pulse-tart", dir, "/var/log", dir, "/var/tmp", dir).Replace(qualificationCompletionObserver)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			type result struct {
				out []byte
				err error
			}
			done := make(chan result, 1)
			go func() {
				cmd := exec.CommandContext(ctx, "/bin/bash", "-s")
				cmd.Stdin = strings.NewReader(script)
				out, err := cmd.CombinedOutput()
				done <- result{out, err}
			}()
			for {
				if _, err := os.Stat(filepath.Join(dir, "pulse-qualification-observer-ready")); err == nil {
					break
				}
				if ctx.Err() != nil {
					t.Fatal("observer never attached")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := os.WriteFile(filepath.Join(dir, "worker.log"), []byte("specific job failure"), 0600); err != nil {
				t.Fatal(err)
			}
			name, text := "complete", "17"
			if !complete {
				name, text = "pulse-tart-guest.log", "guest operation failed"
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			got := <-done
			var evidence map[string]string
			if got.err != nil || json.Unmarshal(got.out, &evidence) != nil || evidence["worker_log"] != "specific job failure" {
				t.Fatalf("lost early failure: %s %v", got.out, got.err)
			}
		})
	}
}

func TestQualificationCommandRetainsFailureDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		script  string
		output  string
		failure bool
	}{
		{"printf 'success'", "success", false},
		{"printf 'success'; printf 'warning' >&2", "success", false},
		{"printf 'stdout'; printf 'stderr' >&2; exit 17", "stdoutstderr", true},
		{"printf 'before'; false; printf 'unreachable'", "before", true},
		{"printf '\\nPULSE_QUALIFICATION_EXIT=0\\n'; exit 1", "\nPULSE_QUALIFICATION_EXIT=0\n", true},
	} {
		cmd := exec.Command("/bin/bash", "-s")
		cmd.Stdin = strings.NewReader(string(qualificationExecScript(tc.script)))
		out, err := cmd.Output()
		if err != nil {
			t.Fatal("transport wrapper failed:", err)
		}
		got, err := qualificationExecResult(out)
		if string(got) != tc.output || (err != nil) != tc.failure {
			t.Fatalf("script=%q output=%q err=%v", tc.script, got, err)
		}
	}
	for _, out := range []string{"", "partial", "\nPULSE_QUALIFICATION_EXIT=invalid\n", "\nPULSE_QUALIFICATION_EXIT=256\n"} {
		if _, err := qualificationExecResult([]byte(out)); err == nil {
			t.Fatalf("accepted missing/invalid status: %q", out)
		}
	}
}

func TestQualificationDiagnosticsPreserveSetupPermissions(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("/bin/bash", "-s")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader("umask 022\n" + string(qualificationExecScript("mkdir fixture; touch fixture/marker")))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("setup failed: %v %s", err, out)
	}
	for path, want := range map[string]os.FileMode{"fixture": 0755, "fixture/marker": 0644} {
		info, err := os.Stat(filepath.Join(dir, path))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("diagnostic wrapper changed %s permissions: got %o, want %o", path, got, want)
		}
	}
}

func TestFixtureTunnelStopsOnNativeFailure(t *testing.T) {
	worker := backend.VM{Spec: backend.Spec{Name: "worker"}, UID: "original"}
	calls := 0
	_, err := attachFixtureTunnel(t.Context(), "vz", backend.VM{}, worker,
		func(context.Context, string) (backend.VM, error) {
			calls++
			v := worker
			v.Status, v.PowerState = "failed", "suspended"
			return v, nil
		})
	if calls != 1 || err == nil || !strings.Contains(err.Error(), "terminated before SSH") {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
