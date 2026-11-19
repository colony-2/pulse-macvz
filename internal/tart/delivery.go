package tart

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

var ErrDeliveryUnavailable = errors.New("Tart guest delivery unavailable")

// DeliveryStatus contains no credentials, scripts, leases, or remote error text.
// ExecutionPossible is a durable fence written BEFORE requesting guest exec.
type DeliveryStatus struct {
	StartedAt         time.Time `json:"started_at"`
	LastAttemptAt     time.Time `json:"last_attempt_at"`
	LastSuccessAt     time.Time `json:"last_success_at,omitempty"`
	Attempts          uint      `json:"attempts"`
	Stage             string    `json:"stage"`
	Cause             string    `json:"cause,omitempty"`
	ExecutionPossible bool      `json:"execution_possible"`
	Expired           bool      `json:"expired"`
}

type DeliveryObservation struct {
	Name string `json:"name"`
	UID  string `json:"uid"`
	DeliveryStatus
}

func (b *Client) deliveryBudget() time.Duration {
	if b.cfg.DeliveryTimeout > 0 {
		return b.cfg.DeliveryTimeout
	}
	return 2 * time.Minute
}

func deliveryCause(err error) string {
	var network net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, syscall.EHOSTUNREACH):
		return "EHOSTUNREACH"
	case errors.Is(err, syscall.ENETUNREACH):
		return "ENETUNREACH"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "ECONNREFUSED"
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return "permission_denied"
	case errors.As(err, &network) && network.Timeout():
		return "timeout"
	}
	for _, item := range []struct{ text, code string }{
		{"unable to authenticate", "ssh_authentication_failed"},
		{"host key mismatch", "ssh_host_key_mismatch"},
		{"no common algorithm", "ssh_algorithm_negotiation_failed"},
	} {
		if strings.Contains(err.Error(), item.text) {
			return item.code
		}
	}
	return "operation_failed"
}

func (b *Client) deliveryStage(r *record, stage string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.records[r.Spec.Name] != r {
		return Err
	}
	if stage == "guest_exec" {
		if r.Delivery.Expired || time.Since(r.Delivery.StartedAt) >= b.deliveryBudget() {
			return context.DeadlineExceeded
		}
		r.Delivery.ExecutionPossible = true
	}
	r.Delivery.Stage = stage
	return b.save(r) // Never dispatch exec if the fence could not be saved.
}

func (b *Client) deliveryFailed(r *record, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || r.Delivered {
		return
	}
	r.Delivery.Cause = deliveryCause(err)
	if r.Delivery.ExecutionPossible {
		r.Delivery.Stage = "uncertain"
	}
	if e := b.save(r); e != nil {
		r.Delivery.Cause = "delivery_state_save_failed"
	}
	if r.Delivery.Attempts >= 3 || r.Delivery.ExecutionPossible {
		b.lastDeliveryFailure = deliveryError(r)
		b.deliveryRetryAt = time.Now().Add(30 * time.Second)
	}
}

func deliveryError(r *record) error {
	d := r.Delivery
	hint := ""
	if d.Cause == "EHOSTUNREACH" || d.Cause == "ENETUNREACH" || d.Cause == "permission_denied" {
		hint = "; check routing and macOS Local Network permission for the process launching Pulse (including terminal/tmux context)"
	}
	return fmt.Errorf("%w: VM %s stage=%s cause=%s attempts=%d last_attempt=%s%s", ErrDeliveryUnavailable,
		r.Spec.Name, d.Stage, d.Cause, d.Attempts, d.LastAttemptAt.Format(time.RFC3339), hint)
}

func (b *Client) deliverySucceeded(r *record) {
	r.Delivered = true
	r.Delivery.Stage, r.Delivery.Cause = "delivered", ""
	r.Delivery.LastSuccessAt = time.Now().UTC()
	if err := b.save(r); err != nil {
		r.Delivered = false
		r.Delivery.Cause = "delivery_state_save_failed"
		return
	}
	b.lastDeliveryFailure = nil
}

func (b *Client) deliveryMarker(name string) bool {
	f, err := os.Open(b.artifact(name, "events"))
	if err != nil {
		return false
	}
	defer f.Close()
	data, _ := io.ReadAll(io.LimitReader(f, 1<<20))
	for _, line := range bytes.Split(data, []byte("\n")) {
		if readyLine(line) {
			return true
		}
	}
	return false
}

func readyLine(line []byte) bool {
	return bytes.Equal(line, []byte("PULSE_HANDOFF_OK")) || bytes.Equal(line, []byte("PULSE_BASE_READY")) ||
		bytes.HasPrefix(line, []byte("PULSE_TEMPLATE_READY {")) || bytes.HasPrefix(line, []byte("PULSE_DOCKER_ENGINE_READY"))
}

type deliveryWriter struct {
	mu          sync.Mutex
	destination io.Writer
	delivered   func()
	line        []byte
	done        bool
}

func (w *deliveryWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.destination.Write(p)
	for _, c := range p[:n] {
		if w.done {
			break
		}
		if c == '\n' {
			if readyLine(w.line) {
				w.done = true
				w.delivered()
			}
			w.line = nil
		} else if len(w.line) < 4096 {
			w.line = append(w.line, c)
		}
	}
	return n, err
}

func (b *Client) DeliveryObservations(ctx context.Context) ([]DeliveryObservation, error) {
	if err := b.mu.LockContext(ctx); err != nil {
		return nil, err
	}
	defer b.mu.Unlock()
	out := []DeliveryObservation{}
	for name, r := range b.records {
		if r.Delivery != nil {
			out = append(out, DeliveryObservation{name, r.UID, *r.Delivery})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (b *Client) deliveryHealth() error {
	for _, r := range b.records {
		if r.Delivered || r.Delivery == nil {
			continue
		}
		d := r.Delivery
		if d.Expired || (d.Cause != "" && (d.Attempts >= 3 || d.ExecutionPossible)) {
			return deliveryError(r)
		}
	}
	return nil
}

func (b *Client) DeliveryHealth(ctx context.Context) error {
	if err := b.mu.LockContext(ctx); err != nil {
		return err
	}
	defer b.mu.Unlock()
	if err := b.deliveryHealth(); err != nil {
		return err
	}
	return b.lastDeliveryFailure // Cleared only by a real successful delivery.
}

// After expired VMs are cleaned up, permit a recovery attempt after cooldown.
// This is admission policy, not proof that connectivity has recovered.
func (b *Client) AdmissionHealth(ctx context.Context) error {
	if err := b.mu.LockContext(ctx); err != nil {
		return err
	}
	defer b.mu.Unlock()
	if err := b.deliveryHealth(); err != nil {
		return err
	}
	if time.Now().Before(b.deliveryRetryAt) {
		return b.lastDeliveryFailure
	}
	return nil
}
