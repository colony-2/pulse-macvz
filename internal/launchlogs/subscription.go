// Package launchlogs implements bounded, independent API log subscriptions.
package launchlogs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/colony-2/pulse-macvz/guest/logstore"
	"github.com/colony-2/pulse-macvz/internal/provider"
	api "github.com/colony-2/pulse-provider-api"
)

type Reader func(context.Context, string, uint64) (logstore.Page, string, error)
type Manager struct {
	mu              sync.Mutex
	closed          bool
	resident        int
	subscriptions   map[*subscription]bool
	Read            Reader
	CallbackTimeout time.Duration
	PollInterval    time.Duration
}

func New(read Reader) *Manager {
	return &Manager{Read: read, subscriptions: map[*subscription]bool{}, CallbackTimeout: 5 * time.Second, PollInterval: 250 * time.Millisecond}
}

type item struct {
	id     string
	source api.LogSource
	data   []byte
}
type subscription struct {
	mu         sync.Mutex
	manager    *Manager
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	result     api.ListenerResult
	coverage   []api.SourceStatus
	seq        map[string]uint64
	pending    map[string]int
	queue      chan item
	id         string
	listener   api.ListenOptions
	admissions map[string]api.Status
	offsets    map[string]uint64
	phases     map[string]string
	ended      bool
}

