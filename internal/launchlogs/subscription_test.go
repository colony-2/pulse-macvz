package launchlogs

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/colony-2/pulse-macvz/guest/logstore"
	api "github.com/colony-2/pulse-provider-api"
)

func finishTest(t *testing.T, s api.LogSubscription) api.ListenerResult {
	t.Helper()
	ctx, c := context.WithTimeout(context.Background(), 3*time.Second)
	defer c()
	r, e := s.Wait(ctx)
	if errors.Is(e, context.DeadlineExceeded) {
		t.Fatal("subscription did not finish")
	}
	return r
}
func testManager(read Reader) *Manager { m := New(read); m.PollInterval = time.Millisecond; return m }
func TestBinarySequencingAndIndependentAdmissionContext(t *testing.T) {
	data := []byte{0, 255, 'x'}
	m := testManager(func(_ context.Context, id string, off uint64) (logstore.Page, string, error) {
		p := logstore.Page{Manifest: logstore.Manifest{Version: 1, LaunchID: id, End: 3, Closed: true}, Next: 3}
		if off == 0 {
			p.Frames = []logstore.Frame{{Data: data}}
		}
		return p, "running", nil
	})
	defer m.Close()
	var logs []api.LaunchLog
	s := m.Subscribe([]api.Launch{{LaunchID: "a"}}, api.SubmitOptions{Listener: &api.ListenOptions{OnLog: func(_ context.Context, l api.LaunchLog) error { logs = append(logs, l); return nil }}})
	m.Admitted(s, []api.Submission{{LaunchID: "a", Status: api.Accepted}}, nil)
	r := finishTest(t, s)
	if r.Reason != api.ListenerSourcesClosed || len(logs) != 3 {
		t.Fatal(r, logs)
	}
	for i, l := range logs {
		if l.Sequence != uint64(i+1) || l.SubscriptionID == "" {
			t.Fatal(l)
		}
	}
	if !bytes.Equal(logs[2].Data, data) {
		t.Fatal("binary data changed")
	}
	coverage := s.Coverage()
	coverage[0].State = api.SourcePending
	if s.Coverage()[0].State == api.SourcePending {
		t.Fatal("coverage aliases internal state")
	}
}
func TestStopAndProviderCloseFromCallback(t *testing.T) {
	for _, closeProvider := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop", true: "close"}[closeProvider], func(t *testing.T) {
			m := testManager(func(context.Context, string, uint64) (logstore.Page, string, error) {
				return logstore.Page{Pending: true}, "", nil
			})
			defer m.Close()
			var s api.LogSubscription
			s = m.Subscribe([]api.Launch{{LaunchID: "a"}}, api.SubmitOptions{Listener: &api.ListenOptions{OnLog: func(context.Context, api.LaunchLog) error {
				if closeProvider {
					m.Close()
				} else {
					s.Stop()
				}
				return nil
			}}})
			m.Admitted(s, []api.Submission{{LaunchID: "a", Status: api.Accepted}}, nil)
			r := finishTest(t, s)
			want := api.ListenerStopped
			if closeProvider {
				want = api.ListenerProviderClosed
			}
			if r.Reason != want {
				t.Fatal(r)
			}
		})
	}
}
func TestStuckCallbacksRetainCapacity(t *testing.T) {
	m := testManager(func(context.Context, string, uint64) (logstore.Page, string, error) {
		return logstore.Page{Pending: true}, "", nil
	})
	m.CallbackTimeout = 50 * time.Millisecond
	unblock := make(chan struct{})
	defer close(unblock)
	defer m.Close()
	started := make(chan struct{}, 32)
	var subs []api.LogSubscription
	for i := 0; i < 32; i++ {
		s := m.Subscribe([]api.Launch{{LaunchID: "a"}}, api.SubmitOptions{Listener: &api.ListenOptions{OnLog: func(context.Context, api.LaunchLog) error { started <- struct{}{}; <-unblock; return nil }}})
		m.Admitted(s, []api.Submission{{LaunchID: "a", Status: api.Accepted}}, nil)
		subs = append(subs, s)
	}
	for range subs {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("callback not dispatched")
		}
	}
	for _, s := range subs {
		if r := finishTest(t, s); r.Reason != api.ListenerSlow {
			t.Fatal(r)
		}
	}
	extra := m.Subscribe([]api.Launch{{LaunchID: "b"}}, api.SubmitOptions{Listener: &api.ListenOptions{OnLog: func(context.Context, api.LaunchLog) error { return nil }}})
	if r := finishTest(t, extra); r.Reason != api.ListenerCapacityExceeded {
		t.Fatal(r)
	}
}
func TestContextDurationErrorsAndNilListener(t *testing.T) {
	m := testManager(func(context.Context, string, uint64) (logstore.Page, string, error) {
		return logstore.Page{Pending: true}, "", nil
	})
	defer m.Close()
	if s := m.Subscribe(nil, api.SubmitOptions{}); s != nil {
		t.Fatal("nil listener allocated subscription")
	}
	for _, tc := range []struct {
		name     string
		duration time.Duration
		ctx      context.Context
		callback error
		reason   api.ListenerReason
	}{
		{name: "duration", duration: time.Millisecond, reason: api.ListenerDurationElapsed},
		{name: "invalid", duration: -1, reason: api.ListenerProtocolError},
		{name: "callback", callback: errors.New("secret"), reason: api.ListenerError},
		{name: "stop", callback: api.ErrStopListening, reason: api.ListenerStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := m.Subscribe([]api.Launch{{LaunchID: "a"}}, api.SubmitOptions{Listener: &api.ListenOptions{Duration: tc.duration, OnLog: func(context.Context, api.LaunchLog) error { return tc.callback }}})
			m.Admitted(s, []api.Submission{{LaunchID: "a", Status: api.Accepted}}, nil)
			r := finishTest(t, s)
			if r.Reason != tc.reason {
				t.Fatal(r)
			}
			if r.Cause != nil && r.Cause.Error() == "secret" {
				t.Fatal("callback leaked")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := m.Subscribe([]api.Launch{{LaunchID: "a"}}, api.SubmitOptions{Listener: &api.ListenOptions{Context: ctx, OnLog: func(context.Context, api.LaunchLog) error { return nil }}})
	cancel()
	if r := finishTest(t, s); r.Reason != api.ListenerContextCanceled {
		t.Fatal(r)
	}
}
func TestBatchOverflowAndGap(t *testing.T) {
	m := testManager(func(_ context.Context, id string, off uint64) (logstore.Page, string, error) {
		return logstore.Page{Gap: true}, "", nil
	})
	defer m.Close()
	s := m.Subscribe([]api.Launch{{LaunchID: "a"}}, api.SubmitOptions{Listener: &api.ListenOptions{OnLog: func(context.Context, api.LaunchLog) error { return nil }}})
	m.Admitted(s, []api.Submission{{LaunchID: "a", Status: api.Accepted}}, nil)
	if r := finishTest(t, s); r.Reason != api.ListenerSourceError {
		t.Fatal(r)
	}
	// Hold the supervisor in a callback and fill only the bounded queue.
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s = m.Subscribe([]api.Launch{{LaunchID: "b"}}, api.SubmitOptions{Listener: &api.ListenOptions{OnLog: func(context.Context, api.LaunchLog) error { once.Do(func() { close(entered) }); <-release; return nil }}})
	m.Admitted(s, []api.Submission{{LaunchID: "b", Status: api.Accepted}}, nil)
	<-entered
	raw := s.(*subscription)
	raw.mu.Lock()
	for i := 0; i < 100; i++ {
		raw.enqueueLocked(item{"b", api.LogSourceLaunch, []byte("event")})
	}
	raw.mu.Unlock()
	if r := finishTest(t, s); r.Reason != api.ListenerOverflow {
		t.Fatal(r)
	}
	close(release)
}

func TestCallbackPanicIsIsolatedAndDeadlineIsVisible(t *testing.T) {
	m := testManager(func(context.Context, string, uint64) (logstore.Page, string, error) {
		return logstore.Page{Pending: true}, "", nil
	})
	defer m.Close()
	s := m.Subscribe([]api.Launch{{LaunchID: "a"}}, api.SubmitOptions{Listener: &api.ListenOptions{OnLog: func(ctx context.Context, _ api.LaunchLog) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("missing callback deadline")
		}
		panic("private callback text")
	}}})
	m.Admitted(s, []api.Submission{{LaunchID: "a", Status: api.Accepted}}, nil)
	if r := finishTest(t, s); r.Reason != api.ListenerError {
		t.Fatal(r)
	}
}
