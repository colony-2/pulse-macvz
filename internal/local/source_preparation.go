package local

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/colony-2/pulse-tart/internal/backend"
)

type sourcePreparation struct {
	done     bool
	err      error
	reported bool
	mu       sync.Mutex
	progress backend.SourceProgress
}

// PrepareSource polls a shared registry import. Imports belong to the client,
// not a short reconciliation call or a VM's guest-delivery deadline. Close
// cancels and joins them before releasing controller ownership.
func (b *Client) PrepareSource(ctx context.Context, source string) (bool, error) {
	preparer, ok := b.hypervisor().(interface {
		PrepareSource(context.Context, string) error
	})
	if !ok || !strings.Contains(source, "@sha256:") {
		return true, nil
	}
	if err := b.mu.LockContext(ctx); err != nil {
		return false, err
	}
	defer b.mu.Unlock()
	if b.closed {
		return false, failure("prepare registry source: client closed", nil)
	}
	if b.sources == nil {
		b.sources = map[string]*sourcePreparation{}
	}
	if result := b.sources[source]; result != nil {
		if result.done && result.err != nil && result.reported {
			// Retry on a later reconciliation, retaining the failed observation
			// until then so the CLI can include it in its final progress report.
			delete(b.sources, source)
		} else {
			if result.done && result.err != nil {
				result.reported = true
			}
			return result.done && result.err == nil, result.err
		}
	}
	result := &sourcePreparation{progress: backend.SourceProgress{Source: source,
		TransferProgress: backend.TransferProgress{Stage: "queued"}, Started: time.Now(), Updated: time.Now()}}
	b.sources[source] = result
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		call, cancel := context.WithTimeout(b.ctx, b.timeout)
		defer cancel()
		call = backend.WithProgress(call, func(p backend.TransferProgress) {
			result.mu.Lock()
			result.progress.TransferProgress, result.progress.Updated = p, time.Now()
			result.mu.Unlock()
		})
		err := preparer.PrepareSource(call, source)
		result.mu.Lock()
		if err != nil {
			result.progress.Stage = "failed"
		} else if result.progress.Stage != "cached" {
			result.progress.Stage = "ready"
		}
		result.progress.Updated = time.Now()
		result.mu.Unlock()
		b.mu.Lock()
		result.done, result.err = true, err
		b.mu.Unlock()
	}()
	return false, nil
}

func (b *Client) PreparationProgress() backend.PreparationProgress {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out backend.PreparationProgress
	for _, source := range b.sources {
		source.mu.Lock()
		out.Sources = append(out.Sources, source.progress)
		source.mu.Unlock()
	}
	for name, r := range b.records {
		v := backend.VMProgress{Name: name, Stage: r.Phase}
		if r.Delivery != nil {
			v.Stage, v.Cause = r.Delivery.Stage, r.Delivery.Cause
		}
		out.VMs = append(out.VMs, v)
	}
	return out
}
