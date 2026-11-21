package servercli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse-provider-api/plugin"
)

func retryTestOptions() serverOptions {
	return serverOptions{transport: transportOptions{reconnect: plugin.RestartPolicy{InitialDelay: time.Millisecond, MaxDelay: 8 * time.Millisecond, Multiplier: 2, HealthyResetInterval: time.Minute}}}
}

func TestReverseBackoffPlateausAndResets(t *testing.T) {
	o := retryTestOptions()
	o.transport.reconnect.InitialDelay = time.Second
	o.transport.reconnect.MaxDelay = 5 * time.Minute
	b := newReconnectBackoff(o.transport)
	b.Reset()
	for i, want := range []time.Duration{1, 2, 4, 8, 16, 32, 64, 128, 256, 300, 300} {
		if got := b.NextBackOff(); got != want*time.Second {
			t.Fatalf("step %d: %v want %v", i, got, want*time.Second)
		}
	}
	b.Reset()
	if got := b.NextBackOff(); got != time.Second {
		t.Fatal("reset failed", got)
	}
	b.RandomizationFactor = .2
	for range 1000 {
		if got := b.NextBackOff(); got <= 0 || got > 5*time.Minute {
			t.Fatal("jitter exceeded maximum", got)
		}
	}
}

func TestReverseRetriesAllSessionOutcomesWithoutLimit(t *testing.T) {
	o := retryTestOptions()
	var logs bytes.Buffer
	o.log = &logs
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	attempts := 0
	results := []error{nil, plugin.ErrProtocol, plugin.ErrUnavailable, errors.New("initialization failed"), errors.New("reconciliation failed")}
	err := superviseReverse(ctx, o, func(**providerFactory) error {
		attempts++
		if attempts == 16 {
			cancel()
		}
		return results[(attempts-1)%len(results)]
	})
	if err != nil || attempts != 16 {
		t.Fatal("finite retry limit", attempts, err)
	}
	if strings.Count(logs.String(), "retry in") != 15 || !strings.Contains(logs.String(), "collector ended the session") {
		t.Fatal("retry reasons/delays missing", logs.String())
	}
}

func TestReverseHealthySessionResetsBackoff(t *testing.T) {
	o := retryTestOptions()
	var logs bytes.Buffer
	o.log = &logs
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	attempts := 0
	err := superviseReverse(ctx, o, func(previous **providerFactory) error {
		attempts++
		if attempts == 3 {
			done := make(chan struct{})
			close(done)
			*previous = &providerFactory{active: &providerSession{closeDone: done, started: time.Now().Add(-time.Minute), stopped: time.Now()}}
		}
		if attempts == 5 {
			cancel()
		}
		return plugin.ErrUnavailable
	})
	if err != nil {
		t.Fatal(err)
	}
	for attempt, delay := range []string{"1ms", "2ms", "1ms", "2ms"} {
		want := fmt.Sprintf("attempt=%d reason=", attempt+1)
		found := false
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(line, want) && strings.HasSuffix(line, "retry in "+delay) {
				found = true
			}
		}
		if !found {
			t.Fatal("backoff did not reset after healthy session", logs.String())
		}
	}
}

type retrySignalWriter struct{ ch chan struct{} }

func (w retrySignalWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("retry in")) {
		select {
		case w.ch <- struct{}{}:
		default:
		}
	}
	return len(p), nil
}

func TestReverseCancellationInterruptsFiveMinuteWait(t *testing.T) {
	o := retryTestOptions()
	o.transport.reconnect.InitialDelay = 5 * time.Minute
	o.transport.reconnect.MaxDelay = 5 * time.Minute
	w := retrySignalWriter{make(chan struct{}, 1)}
	o.log = w
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- superviseReverse(ctx, o, func(**providerFactory) error { return plugin.ErrUnavailable })
	}()
	select {
	case <-w.ch:
	case <-time.After(time.Second):
		t.Fatal("no backoff")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation waited for backoff")
	}
}

func TestReverseWaitsForOldRuntimeCleanup(t *testing.T) {
	o := retryTestOptions()
	w := retrySignalWriter{make(chan struct{}, 1)}
	o.log = w
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	oldDone := make(chan struct{})
	called := make(chan struct{}, 3)
	done := make(chan error, 1)
	go func() {
		attempt := 0
		done <- superviseReverse(ctx, o, func(previous **providerFactory) error {
			attempt++
			called <- struct{}{}
			if attempt == 1 {
				*previous = &providerFactory{active: &providerSession{closeDone: oldDone}}
			} else {
				cancel()
			}
			return plugin.ErrCleanup
		})
	}()
	<-called
	for range 3 {
		select {
		case <-w.ch:
		case <-time.After(time.Second):
			t.Fatal("stopped retrying while cleanup pending")
		}
	}
	select {
	case <-called:
		t.Fatal("opened replacement before cleanup")
	default:
	}
	close(oldDone)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("did not resume after cleanup")
	}
}
