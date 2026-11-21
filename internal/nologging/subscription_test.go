package nologging

import (
	"context"
	"errors"
	"testing"

	api "github.com/colony-2/pulse-provider-api"
)

func TestUnavailableSubscription(t *testing.T) {
	launches := []api.Launch{{LaunchID: "a"}, {LaunchID: "b"}, {LaunchID: "a"}}
	if Subscription(launches, api.SubmitOptions{}) != nil {
		t.Fatal("nil listener created subscription")
	}
	s := Subscription(launches, api.SubmitOptions{Listener: &api.ListenOptions{OnLog: func(context.Context, api.LaunchLog) error { t.Fatal("called listener"); return nil }}})
	select {
	case <-s.Done():
	default:
		t.Fatal("unavailable subscription is still active")
	}
	s.Stop()
	s.Stop()
	r, err := s.Wait(t.Context())
	if !errors.Is(err, api.ErrLogSource) || r.Reason != api.ListenerSourceUnavailable || len(r.Coverage) != 8 {
		t.Fatal(r, err)
	}
	for _, status := range r.Coverage {
		if status.State != api.SourceUnavailable || status.Closed || status.LastSequence != 0 {
			t.Fatal(status)
		}
	}
	r.Coverage[0].State = api.SourceActive
	coverage := s.Coverage()
	coverage[0].LaunchID = "changed"
	if s.Coverage()[0].State != api.SourceUnavailable || s.Coverage()[0].LaunchID != "a" {
		t.Fatal("caller mutated subscription")
	}
}
