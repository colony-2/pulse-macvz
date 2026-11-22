package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colony-2/pulse-macvz/internal/launchhistory"
	"github.com/colony-2/pulse-macvz/internal/local"
	api "github.com/colony-2/pulse-provider-api"
)

func TestHistorySnapshotsAdmissionAndVersionBeforeVMCleanup(t *testing.T) {
	p, _ := dockerFixture(t, true)
	p.Config.Logging.Directory = t.TempDir()
	p.Config.Endpoint = t.TempDir()
	warmPlain(t, p)
	var pair *PairRecord
	for _, v := range p.Store.data.Pairs {
		pair = v
	}
	dir := filepath.Join(p.Config.Endpoint, "vms", pair.Primary.Name)
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(map[string]any{"scope": p.Config.Scope(), "native": map[string]any{"uid": pair.PrimaryUID}})
	if e := os.WriteFile(filepath.Join(dir, "pulse.json"), raw, 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, local.ImageVersionFile), []byte(`{"version":"unstable-1234567","revision":"1234567"}`), 0600); e != nil {
		t.Fatal(e)
	}
	p.EnableHistory()
	l := launch("history")
	out, e := p.Submit(t.Context(), []api.Launch{l})
	if e != nil || out[0].Status != api.Accepted {
		t.Fatal(out, e)
	}
	root := launchhistory.Root(p.Config)
	h, e := launchhistory.Read(root, "history")
	if e != nil || h.C2J == nil || h.C2J.Revision != "1234567" || h.SubmittedAt == nil || h.JobID != l.Job.JobID || h.Tenant != l.Job.TenantID || h.Base != pair.Base || h.Admission != "accepted" {
		t.Fatal(h, e)
	}
	data, _ := json.Marshal(h)
	if strings.Contains(string(data), l.Job.JobDB) || strings.Contains(string(data), "lease") {
		t.Fatal("private fields in history", string(data))
	}
	if e := os.RemoveAll(dir); e != nil {
		t.Fatal(e)
	}
	p.Store.mu.Lock()
	p.Store.data.Records["history"].Phase = "done"
	e = p.Store.save()
	delete(p.Store.data.Records, "history")
	p.Store.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	h, e = launchhistory.Read(root, "history")
	if e != nil || h.C2J == nil || h.CleanupObservedAt == nil || h.Phase != "done" {
		t.Fatal(h, e)
	}
}
func TestHistoryFailureDoesNotStopAdmission(t *testing.T) {
	p, _ := dockerFixture(t, true)
	warmPlain(t, p)
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if e := os.WriteFile(file, []byte("x"), 0600); e != nil {
		t.Fatal(e)
	}
	p.Config.Logging.Directory = file
	p.EnableHistory()
	out, e := p.Submit(t.Context(), []api.Launch{launch("admit")})
	if e != nil || out[0].Status != api.Accepted {
		t.Fatal(out, e)
	}
}
