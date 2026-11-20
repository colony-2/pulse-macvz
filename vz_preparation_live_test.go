package tart

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"
)

// Preparation needs no host endpoint. Returning not-ready keeps ordinary pair
// reconciliation from configuring primaries or registering launchd services.
// This is only a source-cache gate, never evidence for paired qualification.
type preparationOnlyBackend struct{ *qualificationBackend }

func (b *preparationOnlyBackend) Endpoint(context.Context, string, string, int) (bool, error) {
	return false, nil
}

func TestLiveVZDockerPreparationCache(t *testing.T) {
	if os.Getenv("PULSE_TEST_VZ_DOCKER_PREPARE") != "1" {
		t.Skip("opt-in VZ source preparation")
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" || os.Getenv("PULSE_VZ_TEST_CACHE") == "" {
		t.Fatal("requires Apple Silicon and PULSE_VZ_TEST_CACHE")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Minute)
	defer cancel()
	q := newLiveQualification(t, ctx, "vz")
	defer q.cleanup()
	q.r.p.Backend = &preparationOnlyBackend{q.r.p.Backend.(*qualificationBackend)}
	q.prepareFixtureBase()
	waitLive(t, ctx, "prepared VZ bases and slot snapshots", 30*time.Minute, func(ctx context.Context) bool {
		q.reconcile(ctx)
		images, err := q.r.Images()
		if err != nil {
			t.Fatal(err)
		}
		slots, err := q.r.Slots()
		if err != nil {
			t.Fatal(err)
		}
		ready := len(slots) == 2 && len(images) >= 2
		for _, im := range images {
			if im.Phase == "failed" || im.Phase == "quarantine" {
				t.Fatal("source preparation failed", im.Name, im.Phase)
			}
			ready = ready && im.Phase == "ready"
		}
		for _, slot := range slots {
			if slot.Phase == "failed" || slot.Phase == "quarantine" {
				t.Fatal("slot preparation failed", slot.Template, slot.Phase)
			}
			ready = ready && slot.Phase == "ready"
		}
		return ready
	})
	q.cacheSources()
	t.Log("VZ_PREPARATION_CACHE_PASS: reusable bases and suspended templates; no paired qualification")
}
