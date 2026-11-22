package servercli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse-macvz/internal/config"
	"github.com/colony-2/pulse-macvz/internal/launchhistory"
	"github.com/colony-2/pulse-macvz/internal/local"
	"github.com/colony-2/pulse-macvz/internal/workerhealth"
)

func TestLaunchHealthReadOnlyAndStructuredOutput(t *testing.T) {
	o, conf := historyOptions(t)
	before, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	scope := (config.Settings{Config: o.config, Kind: "vz", Endpoint: o.vmHome}).Scope()
	dir := writeStatusFixture(t, o.vmHome, scope, "worker", map[string]any{}, true)
	version, _ := json.Marshal(local.ImageVersion{Version: "v1"})
	if err = os.WriteFile(filepath.Join(dir, local.ImageVersionFile), version, 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	h := launchhistory.Record{LaunchID: "active", VMName: "worker", VMUID: "worker-uid", Tenant: "tenant", JobID: "job", ObservedAt: &now, Health: &workerhealth.State{Status: "unsupported", CheckedAt: now}}
	if err = launchhistory.Save(logRoot(o), h); err != nil {
		t.Fatal(err)
	}
	for _, asJSON := range []bool{false, true} {
		cmd := NewLaunchesCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		args := []string{"health", "active", "--vm-home", o.vmHome, "--log-directory", o.logDirectory, "--config", conf}
		if asJSON {
			args = append(args, "--json")
		}
		cmd.SetArgs(args)
		if err = cmd.ExecuteContext(t.Context()); err != nil {
			t.Fatal(out.String(), err)
		}
		if strings.Contains(out.String(), "SECRET") {
			t.Fatal("private metadata leaked", out.String())
		}
		if asJSON {
			var r launchHealthReport
			if err = json.Unmarshal(out.Bytes(), &r); err != nil || r.LaunchID != "active" || r.Probe.Supported || r.Monitor == nil {
				t.Fatal(out.String(), err)
			}
		} else if !strings.Contains(out.String(), "Probe: unsupported") || !strings.Contains(out.String(), "Job: tenant / job") {
			t.Fatal(out.String())
		}
	}
	after, err := os.ReadFile(conf)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("inspection changed configuration", err)
	}
	h.LaunchID = "deleted"
	h.VMName = "gone"
	h.VMUID = "gone-uid"
	if err = launchhistory.Save(logRoot(o), h); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"missing", "deleted"} {
		cmd := NewLaunchesCommand()
		cmd.SetArgs([]string{"health", id, "--vm-home", o.vmHome, "--log-directory", o.logDirectory, "--config", conf})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		if err = cmd.ExecuteContext(t.Context()); err == nil {
			t.Fatal("inactive health request succeeded", id)
		}
	}
}
