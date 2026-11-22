package local

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/colony-2/pulse-macvz/internal/backend"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func preparedFixture(t *testing.T, b *Client, m *mockNative, name, uid, phase string) {
	t.Helper()
	spec := b.bases[0]
	if err := os.MkdirAll(b.path(name), 0700); err != nil {
		t.Fatal(err)
	}
	// Adopt a stopped source, then give the fixture unused builder ownership.
	m.mu.Lock()
	m.vms[name] = hardware{Name: name, CPU: 4, Memory: 8192, MACAddress: "02:00:00:00:00:01", State: "stopped"}
	m.mu.Unlock()
	s := backend.Spec{Name: name, OS: "darwin", CPU: 4, Memory: 8192, Image: spec.Image}
	_, err := b.AdoptSource(t.Context(), s, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.records[name]
	r.UID = uid
	r.Reusable = phase == "ready"
	r.Delivered = phase == "ready"
	r.Delivery = &DeliveryStatus{Expired: phase != "ready", Stage: "expired_before_exec"}
	if !r.Reusable {
		if err = b.binding(r, true); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(map[string]any{"scope": b.scope, "key": []byte(strings.Repeat("k", 32)), "image": map[string]string{"name": name, "uid": uid, "phase": phase, "kind": "base"}})
	b.properties[name] = raw
	if err = b.save(r); err != nil {
		t.Fatal(err)
	}
}

func TestPreparationRetryBudgetSurvivesDeletionAndRestart(t *testing.T) {
	b, m, _ := fixture(t)
	for n, uid := range []string{"one", "two", "three"} {
		preparedFixture(t, b, m, "base", uid, "failed")
		if n == 1 {
			b = reopenProperties(t, b, m, "")
		}
		pending, err := b.RetryPreparation(t.Context(), "base", uid)
		if err != nil || pending != (n < 2) {
			t.Fatal(n, pending, err)
		}
		_, err = os.Stat(b.path("base"))
		if (n < 2) != os.IsNotExist(err) {
			t.Fatal("incorrect replacement budget", n, err)
		}
		if _, err = os.Stat(filepath.Join(b.dir, "preparation-retries", uid+".json")); err != nil {
			t.Fatal("lost failure evidence", err)
		}
	}
}
func TestPreparationRetryRejectsAcceptedAndUncertainIdentity(t *testing.T) {
	for _, which := range []string{"job", "pair", "foreign", "uid", "quarantine", "ready"} {
		t.Run(which, func(t *testing.T) {
			b, m, _ := fixture(t)
			preparedFixture(t, b, m, "base", "one", "failed")
			var p map[string]any
			_ = json.Unmarshal(b.properties["base"], &p)
			uid := "one"
			switch which {
			case "job", "pair":
				p[which] = map[string]string{"id": "accepted"}
			case "foreign":
				p["scope"] = "someone-else"
			case "uid":
				uid = "replacement"
			case "quarantine", "ready":
				p["image"].(map[string]any)["phase"] = which
			}
			b.properties["base"], _ = json.Marshal(p)
			pending, err := b.RetryPreparation(t.Context(), "base", uid)
			if pending || (which != "ready" && err == nil) {
				t.Fatal("unsafe retry", pending, err)
			}
			if _, err = os.Stat(b.path("base")); err != nil {
				t.Fatal("protected VM removed", err)
			}
		})
	}
}
func TestPreparationRetryStopsFailedBuilderAndWaitsForDelivery(t *testing.T) {
	b, m, _ := fixture(t)
	preparedFixture(t, b, m, "base", "one", "failed")
	m.mu.Lock()
	h := m.vms["base"]
	h.State = "running"
	m.vms["base"] = h
	m.mu.Unlock()
	pending, err := b.RetryPreparation(t.Context(), "base", "one")
	if err != nil || !pending {
		t.Fatal(pending, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b.workers["base"] = true
	b.workerCancels = map[string]context.CancelFunc{"base": cancel}
	pending, err = b.RetryPreparation(t.Context(), "base", "one")
	if err != nil || !pending || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal(pending, err, ctx.Err())
	}
	if _, err = os.Stat(b.path("base")); err != nil {
		t.Fatal("deleted before worker stopped")
	}
	b.workers["base"] = false
	pending, err = b.RetryPreparation(t.Context(), "base", "one")
	if err != nil || !pending {
		t.Fatal(pending, err)
	}
}
func TestPreparationDoesNotRetryUncertainExecutionBeforeObservation(t *testing.T) {
	b, m, _ := fixture(t)
	preparedFixture(t, b, m, "base", "one", "preparing")
	r := b.records["base"]
	r.Delivery = &DeliveryStatus{ExecutionPossible: true, Stage: "uncertain"}
	r.Created = time.Now()
	if pending, err := b.RetryPreparation(t.Context(), "base", "one"); err != nil || pending {
		t.Fatal(pending, err)
	}
	b.basePreparations = map[string]*basePreparation{"base": {uid: "one", done: true, err: errors.New("guest failed")}}
	if pending, err := b.RetryPreparation(t.Context(), "base", "one"); err != nil || !pending {
		t.Fatal(pending, err)
	}
}

func TestRetirementCancelsUnusedPreparationObserver(t *testing.T) {
	b, m, _ := fixture(t)
	preparedFixture(t, b, m, "base", "one", "preparing")
	observed, cancel := context.WithCancel(t.Context())
	defer cancel()
	b.basePreparations = map[string]*basePreparation{"base": {uid: "one", ctx: observed, cancel: cancel}}
	if err := b.StopPreparation(t.Context(), "base", "one"); !errors.Is(err, backend.ErrBusy) || observed.Err() == nil {
		t.Fatal("observer not joined", err)
	}
	b.basePreparations["base"].done = true
	if err := b.StopPreparation(t.Context(), "base", "one"); err != nil {
		t.Fatal(err)
	}
}
