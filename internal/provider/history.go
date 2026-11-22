package provider

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/colony-2/pulse-macvz/internal/launchhistory"
	"github.com/colony-2/pulse-macvz/internal/local"
)

// EnableHistory installs an observational journal hook. It never participates
// in admission or recovery: a history disk error must not stop accepted work.
func (p *Provider) EnableHistory() {
	p.Store.mu.Lock()
	defer p.Store.mu.Unlock()
	warned := false
	p.Store.onSave = func() {
		failed := false
		for _, r := range p.Store.data.Records {
			now := time.Now().UTC()
			h := launchhistory.Record{Health: r.Health, LaunchID: r.LaunchID, InstanceID: r.Job.InstanceID, Tenant: r.Job.TenantID, JobID: r.Job.JobID, VMName: r.Name, VMUID: r.UID, Slot: r.SlotID, Base: r.Base, Admission: string(r.Outcome), Phase: r.Phase, ObservedAt: &now}
			if !r.Created.IsZero() {
				stamp := r.Created
				h.SubmittedAt = &stamp
			}
			if h.Base == "" {
				if pair := p.Store.data.Pairs[r.PairID]; pair != nil {
					h.Base = pair.Base
				}
			}
			if r.Phase == "done" {
				h.CleanupObservedAt = &now
			}
			// The version sidecar belongs to this immutable VM, not today's base image.
			if p.Config.Kind == "vz" && r.UID != "" && filepath.Base(r.Name) == r.Name {
				dir := filepath.Join(p.Config.Endpoint, "vms", r.Name)
				var identity struct {
					Scope  string
					Native struct{ UID string }
				}
				data, e := os.ReadFile(filepath.Join(dir, "pulse.json"))
				if e == nil && json.Unmarshal(data, &identity) == nil && identity.Scope == p.Config.Scope() && identity.Native.UID == r.UID {
					h.C2J, _ = local.ReadImageVersion(dir)
				}
			}
			if e := launchhistory.Save(launchhistory.Root(p.Config), h); e != nil {
				failed = true
			}
		}
		if failed && !warned {
			slog.Warn("launch history could not be saved; VM operation continues")
		}
		warned = failed
	}
	p.Store.onSave()
}
