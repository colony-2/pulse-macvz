package local

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse-macvz/internal/backend"
)

func TestStoppedDeletionCancelsDeliveryAndDefersUntilWorkerExits(t *testing.T) {
	b, m, spec := fixture(t)
	entered := make(chan struct{})
	b.native.(*fixtureHypervisor).command = func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] == "ip" {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return m.run(ctx, args...)
	}
	v, err := b.Create(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("delivery did not start")
	}
	if err := b.Delete(t.Context(), spec.Name); err == nil || errors.Is(err, backend.ErrBusy) {
		t.Fatal("running guest must not be treated as deferred deletion", err)
	}
	b.mu.Lock()
	if !b.workers[spec.Name] {
		t.Fatal("running guest delivery was cancelled")
	}
	b.mu.Unlock()
	if err := b.native.Stop(t.Context(), spec.Name); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(t.Context(), spec.Name); !errors.Is(err, backend.ErrBusy) || !strings.Contains(err.Error(), v.UID) || !strings.Contains(err.Error(), spec.Name) {
		t.Fatal("missing deferred cleanup identity", err)
	}
	waitDelivery(t, b, spec.Name)
	if err := b.DeliveryHealth(t.Context()); err != nil {
		t.Fatal("expected cleanup cancellation became a delivery failure", err)
	}
	if err := b.Delete(t.Context(), spec.Name); err != nil {
		t.Fatal("cleanup did not recover", err)
	}
}
