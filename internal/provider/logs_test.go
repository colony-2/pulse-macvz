package provider

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/colony-2/pulse-macvz/guest/logstore"
	"github.com/colony-2/pulse-macvz/internal/backend"
)

type logBackend struct {
	Backend
	vm     backend.VM
	output []byte
	calls  int
}

func (b *logBackend) Get(context.Context, string) (backend.VM, error) { return b.vm, nil }
func (b *logBackend) Exec(_ context.Context, name, uid string, script []byte) ([]byte, error) {
	b.calls++
	if uid != b.vm.UID || !strings.Contains(string(script), "logs <<") {
		return nil, errors.New("bad read")
	}
	return b.output, nil
}
func TestLogReadsRequireImmutableIdentityAndVersion(t *testing.T) {
	b := &logBackend{vm: backend.VM{UID: "vm-1"}, output: []byte("PULSE_LOG_UNAVAILABLE\n")}
	p := &Provider{Backend: b}
	target := LogTarget{LaunchID: "launch", Name: "vm", UID: "wrong"}
	if _, err := p.Logs(t.Context(), target, 0); !errors.Is(err, ErrLogsGone) || b.calls != 0 {
		t.Fatal("read reused VM", err, b.calls)
	}
	target.UID = "vm-1"
	if _, err := p.Logs(t.Context(), target, 0); !errors.Is(err, ErrLogsUnavailable) {
		t.Fatal(err)
	}
	page := logstore.Page{Manifest: logstore.Manifest{Version: 1, LaunchID: "other", End: 1}, Next: 1}
	b.output, _ = json.Marshal(page)
	if _, err := p.Logs(t.Context(), target, 0); !errors.Is(err, logstore.ErrJournal) {
		t.Fatal("wrong assignment", err)
	}
	page.Manifest.LaunchID = "launch"
	b.output, _ = json.Marshal(page)
	if _, err := p.Logs(t.Context(), target, 2); !errors.Is(err, logstore.ErrJournal) {
		t.Fatal("regressing cursor", err)
	}
	page.Next = 0
	page.Manifest.End = 0
	b.output, _ = json.Marshal(page)
	if _, err := p.Logs(t.Context(), target, 0); err != nil {
		t.Fatal(err)
	}
}

func TestLayoutSelectionPrecedesHandoff(t *testing.T) {
	script := startup([]byte(`{}`))
	version := strings.Index(script, "pulse-tart-guest version")
	bootstrap := strings.Index(script, "/bin/bash /opt/pulse-tart-image/bootstrap-guest.sh")
	ingest := strings.Index(script, "pulse-tart-guest ingest")
	if version < 0 || version > bootstrap || version > ingest {
		t.Fatal("layout validated after execution handoff")
	}
	if !strings.Contains(guestStateScript, "test ! -e /var/db/pulse-macvz") || !strings.Contains(guestStateScript, "test ! -e /var/db/pulse-tart") {
		t.Fatal("ambiguous worker layout accepted")
	}
}