func (m *Manager) Subscribe(launches []api.Launch, options api.SubmitOptions) api.LogSubscription {
	if options.Listener == nil {
		return nil
	}
	l := *options.Listener
	ctx := l.Context
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	s := &subscription{manager: m, ctx: ctx, cancel: cancel, done: make(chan struct{}), seq: map[string]uint64{}, pending: map[string]int{}, queue: make(chan item, 64), id: hex.EncodeToString(nonce), listener: l, admissions: map[string]api.Status{}, offsets: map[string]uint64{}, phases: map[string]string{}}
	seen := map[string]bool{}
	for _, v := range launches {
		if v.LaunchID == "" || seen[v.LaunchID] {
			continue
		}
		seen[v.LaunchID] = true
		for _, source := range []api.LogSource{api.LogSourceLaunch, api.LogSourceCombined, api.LogSourceStdout, api.LogSourceStderr} {
			state := api.SourcePending
			var cause error
			if source == api.LogSourceStdout || source == api.LogSourceStderr {
				state = api.SourceUnavailable
				cause = fmt.Errorf("%w: guest provides combined output", api.ErrLogSource)
			}
			s.coverage = append(s.coverage, api.SourceStatus{LaunchID: v.LaunchID, Source: source, State: state, Cause: cause})
		}
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		s.finish(api.ListenerProviderClosed, api.ErrProviderClosed)
		return s
	}
	if l.Duration < 0 || l.OnLog == nil || len(seen) == 0 || len(seen) > 100 {
		m.mu.Unlock()
		s.finish(api.ListenerProtocolError, api.ErrLogProtocol)
		return s
	}
	if m.resident >= 32 {
		m.mu.Unlock()
		s.finish(api.ListenerCapacityExceeded, api.ErrLogCapacity)
		return s
	}
	m.resident++
	m.subscriptions[s] = true
	m.mu.Unlock()
	go s.run()
	return s
}
func (m *Manager) Admitted(sub api.LogSubscription, results []api.Submission, submitErr error) {
	s, ok := sub.(*subscription)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range results {
		s.admissions[r.LaunchID] = r.Status
	}
	for i := range s.coverage {
		c := &s.coverage[i]
		if c.Source != api.LogSourceLaunch {
			continue
		}
		status, ok := s.admissions[c.LaunchID]
		if !ok {
			status = api.Unavailable
			s.admissions[c.LaunchID] = status
		}
		// Never include arbitrary errors, job payloads, credentials, or lease bytes.
		s.enqueueLocked(item{c.LaunchID, api.LogSourceLaunch, []byte("admission: " + string(status) + "\n")})
	}
}
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	subs := make([]*subscription, 0, len(m.subscriptions))
	for s := range m.subscriptions {
		subs = append(subs, s)
	}
	m.mu.Unlock()
	for _, s := range subs {
		s.finish(api.ListenerProviderClosed, api.ErrProviderClosed)
	}
}
func (s *subscription) Stop()                 { s.finish(api.ListenerStopped, nil) }
func (s *subscription) Done() <-chan struct{} { return s.done }
func (s *subscription) Coverage() []api.SourceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]api.SourceStatus(nil), s.coverage...)
}
func (s *subscription) Wait(ctx context.Context) (api.ListenerResult, error) {
	select {
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		r := s.result
		r.Coverage = append([]api.SourceStatus(nil), r.Coverage...)
		return r, r.Cause
	case <-ctx.Done():
		return api.ListenerResult{}, ctx.Err()
	}
}
func (s *subscription) finish(reason api.ListenerReason, cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finishLocked(reason, cause)
}
func (s *subscription) finishLocked(reason api.ListenerReason, cause error) {
	if s.ended {
		return
	}
	s.ended = true
	for i := range s.coverage {
		c := &s.coverage[i]
		if c.State == api.SourcePending || c.State == api.SourceActive || (c.State == api.SourceClosed && s.pending[c.LaunchID+"/"+string(c.Source)] > 0) {
			c.State = api.SourceInterrupted
			c.Cause = cause
		}
	}
	s.result = api.ListenerResult{Reason: reason, Cause: cause, Coverage: append([]api.SourceStatus(nil), s.coverage...)}
	s.cancel()
	close(s.done)
}
func (s *subscription) enqueueLocked(i item) {
	if s.ended {
		return
	}
	select {
	case s.queue <- i:
		s.pending[i.id+"/"+string(i.source)]++
	default:
		s.finishLocked(api.ListenerOverflow, fmt.Errorf("%w: listener queue full", api.ErrLogDelivery))
	}
}
func (s *subscription) mark(id string, source api.LogSource, state api.SourceState, cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	for i := range s.coverage {
		c := &s.coverage[i]
		if c.LaunchID == id && c.Source == source {
			c.State = state
			c.Cause = cause
			c.Closed = state == api.SourceClosed
		}
	}
}
func (s *subscription) allClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.coverage {
		if c.State == api.SourcePending || c.State == api.SourceActive {
			return false
		}
	}
	return true
}
func (s *subscription) poll() {
	s.mu.Lock()
	a := make(map[string]api.Status, len(s.admissions))
	for id, v := range s.admissions {
		a[id] = v
	}
	s.mu.Unlock()
	for id, status := range a {
		if s.phases[id] == "closed" {
			continue
		}
		if status != api.Accepted && status != api.Unknown {
			s.mark(id, api.LogSourceLaunch, api.SourceClosed, nil)
			s.mark(id, api.LogSourceCombined, api.SourceUnavailable, api.ErrLogSource)
			s.phases[id] = "closed"
			continue
		}
		p, phase, err := s.manager.Read(s.ctx, id, s.offsets[id])
		if s.ctx.Err() != nil {
			return
		}
		if err != nil {
			state := api.SourceInterrupted
			if errors.Is(err, provider.ErrLogsUnavailable) {
				state = api.SourceUnavailable
			}
			s.mark(id, api.LogSourceCombined, state, api.ErrLogSource)
			s.mark(id, api.LogSourceLaunch, api.SourceClosed, nil)
			s.phases[id] = "closed"
			continue
		}
		if phase != "" && phase != s.phases[id] {
			s.mu.Lock()
			s.enqueueLocked(item{id, api.LogSourceLaunch, []byte("phase: " + phase + "\n")})
			s.mu.Unlock()
			s.phases[id] = phase
		}
		if p.Pending {
			continue
		}
		if p.Gap {
			s.finish(api.ListenerSourceError, fmt.Errorf("%w: retained output has a gap", api.ErrLogSource))
			return
		}
		for _, f := range p.Frames {
			s.mu.Lock()
			s.enqueueLocked(item{id, api.LogSourceCombined, f.Data})
			s.mu.Unlock()
		}
		s.offsets[id] = p.Next
		if p.Manifest.Closed && p.Next == p.Manifest.End {
			s.mark(id, api.LogSourceCombined, api.SourceClosed, nil)
			s.mark(id, api.LogSourceLaunch, api.SourceClosed, nil)
			s.phases[id] = "closed"
		}
	}
}
func (s *subscription) run() {
	// The callback worker owns the resident capacity until it actually exits,
	// including when a callback ignores cancellation after Done has closed.
	type call struct {
		ctx context.Context
		log api.LaunchLog
	}
	work := make(chan call)
	results := make(chan error, 1)
	go func() {
		defer func() { m := s.manager; m.mu.Lock(); m.resident--; delete(m.subscriptions, s); m.mu.Unlock() }()
		for entry := range work {
			if s.ctx.Err() != nil {
				return
			}
			s.mu.Lock()
			if s.ended {
				s.mu.Unlock()
				return
			}
			s.seq[entry.log.LaunchID]++
			entry.log.Sequence = s.seq[entry.log.LaunchID]
			entry.log.Timestamp = time.Now().UTC()
			for j := range s.coverage {
				c := &s.coverage[j]
				if c.LaunchID == entry.log.LaunchID && c.Source == entry.log.Source {
					c.LastSequence = entry.log.Sequence
					if c.State == api.SourcePending {
						c.State = api.SourceActive
					}
				}
			}
			s.mu.Unlock()
			err := invoke(s.listener.OnLog, entry.ctx, entry.log)
			results <- err
		}
	}()
	defer close(work)
	poll := time.NewTicker(s.manager.PollInterval)
	defer poll.Stop()
	var duration <-chan time.Time
	var timer *time.Timer
	if s.listener.Duration > 0 {
		timer = time.NewTimer(s.listener.Duration)
		defer timer.Stop()
		duration = timer.C
	}
	terminate := func() {
		if errors.Is(s.ctx.Err(), context.DeadlineExceeded) {
			s.finish(api.ListenerContextDeadline, context.DeadlineExceeded)
		} else {
			s.finish(api.ListenerContextCanceled, context.Canceled)
		}
	}
	for {
		select {
		case <-s.ctx.Done():
			terminate()
			return
		case <-duration:
			s.finish(api.ListenerDurationElapsed, nil)
			return
		case <-poll.C:
			s.poll()
		case i := <-s.queue:
			entry := api.LaunchLog{LaunchID: i.id, SubscriptionID: s.id, Source: i.source, Data: i.data}
			callbackCtx, cancelCallback := context.WithTimeout(s.ctx, s.manager.CallbackTimeout)
			select {
			case work <- call{callbackCtx, entry}:
			case <-s.ctx.Done():
				cancelCallback()
				terminate()
				return
			}
			t := time.NewTimer(s.manager.CallbackTimeout)
			select {
			case err := <-results:
				if err == nil {
					s.mu.Lock()
					s.pending[i.id+"/"+string(i.source)]--
					s.mu.Unlock()
				}
				cancelCallback()
				t.Stop()
				if errors.Is(err, api.ErrStopListening) {
					s.Stop()
					return
				}
				if err != nil {
					s.finish(api.ListenerError, fmt.Errorf("%w: callback failed", api.ErrLogDelivery))
					return
				}
			case <-t.C:
				cancelCallback()
				s.finish(api.ListenerSlow, fmt.Errorf("%w: callback deadline exceeded", api.ErrLogDelivery))
				return
			case <-s.ctx.Done():
				cancelCallback()
				t.Stop()
				terminate()
				return
			case <-duration:
				cancelCallback()
				t.Stop()
				s.finish(api.ListenerDurationElapsed, nil)
				return
			}
		}
		if len(s.queue) == 0 && s.allClosed() {
			reason := api.ListenerSourcesClosed
			var cause error
			for _, c := range s.Coverage() {
				if c.Source == api.LogSourceCombined && c.State == api.SourceUnavailable {
					reason = api.ListenerSourceUnavailable
					cause = api.ErrLogSource
				}
				if c.Source == api.LogSourceCombined && c.State == api.SourceInterrupted {
					reason = api.ListenerSourceError
					cause = api.ErrLogSource
				}
			}
			s.finish(reason, cause)
			return
		}
	}
}

func invoke(fn func(context.Context, api.LaunchLog) error, ctx context.Context, log api.LaunchLog) (err error) {
	defer func() {
		if recover() != nil {
			err = api.ErrLogDelivery
		}
	}()
	return fn(ctx, log)
}
