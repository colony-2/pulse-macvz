package servercli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse-macvz/guest/logstore"
	"github.com/colony-2/pulse-macvz/internal/launchhistory"
	"github.com/colony-2/pulse-macvz/internal/local"
)

func historyOptions(t *testing.T) (serverOptions, string) {
	t.Helper()
	o := statusOptions(t)
	o.logDirectory = t.TempDir()
	if e := o.resolvePool(); e != nil {
		t.Fatal(e)
	}
	conf := filepath.Join(t.TempDir(), "provider.json")
	if e := os.WriteFile(conf, []byte(`{"version":1,"flags":{}}`), 0600); e != nil {
		t.Fatal(e)
	}
	return o, conf
}
func TestLaunchCommandsFilterDetailsAndOldArchiveUnknownFields(t *testing.T) {
	o, conf := historyOptions(t)
	root := logRoot(o)
	now := time.Now().UTC().Truncate(time.Second)
	for i, id := range []string{"recent", "old"} {
		stamp := now.Add(-time.Duration(i) * 48 * time.Hour)
		r := launchhistory.Record{LaunchID: id, Tenant: "tenant-a", JobID: "job-" + id, SubmittedAt: &stamp, C2J: &local.ImageVersion{Version: "v1", Revision: "1234567890abcdef"}, Base: "ubuntu", VMUID: "immutable", Phase: "done"}
		if e := launchhistory.Save(root, r); e != nil {
			t.Fatal(e)
		}
	}
	dir := logstore.Dir(root, "legacy")
	if e := logstore.Atomic(dir, "identity.json", []byte(`{"launch_id":"legacy","vm_name":"gone","vm_uid":"old-uid","phase":"active","job":{"instance_id":"db","tenant_id":"tenant-b","job_id":"job-legacy","jobdb":"SECRET_URL"}}`)); e != nil {
		t.Fatal(e)
	}
	if e := logstore.Save(dir, logstore.Manifest{LaunchID: "legacy", Unavailable: true, Interrupted: true}); e != nil {
		t.Fatal(e)
	}
	run := func(args ...string) (string, error) {
		cmd := NewLaunchesCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append(args, "--vm-home", o.vmHome, "--log-directory", o.logDirectory, "--config", conf))
		e := cmd.ExecuteContext(t.Context())
		return out.String(), e
	}
	out, e := run("--json", "--since", "24h", "--tenant", "tenant-a", "--job", "job-recent", "--limit", "1")
	if e != nil {
		t.Fatal(out, e)
	}
	var report launchReport
	if e = json.Unmarshal([]byte(out), &report); e != nil || len(report.Launches) != 1 || report.Launches[0].LaunchID != "recent" {
		t.Fatal(out, e)
	}
	out, e = run("show", "legacy", "--json")
	if e != nil {
		t.Fatal(e)
	}
	var legacy launchView
	if e = json.Unmarshal([]byte(out), &legacy); e != nil || legacy.SubmittedAt != nil || legacy.C2J != nil || legacy.Logs.State != "helper unavailable" || strings.Contains(out, "SECRET_URL") {
		t.Fatal(out, e)
	}
	out, e = run("show", "recent")
	if e != nil || !strings.Contains(out, "Job ID: job-recent") || !strings.Contains(out, "c2j: v1") || !strings.Contains(out, "Logs: not retained") {
		t.Fatal(out, e)
	}
	out, e = run("--limit", "0")
	if e != nil || !strings.Contains(out, "3 shown") || !strings.Contains(out, "unknown") {
		t.Fatal(out, e)
	}
	for _, args := range [][]string{{"show", "missing"}, {"--limit", "-1"}, {"--since", "yesterday"}} {
		if out, e = run(args...); e == nil {
			t.Fatal("invalid command succeeded", args, out)
		}
	}
	// Read-only inspection must not migrate legacy metadata or write a config.
	if _, e = os.Stat(filepath.Join(root, "history", logstore.Key("legacy")+".json")); !os.IsNotExist(e) {
		t.Fatal("inspection modified legacy history", e)
	}
}
func TestHistoryScanWarningsAndOwnerIsolation(t *testing.T) {
	o, _ := historyOptions(t)
	root := logRoot(o)
	if e := launchhistory.Save(root, launchhistory.Record{LaunchID: "valid"}); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, "history", "bad.json"), []byte("broken"), 0600); e != nil {
		t.Fatal(e)
	}
	report, e := readLaunches(t.Context(), o)
	if e != nil || len(report.Launches) != 1 || len(report.Warnings) == 0 {
		t.Fatal(report, e)
	}
	o.owner = "another"
	if e = o.resolvePool(); e != nil {
		t.Fatal(e)
	}
	report, e = readLaunches(t.Context(), o)
	if e != nil || len(report.Launches) != 0 {
		t.Fatal(report, e)
	}
}

func TestLaunchHistoryReadableWithoutVMInventory(t *testing.T) {
	o, _ := historyOptions(t)
	if e := launchhistory.Save(logRoot(o), launchhistory.Record{LaunchID: "retained"}); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(o.vmHome, "vms"), []byte("not a directory"), 0600); e != nil {
		t.Fatal(e)
	}
	report, e := readLaunches(t.Context(), o)
	if e != nil || len(report.Launches) != 1 || len(report.Warnings) == 0 {
		t.Fatal(report, e)
	}
}
