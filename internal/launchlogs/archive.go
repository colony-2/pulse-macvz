package launchlogs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/colony-2/pulse-macvz/guest/logstore"
	"github.com/colony-2/pulse-macvz/internal/config"
	"github.com/colony-2/pulse-macvz/internal/provider"
)

type Archive struct {
	root     string
	config   config.Logging
	provider *provider.Provider
	mu       sync.Mutex
	phases   map[string]string
	failures map[string]error
	cancel   context.CancelFunc
	done     chan struct{}
}

func NewArchive(p *provider.Provider, c config.Logging) *Archive {
	root := c.Directory
	if root == "" {
		root = filepath.Join(p.Config.Endpoint, "logs")
		if p.Config.Endpoint == "" {
			root = filepath.Join(p.Config.StateDir, "logs")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &Archive{root: filepath.Join(root, p.Config.Scope()), config: c, provider: p, phases: map[string]string{}, failures: map[string]error{}, cancel: cancel, done: make(chan struct{})}
	go a.run(ctx)
	return a
}
func (a *Archive) Close() { a.cancel(); <-a.done }
func (a *Archive) Read(ctx context.Context, id string, offset uint64) (logstore.Page, string, error) {
	if err := ctx.Err(); err != nil {
		return logstore.Page{}, "", err
	}
	a.mu.Lock()
	phase, failure := a.phases[id], a.failures[id]
	a.mu.Unlock()
	page, err := logstore.Read(a.root, id, offset)
	// Deliver durable data before reporting an interrupted source.
	if page.Manifest.Unavailable {
		failure = provider.ErrLogsUnavailable
	}
	if err == nil && len(page.Frames) == 0 && page.Manifest.Interrupted {
		if failure != nil {
			err = failure
		} else {
			err = logstore.ErrJournal
		}
	}
	if err == nil && len(page.Frames) == 0 && !page.Manifest.Closed && failure != nil {
		err = failure
	}
	return page, phase, err
}
func (a *Archive) run(ctx context.Context) {
	defer close(a.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	a.clean()
	var cached []provider.LogTarget

	for {
		if ctx.Err() != nil {
			return
		}
		short, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		targets, err := a.provider.LogTargets(short)
		cancel()
		if err == nil {
			cached = targets
		} else {
			targets = cached
		}
		if err == nil || len(targets) > 0 {
			// Bound SSH concurrency independently of slots/listeners.
			sem := make(chan struct{}, 4)
			var wg sync.WaitGroup
			for _, target := range targets {
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					wg.Wait()
					return
				}
				wg.Add(1)
				go func(t provider.LogTarget) { defer wg.Done(); defer func() { <-sem }(); a.collect(ctx, t) }(target)
			}
			wg.Wait()
			// Missing VM records must not leave subscriptions pending forever.
			live := map[string]bool{}
			for _, t := range targets {
				live[t.LaunchID] = true
			}
			a.mu.Lock()
			var missing []string
			for id := range a.phases {
				if !live[id] {
					missing = append(missing, id)
				}
			}
			a.mu.Unlock()
			for _, id := range missing {
				a.fail(id, provider.ErrLogsGone)
			}
			a.mu.Lock()
			if len(a.phases) > 4096 {
				for _, id := range missing {
					delete(a.phases, id)
					delete(a.failures, id)
				}
			}
			a.mu.Unlock()
		}
		a.clean()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (a *Archive) collect(ctx context.Context, t provider.LogTarget) {
	a.mu.Lock()
	a.phases[t.LaunchID] = safePhase(t.Phase)
	failure := a.failures[t.LaunchID]
	a.mu.Unlock()
	if failure != nil {
		return
	}
	if err := logstore.PrivateDir(a.root); err != nil {
		a.fail(t.LaunchID, logstore.ErrJournal)
		return
	}
	dir := logstore.Dir(a.root, t.LaunchID)
	m, err := logstore.Load(dir, t.LaunchID)
	if err != nil && !os.IsNotExist(err) {
		a.fail(t.LaunchID, logstore.ErrJournal)
		return
	}
	// Bind the archive to immutable VM identity and public job identity only.
	identity, _ := json.Marshal(t)
	identityPath := filepath.Join(dir, "identity.json")
	if old, e := os.ReadFile(identityPath); e == nil {
		var previous provider.LogTarget
		if json.Unmarshal(old, &previous) != nil || previous.UID != t.UID || previous.Name != t.Name || previous.LaunchID != t.LaunchID {
			a.fail(t.LaunchID, logstore.ErrJournal)
			return
		}
	} else if !os.IsNotExist(e) {
		a.fail(t.LaunchID, logstore.ErrJournal)
		return
	} else if e = logstore.Atomic(dir, "identity.json", identity); e != nil {
		a.fail(t.LaunchID, logstore.ErrJournal)
		return
	}
	if m.Closed || m.Interrupted {
		return
	}
	if m.Version == 0 {
		m = logstore.Manifest{Version: 1, LaunchID: t.LaunchID}
		if e := logstore.Save(dir, m); e != nil {
			a.fail(t.LaunchID, logstore.ErrJournal)
			return
		}
	}
	short, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	// A bounded burst catches up without starving other VMs.
	for i := 0; i < 16; i++ {
		page, e := a.provider.Logs(short, t, m.End)
		if e != nil {
			if errors.Is(e, provider.ErrLogsUnavailable) || errors.Is(e, provider.ErrLogsGone) || errors.Is(e, logstore.ErrJournal) {
				a.fail(t.LaunchID, e)
			}
			return
		}
		if page.Pending {
			return
		}
		if e = logstore.Mirror(a.root, t.LaunchID, page, *a.config.Compress); e != nil {
			a.fail(t.LaunchID, logstore.ErrJournal)
			return
		}
		m = page.Manifest
		m.End = page.Next
		if page.Manifest.Closed && page.Next == page.Manifest.End {
			_ = a.provider.AckLogs(short, t, page.Next)
			return
		}
		if len(page.Frames) == 0 || page.Next == page.Manifest.End {
			return
		}
	}
}
func (a *Archive) fail(id string, e error) {
	a.mu.Lock()
	if _, exists := a.failures[id]; exists {
		a.mu.Unlock()
		return
	}
	a.failures[id] = e
	if len(a.failures) > 8192 {
		for key := range a.failures {
			if _, active := a.phases[key]; !active && key != id {
				delete(a.failures, key)
				break
			}
		}
	}
	a.mu.Unlock()
	dir := logstore.Dir(a.root, id)
	if m, err := logstore.Load(dir, id); err == nil && !m.Closed {
		m.Interrupted = true
		m.Unavailable = errors.Is(e, provider.ErrLogsUnavailable)
		_ = logstore.Save(dir, m)
	}
}
func safePhase(p string) string {
	switch p {
	case "dispatch", "active", "quarantine", "done", "intent", "creating", "running", "complete", "deleting", "accepted", "handoff-intent", "assigned", "ready", "reclaiming":
		return p
	default:
		return "observing execution"
	}
}

// Retention counts actual compressed bytes. Oldest completed streams go first;
// an active stream evicted under pressure is explicitly interrupted, never silent.
func (a *Archive) clean() {
	entries, err := os.ReadDir(a.root)
	if err != nil {
		return
	}
	type candidate struct {
		dir, id string
		size    int64
		when    time.Time
		closed  bool
		files   int
	}
	var candidates []candidate
	var total int64
	totalFiles := 0
	for _, entry := range entries {
		if !entry.IsDir() || len(entry.Name()) != 64 {
			continue
		}
		dir := filepath.Join(a.root, entry.Name())
		var m logstore.Manifest
		b, e := os.ReadFile(filepath.Join(dir, "manifest.json"))
		if e != nil || json.Unmarshal(b, &m) != nil || logstore.Key(m.LaunchID) != entry.Name() {
			continue
		}
		var size int64
		count := 0
		_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, e error) error {
			if e == nil && !d.IsDir() {
				if i, e := d.Info(); e == nil {
					size += i.Size()
					count++
				}
			}
			return nil
		})
		total += size
		totalFiles += count
		candidates = append(candidates, candidate{dir, m.LaunchID, size, m.Updated, m.Closed || m.Interrupted, count})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].closed != candidates[j].closed {
			return candidates[i].closed
		}
		return candidates[i].when.Before(candidates[j].when)
	})
	remaining := len(candidates)
	for _, c := range candidates {
		expired := c.closed && time.Since(c.when) > a.config.MaxAge
		if !expired && total <= a.config.MaxBytes && remaining <= 4096 && totalFiles <= 65536 {
			continue
		}
		a.fail(c.id, logstore.ErrJournal)
		if os.RemoveAll(c.dir) == nil {
			total -= c.size
			totalFiles -= c.files
			remaining--
		}
	}
}
