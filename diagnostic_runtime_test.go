package tart_test

import (
	"context"
	"encoding/base64"
	df "github.com/colony-2/pulse-tart/tests/integration/diagnosticfixture"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	api "github.com/colony-2/pulse-provider-api"
)

func TestTartDiagnosticRuntime(t *testing.T) {
	for _, scenario := range []string{"inventory", "mixed", "preparation", "create", "accepted", "websocket", "handshake", "session", "readiness-error", "readiness-marker", "handoff-error", "handoff-marker", "docker-accepted"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			docker := strings.Contains(scenario, "readiness") || strings.Contains(scenario, "handoff") || scenario == "websocket" || scenario == "handshake" || scenario == "session" || scenario == "docker-accepted"
			f := df.NewTartDiagnosticFixture(t, docker)
			if scenario == "preparation" {
				f.Scenario(scenario)
				e := f.Runtime.Reconcile(ctx)
				if e == nil || !strings.Contains(e.Error(), df.TartDiagnosticPreparation) || !strings.Contains(e.Error(), "503") {
					t.Fatal("real preparation error lost", e)
				}
				f.Scenario("")
			}
			if e := f.Ready(ctx); e != nil {
				t.Fatal(e)
			}
			beforeDiscovery := f.Snapshot()
			if _, e := f.Runtime.GetCapabilities(ctx); e != nil {
				t.Fatal(e)
			}
			if !reflect.DeepEqual(beforeDiscovery, f.Snapshot()) {
				t.Fatal("discovery probed backend")
			}
			if scenario == "mixed" {
				if e := f.Mixed(ctx); e != nil {
					t.Fatal(e)
				}
			}
			before := f.Snapshot()
			f.Scenario(scenario)
			l := f.Launch("fixture-current")
			out, e := f.Runtime.Submit(ctx, []api.Launch{l})
			if e != nil || len(out) != 1 {
				t.Fatal(out, e)
			}
			r := out[0]
			want := api.Unavailable
			cause := ""
			switch scenario {
			case "inventory":
				cause = df.TartDiagnosticInventory
			case "mixed":
				want = api.NoCapacity
				for _, part := range []string{"reservation occupied", "source-referencing clone", "suspension invalid; quarantined"} {
					if !strings.Contains(r.Reason, part) {
						t.Errorf("missing %s in %q", part, r.Reason)
					}
				}
			case "create":
				want = api.Unknown
				cause = df.TartDiagnosticCreate
			case "accepted", "preparation", "docker-accepted":
				want = api.Accepted
			case "handoff-error", "handoff-marker":
				want = api.Unknown
			}
			if r.Status != want || cause != "" && !strings.Contains(r.Reason, cause) {
				t.Fatal(r)
			}
			if want == api.Accepted && r.Reason != "" {
				t.Fatal("accepted reason", r)
			}
			if scenario == "readiness-error" || scenario == "handoff-error" {
				if !strings.Contains(r.Reason, "Process exited with status 23") {
					t.Fatal("SSH exit cause lost", r)
				}
			}
			if scenario == "readiness-marker" && !strings.Contains(r.Reason, "missing PULSE_DOCKER_PRIMARY_READY") {
				t.Fatal(r)
			}
			if scenario == "handoff-marker" && !strings.Contains(r.Reason, "missing PULSE_HANDOFF_OK") {
				t.Fatal(r)
			}
			if scenario == "session" && !strings.Contains(r.Reason, "fixture session denied") {
				t.Fatal(r)
			}
			after := f.Snapshot()
			posts := after["POST job"] - before["POST job"]
			if scenario == "create" || scenario == "accepted" || scenario == "preparation" {
				if posts != 1 {
					t.Fatal("POST count", posts)
				}
			} else if posts != 0 {
				t.Fatal("unexpected job create", posts)
			}
			if want == api.Unknown || want == api.Accepted {
				beforeReplay := f.Snapshot()
				replay, e := f.Runtime.Submit(ctx, []api.Launch{l})
				if e != nil || len(replay) != 1 || replay[0].Status != want {
					t.Fatal(replay, e)
				}
				if !reflect.DeepEqual(beforeReplay, f.Snapshot()) {
					t.Fatal("replay called backend")
				}
				if want == api.Accepted && replay[0].Reason != "" {
					t.Fatal(replay)
				}
			}
			for _, secret := range []string{string(l.Lease), base64.StdEncoding.EncodeToString([]byte(l.Lease)), "fixture unrelated output", "PULSE_ENVELOPE"} {
				if strings.Contains(r.Reason, secret) || strings.Contains(strings.Join(f.Transcript(), "\n"), secret) {
					t.Fatal("payload captured")
				}
			}
			// No diagnostic history is written into the authenticated journal.
			journal, e := os.ReadFile(filepath.Join(f.Config.StateDir, "state.json"))
			if e != nil {
				t.Fatal(e)
			}
			if cause != "" && strings.Contains(string(journal), cause) {
				t.Fatal("persisted cause")
			}
			beforeClose := f.Snapshot()
			if e = f.Close(); e != nil {
				t.Fatal(e)
			}
			afterClose := f.Snapshot()
			for k, n := range afterClose {
				if strings.HasPrefix(k, "DELETE ") && n != beforeClose[k] {
					t.Fatal("close deleted resource")
				}
			}
		})
	}
}
