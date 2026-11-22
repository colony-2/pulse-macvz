// Package workerhealth implements the c2j /health contract and its failure window.
package workerhealth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

const (
	Port         = "8080"
	Interval     = 5 * time.Second
	Timeout      = 3 * time.Second
	StartupGrace = 60 * time.Second
	FailureGrace = 30 * time.Second
)

type Snapshot struct {
	TenantID             string     `json:"tenant_id"`
	JobID                string     `json:"job_id"`
	StartedAt            time.Time  `json:"started_at"`
	ObservedAt           time.Time  `json:"observed_at"`
	TaskType             string     `json:"task_type"`
	ExtensionOp          *string    `json:"extension_op"`
	CurrentTaskStartedAt time.Time  `json:"current_task_started_at"`
	LeaseExpiresAt       *time.Time `json:"lease_expires_at"`
}

// Observation distinguishes a failed probe from an unavailable native observation.
// Only a known supported worker is eligible for automatic cleanup.
type Observation struct {
	Supported bool      `json:"supported"`
	Snapshot  *Snapshot `json:"snapshot,omitempty"`
	Failure   string    `json:"failure,omitempty"`
}

type State struct {
	Status          string     `json:"status"`
	StartedAt       time.Time  `json:"monitor_started_at"`
	CheckedAt       time.Time  `json:"checked_at"`
	LastSuccess     *time.Time `json:"last_success,omitempty"`
	FailingSince    *time.Time `json:"failing_since,omitempty"`
	Failures        int        `json:"consecutive_failures"`
	Reason          string     `json:"reason,omitempty"`
	Snapshot        *Snapshot  `json:"last_snapshot,omitempty"`
	ReapRequestedAt *time.Time `json:"reap_requested_at,omitempty"`
}

// Observe never counts time spent disconnected from monitoring as evidence of
// failure. A restart or long observation gap starts a fresh failure window.
func (s *State) Observe(now time.Time, o Observation, fresh bool) bool {
	if s.ReapRequestedAt != nil {
		return true
	}
	if fresh || s.Status == "unsupported" && o.Supported || s.CheckedAt.IsZero() || now.Sub(s.CheckedAt) > 3*Interval || now.Before(s.CheckedAt) {
		s.FailingSince = nil
		s.Failures = 0
		if s.LastSuccess == nil {
			s.StartedAt = now
		}
	}
	s.CheckedAt = now
	if !o.Supported {
		s.Status = "unsupported"
		s.Reason = "image does not advertise a c2j health probe; upgrade prepared images"
		s.FailingSince = nil
		s.Failures = 0
		return false
	}
	if o.Snapshot != nil {
		s.Snapshot = o.Snapshot
		s.LastSuccess = &now
		s.FailingSince = nil
		s.Failures = 0
		s.Status = "healthy"
		s.Reason = ""
		return false
	}
	s.Reason = o.Failure
	if s.LastSuccess == nil && now.Sub(s.StartedAt) < StartupGrace {
		s.Status = "starting"
		return false
	}
	if s.FailingSince == nil {
		s.FailingSince = &now
	}
	s.Failures++
	s.Status = "unhealthy"
	if now.Sub(*s.FailingSince) >= FailureGrace && s.Failures >= 3 {
		s.Status = "reaping"
		s.ReapRequestedAt = &now
		return true
	}
	return false
}

// Fetch never uses proxy settings or follows redirects. Error messages contain
// no response bodies, URLs or job payloads. Callers supply a VM-local address.
func Fetch(ctx context.Context, address, tenant, job string) Observation {
	o := Observation{Supported: true}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, MaxResponseHeaderBytes: 8192}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/health", nil)
	if err != nil {
		o.Failure = "invalid probe address"
		return o
	}
	resp, err := client.Do(req)
	if err != nil {
		o.Failure = "probe unreachable or timed out"
		return o
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		o.Failure = "probe returned a non-200 status"
		return o
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16385))
	var s Snapshot
	if err != nil || len(data) > 16384 || json.Unmarshal(data, &s) != nil || s.StartedAt.IsZero() || s.ObservedAt.Before(s.StartedAt) || s.TaskType == "" {
		o.Failure = "invalid probe response"
		return o
	}
	if s.TenantID != tenant || s.JobID != job {
		o.Failure = "probe job identity mismatch"
		return o
	}
	o.Snapshot = &s
	return o
}

var ErrIdentity = errors.New("health probe VM identity unavailable or changed")
