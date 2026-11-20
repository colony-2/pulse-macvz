package local

import (
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
	err := b.ObserveBasePreparation(t.Context(), spec.Name, r.UID, "15.7.7")
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatal(err)
	}
	if b.workers[spec.Name] {
		t.Fatal("replayed expired startup")
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
