package local

import (
	"context"
	"errors"
	"testing"
	"time"
)

type preparingHypervisor struct {
	Hypervisor
	prepare func(context.Context, string) error
}

func (h preparingHypervisor) PrepareSource(ctx context.Context, source string) error {
	return h.prepare(ctx, source)
}

func TestSourceImportDoesNotBlockDeliveryOrInventory(t *testing.T) {
	b, _, spec := fixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	b.native = preparingHypervisor{b.hypervisor(), func(ctx context.Context, _ string) error {
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	call, cancel := context.WithCancel(t.Context())
	ready, err := b.PrepareSource(call, spec.Image)
	if err != nil || ready {
		t.Fatal(ready, err)
	}
	cancel() // Returning from a reconciliation must not cancel its import.
	<-started
	if ready, err = b.PrepareSource(t.Context(), spec.Image); err != nil || ready {
		t.Fatal("pending import was not shared", ready, err)
	}
	r := &record{Spec: spec, Delivery: &DeliveryStatus{StartedAt: time.Now()}}
	b.records[spec.Name] = r
	advanced := make(chan error, 1)
	go func() { advanced <- b.deliveryStage(r, "tcp") }()
	select {
	case err := <-advanced:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("source import blocked guest delivery")
	}
	ctx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	if _, err := b.List(ctx); err != nil {
		t.Fatal("source import blocked inventory", err)
	}
	close(release)
	b.wg.Wait()
	if ready, err = b.PrepareSource(t.Context(), spec.Image); err != nil || !ready {
		t.Fatal("completed import not ready", ready, err)
	}
}

func TestSourceImportFailureIsReportedAndRetryable(t *testing.T) {
	b, _, spec := fixture(t)
	failure := errors.New("download interrupted")
	attempts := 0
	b.native = preparingHypervisor{b.hypervisor(), func(context.Context, string) error {
		attempts++
		if attempts == 1 {
			return failure
		}
		return nil
	}}
	_, _ = b.PrepareSource(t.Context(), spec.Image)
	b.wg.Wait()
	if ready, err := b.PrepareSource(t.Context(), spec.Image); ready || !errors.Is(err, failure) {
		t.Fatal("import failure hidden", ready, err)
	}
	_, _ = b.PrepareSource(t.Context(), spec.Image)
	b.wg.Wait()
	if ready, err := b.PrepareSource(t.Context(), spec.Image); !ready || err != nil || attempts != 2 {
		t.Fatal("retry failed", ready, err, attempts)
	}
}

func TestCloseCancelsAndJoinsSourceImports(t *testing.T) {
	b, _, spec := fixture(t)
	started, stopped := make(chan struct{}), make(chan struct{})
	b.native = preparingHypervisor{b.hypervisor(), func(ctx context.Context, _ string) error {
		close(started)
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	}}
	_, _ = b.PrepareSource(t.Context(), spec.Image)
	<-started
	closed := make(chan struct{})
	go func() { _ = b.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel import")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("Close returned before import stopped")
	}
	if ready, err := b.PrepareSource(t.Context(), spec.Image); ready || err == nil {
		t.Fatal("closed client restarted an import")
	}
}
