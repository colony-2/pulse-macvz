package provider

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/colony-2/pulse-macvz/internal/workerhealth"
	api "github.com/colony-2/pulse-provider-api"
)

type healthBackend interface {
	ProbeHealth(context.Context, string, string, string, string) (workerhealth.Observation, error)
	Stop(context.Context, string, string) error
}

// HealthMonitor belongs to the provider process, not the reverse connection or
// the guest. Persisted decisions survive reconnects; observation gaps do not
// count toward the failure window. Close does not cancel running VM jobs.
type HealthMonitor struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func StartHealthMonitor(p *Provider) *HealthMonitor {
	if _, ok := p.Backend.(healthBackend); !ok {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &HealthMonitor{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(m.done)
		ticker := time.NewTicker(workerhealth.Interval)
		defer ticker.Stop()
		seen := map[string]bool{}
		for {
			if err := p.checkLaunchHealth(ctx, time.Now().UTC(), seen); err != nil && ctx.Err() == nil {
				slog.Warn("worker health observation unavailable", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return m
}
func (m *HealthMonitor) Close() {
	if m != nil {
		m.cancel()
		<-m.done
	}
}

func (p *Provider) checkLaunchHealth(ctx context.Context, now time.Time, seen map[string]bool) error {
	b, ok := p.Backend.(healthBackend)
	if !ok {
		return nil
	}
	if err := p.Store.mu.LockContext(ctx); err != nil {
		return err
	}
	if err := p.Store.refresh(); err != nil {
		p.Store.mu.Unlock()
		return err
	}
	var targets []Record
	for _, r := range p.Store.data.Records {
		// Old launches are deliberately grandfathered. New launch records opt in;
		// their image must independently advertise support before failures count.
		if r.Health == nil || r.UID == "" || r.Outcome != api.Accepted || r.Phase == "done" || r.Phase == "quarantine" {
			continue
		}
		if r.Phase == "deleting" && r.Health.ReapRequestedAt == nil {
			continue
		}
		targets = append(targets, *r)
	}
	p.Store.mu.Unlock()
	if len(targets) == 0 {
		clear(seen)
		return nil
	}
	type result struct {
		o   workerhealth.Observation
		err error
	}
	results := make([]result, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t Record) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[i].err = ctx.Err()
				return
			}
			defer func() { <-sem }()
			if t.Health.ReapRequestedAt != nil {
				return
			}
			results[i].o, results[i].err = b.ProbeHealth(ctx, t.Name, t.UID, t.Job.TenantID, t.Job.JobID)
		}(i, t)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.Store.mu.LockContext(ctx); err != nil {
		return err
	}
	defer p.Store.mu.Unlock()
	if err := p.Store.refresh(); err != nil {
		return err
	}
	live := map[string]bool{}
	for i, t := range targets {
		r := p.Store.data.Records[t.LaunchID]
		if r == nil || r.Health == nil || r.Name != t.Name || r.UID != t.UID || r.Phase == "done" || r.Phase == "quarantine" {
			continue
		}
		live[r.LaunchID] = true
		if results[i].err != nil {
			continue
		} // No native observation is not a failed HTTP probe.
		next := *r.Health
		reap := next.Observe(now, results[i].o, !seen[r.LaunchID])
		seen[r.LaunchID] = true
		if !reap {
			r.Health = &next
			continue
		}
		// Check the immutable native spec and UID immediately before committing a
		// cleanup intent. Never stop a replacement or an unrelated owner resource.
		v, err := p.Backend.Get(ctx, r.Name)
		if err != nil || !p.matches(r, v) {
			continue
		}
		if pair := p.Store.data.Pairs[r.PairID]; pair != nil {
			if pair.LaunchID != r.LaunchID || !p.pairMatches(pair, v, true) {
				continue
			}
			pair.Phase = "reclaiming"
		}
		r.Health = &next
		r.Phase = "deleting"
		if err := p.Store.save(); err != nil {
			return err
		}
		if !v.Stopped() {
			slog.Warn("reaping worker after sustained health probe failure", "launch_id", r.LaunchID, "vm", r.Name, "reason", r.Health.Reason)
			if err := b.Stop(ctx, r.Name, r.UID); err != nil {
				return err
			}
		}
	}
	for id := range seen {
		if !live[id] {
			delete(seen, id)
		}
	}
	return p.Store.save()
}
