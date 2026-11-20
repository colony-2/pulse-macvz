package local

import (
	"context"
	"strings"
)

type sourcePreparation struct {
	done bool
	err  error
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
		if result.done && result.err != nil {
			// Surface the failure once. A later reconciliation may retry; never
			// silently turn a failed import into a supposedly ready source.
			delete(b.sources, source)
		}
		return result.done && result.err == nil, result.err
	}
	result := &sourcePreparation{}
	b.sources[source] = result
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		call, cancel := context.WithTimeout(b.ctx, b.timeout)
		defer cancel()
		err := preparer.PrepareSource(call, source)
		b.mu.Lock()
		result.done, result.err = true, err
		b.mu.Unlock()
	}()
	return false, nil
}
