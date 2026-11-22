package workerhealth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFailureWindows(t *testing.T) {
	start := time.Now().UTC()
	fail := Observation{Supported: true, Failure: "unreachable"}
	var s State
	for i := 0; i < 18; i++ {
		if s.Observe(start.Add(time.Duration(i)*Interval), fail, i == 0) {
			t.Fatalf("reaped early at %d", i)
		}
	}
	if !s.Observe(start.Add(90*time.Second), fail, false) {
		t.Fatal("never-started worker not reaped after startup and failure grace")
	}
	if s.Status != "reaping" || s.ReapRequestedAt == nil {
		t.Fatal(s)
	}
	// An already persisted cleanup intent must survive restart.
	if !s.Observe(start.Add(time.Hour), Observation{}, true) {
		t.Fatal("lost reap intent")
	}
}
func TestTransientAndRestart(t *testing.T) {
	start := time.Now().UTC()
	healthy := Observation{Supported: true, Snapshot: &Snapshot{}}
	fail := Observation{Supported: true, Failure: "timeout"}
	var s State
	s.Observe(start, healthy, true)
	for i := 1; i <= 5; i++ {
		if s.Observe(start.Add(time.Duration(i)*Interval), fail, false) {
			t.Fatal("transient outage reaped")
		}
	}
	s.Observe(start.Add(30*time.Second), healthy, false)
	if s.Failures != 0 || s.FailingSince != nil {
		t.Fatal("did not recover")
	}
	s.Observe(start.Add(35*time.Second), fail, false)
	s.Observe(start.Add(time.Hour), fail, true)
	if s.ReapRequestedAt != nil || s.Failures != 1 {
		t.Fatal("downtime counted as failure", s)
	}
	// Large in-process gaps have the same conservative treatment.
	if s.Observe(start.Add(2*time.Hour), fail, false) {
		t.Fatal("observation gap counted")
	}
	for i := 1; i <= 6; i++ {
		s.Observe(start.Add(2*time.Hour+time.Duration(i)*Interval), fail, false)
	}
	if s.ReapRequestedAt == nil {
		t.Fatal("sustained failure not reaped")
	}
}
func TestUnsupported(t *testing.T) {
	var s State
	for i := 0; i < 100; i++ {
		if s.Observe(time.Now().Add(time.Duration(i)*Interval), Observation{}, false) {
			t.Fatal("unsupported worker reaped")
		}
	}
	if s.Status != "unsupported" {
		t.Fatal(s)
	}
}
func TestFetch(t *testing.T) {
	now := time.Now().UTC()
	good := Snapshot{TenantID: "tenant", JobID: "job", StartedAt: now, ObservedAt: now, TaskType: "framework"}
	data, _ := json.Marshal(good)
	for _, tt := range []struct {
		name, body string
		code       int
		valid      bool
	}{
		{"valid", string(data), 200, true}, {"wrong tenant", strings.ReplaceAll(string(data), "tenant", "other"), 200, false},
		{"wrong job", strings.ReplaceAll(string(data), `"job"`, `"other"`), 200, false},
		{"malformed", "{", 200, false}, {"missing fields", "{}", 200, false}, {"large", strings.Repeat("x", 16385), 200, false},
		{"server failure", "secret response", 500, false}, {"redirect", "", 302, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/health" {
					t.Error(r.URL.Path)
				}
				w.Header().Set("Location", "http://invalid.invalid")
				w.WriteHeader(tt.code)
				fmt.Fprint(w, tt.body)
			}))
			defer ts.Close()
			t.Setenv("HTTP_PROXY", "http://invalid.invalid")
			result := Fetch(context.Background(), strings.TrimPrefix(ts.URL, "http://"), "tenant", "job")
			if (result.Snapshot != nil) != tt.valid || strings.Contains(result.Failure, "secret") {
				t.Fatal(result)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if Fetch(ctx, "127.0.0.1:1", "tenant", "job").Snapshot != nil {
		t.Fatal("canceled request succeeded")
	}
}
