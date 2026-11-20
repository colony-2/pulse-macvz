package servercli

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-provider-api/plugin"
)

type fakeRuntime struct {
	api.Provider
	checkErr, closeErr error
	run                func(context.Context) error
	closed             atomic.Int32
}

func (r *fakeRuntime) Check(context.Context) error   { return r.checkErr }
func (r *fakeRuntime) Run(ctx context.Context) error { return r.run(ctx) }
func (r *fakeRuntime) Close() error                  { r.closed.Add(1); return r.closeErr }

func TestFactoryLifetimeAndCleanup(t *testing.T) {
	r := &fakeRuntime{}
	stopped := make(chan struct{})
	r.run = func(ctx context.Context) error { <-ctx.Done(); close(stopped); return ctx.Err() }
	f := &providerFactory{open: func(serverOptions) (runtimeProvider, error) { return r, nil }, fail: func(err error) { t.Error(err) }}
	constructCtx, cancel := context.WithCancel(t.Context())
	_, cleanup, err := f.construct(constructCtx, plugin.Init{Lifetime: t.Context()})
	if err != nil {
		t.Fatal(err)
	}
	cancel() // Construction context must not own maintenance.
	select {
	case <-stopped:
		t.Fatal("construction cancellation stopped maintenance")
	default:
	}
	if err := cleanup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := cleanup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.closed.Load() != 1 {
		t.Fatal("provider must close once")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("cleanup did not wait for maintenance")
	}
}

func TestCheckFailureRegistersCleanup(t *testing.T) {
	failure := errors.New("unsigned runner")
	r := &fakeRuntime{checkErr: failure}
	f := &providerFactory{open: func(serverOptions) (runtimeProvider, error) { return r, nil }}
	p, cleanup, err := f.construct(t.Context(), plugin.Init{Lifetime: t.Context()})
	if p != nil || !errors.Is(err, failure) || cleanup == nil {
		t.Fatal("check failure lost cleanup")
	}
	if err := cleanup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.closed.Load() != 1 {
		t.Fatal("not closed")
	}
}

func TestFactoryRejectsUnsupportedInitializationBeforeOpening(t *testing.T) {
	f := &providerFactory{open: func(serverOptions) (runtimeProvider, error) { t.Fatal("opened runtime"); return nil, nil }}
	for _, in := range []plugin.Init{
		{Initialization: plugin.Initialization{ExecutorEnv: map[string]string{"UNKNOWN": "value"}}},
		{Initialization: plugin.Initialization{ConfigYAML: []byte("unknown: value")}},
	} {
		if _, _, err := f.construct(t.Context(), in); err == nil {
			t.Fatal("ignored unsupported initialization")
		}
	}
}

func TestFailedMaintenanceIsFatal(t *testing.T) {
	failure := errors.New("inventory failed")
	failed := make(chan error, 1)
	r := &fakeRuntime{run: func(context.Context) error { return failure }}
	f := &providerFactory{open: func(serverOptions) (runtimeProvider, error) { return r, nil }, fail: func(err error) { failed <- err }}
	_, cleanup, err := f.construct(t.Context(), plugin.Init{Lifetime: t.Context()})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-failed:
		if !errors.Is(err, failure) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("failure not supervised")
	}
	if err := cleanup(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupTimeoutPreventsOverlappingReplacement(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	r := &fakeRuntime{run: func(context.Context) error { <-release; return nil }}
	f := &providerFactory{open: func(serverOptions) (runtimeProvider, error) { return r, nil }, fail: func(error) {}}
	_, cleanup, err := f.construct(t.Context(), plugin.Init{Lifetime: t.Context()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := cleanup(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unbounded cleanup: %v", err)
	}
	if r.closed.Load() != 0 {
		t.Fatal("closed provider before maintenance stopped")
	}
	if _, _, err := f.construct(t.Context(), plugin.Init{Lifetime: t.Context()}); err == nil {
		t.Fatal("replacement overlapped incomplete cleanup")
	}
}
