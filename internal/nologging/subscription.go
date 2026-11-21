// Package nologging reports unavailable logs without affecting admission.
package nologging

import (
	"context"
	"fmt"

	api "github.com/colony-2/pulse-provider-api"
)

var unavailable = fmt.Errorf("%w: launch logging is not implemented", api.ErrLogSource)

type subscription struct {
	done     chan struct{}
	coverage []api.SourceStatus
}

// Subscription allocates no readers, goroutines, or callbacks. A requested
// listener receives an already-completed, explicitly unavailable subscription.
func Subscription(launches []api.Launch, options api.SubmitOptions) api.LogSubscription {
	if options.Listener == nil {
		return nil
	}
	s := &subscription{done: make(chan struct{})}
	seen := map[string]bool{}
	for _, l := range launches {
		if l.LaunchID == "" || seen[l.LaunchID] {
			continue
		}
		seen[l.LaunchID] = true
		for _, source := range []api.LogSource{api.LogSourceLaunch, api.LogSourceStdout, api.LogSourceStderr, api.LogSourceCombined} {
			s.coverage = append(s.coverage, api.SourceStatus{LaunchID: l.LaunchID, Source: source, State: api.SourceUnavailable, Cause: unavailable})
		}
	}
	close(s.done)
	return s
}

func (*subscription) Stop()                   {}
func (s *subscription) Done() <-chan struct{} { return s.done }
func (s *subscription) Coverage() []api.SourceStatus {
	return append([]api.SourceStatus(nil), s.coverage...)
}
func (s *subscription) Wait(context.Context) (api.ListenerResult, error) {
	return api.ListenerResult{Reason: api.ListenerSourceUnavailable, Cause: unavailable, Coverage: s.Coverage()}, unavailable
}
