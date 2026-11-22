package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBaseObservationDetectsRebootsWithoutTreatingLogGrowthAsSuccess(t *testing.T) {
	s := &basePreparation{}
	for i := 0; i < 4; i++ {
		data := []byte(fmt.Sprintf("PULSE_BASE_OBSERVATION 00000000-0000-0000-0000-%012d preparing %d\n", i, i*100))
		if err := s.observe(data, time.Now()); err != nil {
			t.Fatal(err)
		}
		if s.reboots != i || s.logBytes != int64(i*100) {
			t.Fatal(s)
		}
		if (s.err != nil) != (i == 3) {
			t.Fatal("incorrect reboot threshold", s)
		}
		// Re-observing the same boot does not count another restart.
		if err := s.observe(data, time.Now()); err != nil || s.reboots != i {
			t.Fatal(s, err)
		}
	}
	if s.cause != "guest_reboot_loop" || s.stage != "base_failed" {
		t.Fatal(s)
	}
}

func TestBaseObservationReadyAndFailedAreExplicit(t *testing.T) {
	for _, state := range []string{"ready", "failed", "absent", "preparing"} {
		s := &basePreparation{}
		if err := s.observe([]byte("PULSE_BASE_OBSERVATION 00000000-0000-0000-0000-000000000001 "+state+" 42"), time.Now()); err != nil {
			t.Fatal(err)
		}
		if (s.err != nil) != (state == "failed") || s.stage != "base_"+state {
			t.Fatal(s)
		}
	}
	for _, input := range []string{
		"SECRET_GUEST_OUTPUT", "PULSE_BASE_OBSERVATION 00000000-0000-0000-0000-000000000001 ready -1",
		"PULSE_BASE_OBSERVATION 00000000-0000-0000-0000-00000000000z ready 1",
		"PULSE_BASE_OBSERVATION 00000000-0000-0000-0000-000000000001 unknown 1",
		"PULSE_BASE_OBSERVATION 00000000-0000-0000-0000-000000000001 ready 1 EXTRA",
	} {
		_, _, _, err := parseBaseObservation([]byte(input))
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatal(err)
		}
	}
}

func TestBasePreparationDeadlineSurvivesControllerRestart(t *testing.T) {
	b, m, spec := fixture(t)
	if _, err := b.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	b.Close()
	r := b.records[spec.Name]
	r.Created = time.Now().Add(-2 * time.Hour)
	r.Delivery.ExecutionPossible = true
	if err := b.save(r); err != nil {
		t.Fatal(err)
	}
	b = reopenProperties(t, b, m, "")
	if err := b.ObserveBasePreparation(t.Context(), spec.Name, "wrong-uid", "15.7.7"); err == nil {
		t.Fatal("observed wrong VM")
	}
	if err := b.ObserveBasePreparation(t.Context(), spec.Name, r.UID, "15.7.7"); err != nil {
		t.Fatal("expired builder must still be observed", err)
	}
	b.wg.Wait()
	err := b.ObserveBasePreparation(t.Context(), spec.Name, r.UID, "15.7.7")
	if err == nil || !strings.Contains(err.Error(), "deadline expired") {
		t.Fatal(err)
	}
	if b.workers[spec.Name] {
		t.Fatal("replayed expired startup")
	}
}

