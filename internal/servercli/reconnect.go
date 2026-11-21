package servercli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cenkalti/backoff/v7"
)

var collectorStopped = errors.New("collector ended the session")

// ExponentialBackOff caps its nominal interval, not jitter. Enforce the user's
// maximum on the actual wait as well.
type reconnectBackoff struct {
	*backoff.ExponentialBackOff
	maximum time.Duration
}

func newReconnectBackoff(o transportOptions) *reconnectBackoff {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = o.reconnect.InitialDelay
	b.MaxInterval = o.reconnect.MaxDelay
	b.Multiplier = o.reconnect.Multiplier
	b.RandomizationFactor = o.reconnect.JitterFraction
	return &reconnectBackoff{ExponentialBackOff: b, maximum: o.reconnect.MaxDelay}
}

func (b *reconnectBackoff) NextBackOff() time.Duration {
	return min(b.ExponentialBackOff.NextBackOff(), b.maximum)
}

func runReverse(ctx context.Context, o serverOptions, open runtimeOpener) error {
	return superviseReverse(ctx, o, func(previous **providerFactory) error {
		return runPluginSession(ctx, o, open, previous)
	})
}

// No operation here resubmits a launch. Each attempt authenticates and creates
// a fresh runtime, only after the previous runtime has completed cleanup.
func superviseReverse(ctx context.Context, o serverOptions, session func(**providerFactory) error) error {
	b := newReconnectBackoff(o.transport)
	var previous *providerFactory
	attempts := uint64(0)
	_, err := backoff.Retry(ctx, func() (struct{}, error) {
		if ctx.Err() != nil {
			return struct{}{}, ctx.Err()
		}
		if previous != nil {
			if err := previous.reconnectReady(); err != nil {
				return struct{}{}, err
			}
			previous = nil
		}
		attempts++
		err := session(&previous)
		if previous != nil && o.transport.reconnect.HealthyResetInterval > 0 && previous.healthyDuration() >= o.transport.reconnect.HealthyResetInterval {
			b.Reset()
		}
		if err == nil {
			err = collectorStopped
		}
		return struct{}{}, err
	}, backoff.WithBackOff(b), backoff.WithMaxElapsedTime(0), backoff.WithMaxTries(0), backoff.WithNotify(func(err error, delay time.Duration) {
		if o.log != nil {
			fmt.Fprintf(o.log, "Reverse connection: attempt=%d reason=%s; retry in %s\n", attempts, statusText(err.Error()), delay.Round(time.Millisecond))
		}
	}))
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// Incomplete/hung cleanup is polled with the same bounded backoff, rather than
// abandoning the old runtime and accumulating live sessions or ownership locks.
func (f *providerFactory) reconnectReady() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.constructing {
		return errors.New("waiting for previous runtime construction to finish cleanup")
	}
	if f.active == nil {
		return nil
	}
	select {
	case <-f.active.closeDone:
		if f.active.closeErr != nil {
			return errors.New("previous runtime cleanup failed; replacement is blocked")
		}
		return nil
	default:
		return errors.New("waiting for previous runtime cleanup before reconnecting")
	}
}

func (f *providerFactory) healthyDuration() time.Duration {
	f.mu.Lock()
	s := f.active
	f.mu.Unlock()
	if s == nil {
		return 0
	}
	s.timing.Lock()
	defer s.timing.Unlock()
	if s.started.IsZero() || s.stopped.IsZero() {
		return 0
	}
	return s.stopped.Sub(s.started)
}