func TestExpiredBaseRecoveryObservesBeforeTimeoutWithoutReplayingPreparation(t *testing.T) {
	for _, result := range []string{"ready", "preparing", "absent", "failed", "unreachable"} {
		t.Run(result, func(t *testing.T) {
			b, m, spec := fixture(t)
			if _, err := b.Create(t.Context(), spec); err != nil {
				t.Fatal(err)
			}
			b.Close()
			r := b.records[spec.Name]
			r.Created = time.Now().Add(-2 * time.Hour)
			r.Delivery.ExecutionPossible = true
			if err := b.save(r); err != nil {
				t.Fatal(err)
			}
			b = reopenProperties(t, b, m, "")
			r = b.records[spec.Name]
			s := &basePreparation{uid: r.UID}
			observed, shutdown := 0, 0
			exec := func(ctx context.Context, name, uid string, script []byte) ([]byte, error) {
				if ctx.Err() != nil || name != spec.Name || uid != r.UID {
					t.Fatal("invalid recovery target/context", name, uid, ctx.Err())
				}
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 15*time.Second {
					t.Fatal("recovery command must be bounded")
				}
				switch string(script) {
				case "expected='15.7.7'\n" + observeMacBase + observeImageVersion:
					observed++
					if result == "unreachable" {
						return nil, errors.New("guest unavailable")
					}
					return []byte("PULSE_BASE_OBSERVATION 00000000-0000-0000-0000-000000000001 " + result + " 42\nPULSE_C2J 123abcd v1.2.3\n"), nil
				case shutdownReadyMacBase:
					shutdown++
					v, err := ReadImageVersion(b.path(name))
					if err != nil || v.Version != "v1.2.3" {
						t.Fatal("shutdown preceded durable version", v, err)
					}
					data, err := os.ReadFile(b.artifact(name, "events"))
					if err != nil || !strings.Contains(string(data), "PULSE_BASE_READY") || !b.deliveryMarker(name) {
						t.Fatal("shutdown preceded durable readiness", err)
					}
					return nil, nil
				default:
					t.Fatal("replayed startup or unexpected command")
					return nil, nil
				}
			}
			b.wg.Add(1)
			b.monitorBase(spec.Name, r, s, "15.7.7", r.Created.Add(b.timeout), exec)
			if observed != 1 || !s.done {
				t.Fatal("expired recovery did not perform exactly one observation", observed, s)
			}
			if result == "ready" {
				if shutdown != 1 || s.err != nil || !r.Delivered {
					t.Fatal("completed builder was not recovered", shutdown, s)
				}
				b = reopenProperties(t, b, m, "")
				if !b.records[spec.Name].Delivered || !b.deliveryMarker(spec.Name) {
					t.Fatal("recovered readiness lost on restart")
				}
			} else if shutdown != 0 || s.err == nil || s.stage != "base_failed" {
				t.Fatal("unfinished builder received a new budget or was shut down", shutdown, s)
			}
		})
	}
}

func TestObservedBaseReadinessIsDurableWithoutStartupReplay(t *testing.T) {
	b, m, spec := fixture(t)
	if _, err := b.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	b.Close()
	b = reopenProperties(t, b, m, "")
	r := b.records[spec.Name]
	b.mu.Lock()
	err := b.recordBaseReady(r)
	b.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	b = reopenProperties(t, b, m, "")
	if !b.records[spec.Name].Delivered || !b.deliveryMarker(spec.Name) {
		t.Fatal("lost observed readiness")
	}
	data, err := os.ReadFile(b.artifact(spec.Name, "events"))
	if err != nil || string(data) != "PULSE_BASE_READY\n" {
		t.Fatal(string(data), err)
	}
	if strings.Contains(observeMacBase, "prepare-base.sh") || strings.Contains(observeMacBase, "shutdown") {
		t.Fatal("readiness observation must not start work or shut down before host persistence")
	}
}

func TestLinuxBaseRecoveryObservesServiceWithoutReplayingBuild(t *testing.T) {
	for _, state := range []string{"ready", "failed"} {
		t.Run(state, func(t *testing.T) {
			b, _, spec := fixture(t)
			if _, err := b.Create(t.Context(), spec); err != nil {
				t.Fatal(err)
			}
			b.Close()
			r := b.records[spec.Name]
			r.Spec.OS = "linux"
			b.closed = false
			b.ctx = t.Context()
			s := &basePreparation{uid: r.UID}
			observed, stopped := 0, 0
			exec := func(_ context.Context, name, uid string, script []byte) ([]byte, error) {
				if name != spec.Name || uid != r.UID {
					t.Fatal("wrong recovery identity")
				}
				switch string(script) {
				case "expected='24.04'\n" + observeLinuxBase + observeImageVersion:
					observed++
					return []byte("PULSE_BASE_OBSERVATION 00000000-0000-0000-0000-000000000001 " + state + " 100\nPULSE_C2J 123abcd v1\n"), nil
				case shutdownReadyLinuxBase:
					stopped++
					if !r.Delivered || !b.deliveryMarker(name) {
						t.Fatal("shutdown before readiness persisted")
					}
					return nil, nil
				default:
					t.Fatal("replayed startup instead of observing")
					return nil, nil
				}
			}
			b.wg.Add(1)
			b.monitorBase(spec.Name, r, s, "24.04", time.Now().Add(time.Minute), exec)
			if observed != 1 || !s.done {
				t.Fatal("service observation did not finish", s)
			}
			if state == "ready" {
				if stopped != 1 || s.err != nil {
					t.Fatal("ready Linux base not recovered", s)
				}
			} else if stopped != 0 || s.err == nil || r.Delivered {
				t.Fatal("failed build incorrectly accepted or stopped", s)
			}
		})
	}
}
