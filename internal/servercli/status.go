package servercli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/colony-2/pulse-macvz/internal/config"
	"github.com/colony-2/pulse-macvz/internal/local"
	engine "github.com/colony-2/pulse-macvz/internal/provider"
	"github.com/colony-2/pulse-macvz/internal/vz"
	"github.com/spf13/cobra"
)

// NewStatusCommand reads atomic per-VM metadata and native lock state. It never
// opens a Runtime: doing so would acquire the controller lock and write state.
func NewStatusCommand() *cobra.Command {
	return newInspectionCommand("status", "Show slot availability, assignments, and c2j versions", printPoolStatus)
}

// NewImagesCommand shows the detailed inventory without starting or modifying VMs.
func NewImagesCommand() *cobra.Command {
	return newInspectionCommand("images", "Show worker VMs, base images, and cached templates", printPoolImages)
}

func newInspectionCommand(name, short string, printReport func(io.Writer, poolStatus) error) *cobra.Command {
	var asJSON bool
	var cmd *cobra.Command
	cmd = providerCommand(name, short, func(ctx context.Context, o serverOptions) error {
		report, err := readPoolStatus(ctx, o)
		if err != nil {
			return err
		}
		if asJSON {
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			return encoder.Encode(report)
		}
		return printReport(cmd.OutOrStdout(), report)
	})
	cmd.Flags().BoolVarP(&asJSON, "json", "j", false, "Print the full status snapshot as JSON")
	return cmd
}

type poolStatus struct {
	ObservedAt time.Time     `json:"observed_at"`
	Owner      string        `json:"owner"`
	Home       string        `json:"home"`
	Scope      string        `json:"scope"`
	Slots      []*slotStatus `json:"slots"`
	Images     []imageStatus `json:"images"`
	Notes      []string      `json:"notes"`
	Warnings   []string      `json:"warnings,omitempty"`
}
type slotStatus struct {
	ID                     string             `json:"id"`
	Base                   string             `json:"base,omitempty"`
	Configured             bool               `json:"configured"`
	State                  string             `json:"state"`
	Template               *statusVM          `json:"template,omitempty"`
	Allocations            []statusAllocation `json:"allocations,omitempty"`
	Jobs                   []statusJob        `json:"jobs,omitempty"`
	LastRecordedSubmission *time.Time         `json:"last_recorded_submission,omitempty"`
}
type statusVM struct {
	C2J      *local.ImageVersion   `json:"c2j,omitempty"`
	Name     string                `json:"name"`
	UID      string                `json:"uid"`
	OS       string                `json:"os,omitempty"`
	Power    string                `json:"power"`
	Phase    string                `json:"recorded_phase,omitempty"`
	Created  time.Time             `json:"created"`
	Delivery *local.DeliveryStatus `json:"delivery,omitempty"`
}
type statusAllocation struct {
	ID        string    `json:"id"`
	Phase     string    `json:"phase"`
	LaunchID  string    `json:"launch_id,omitempty"`
	Created   time.Time `json:"created"`
	Primary   *statusVM `json:"primary,omitempty"`
	Companion *statusVM `json:"companion,omitempty"`
}
type statusJob struct {
	LaunchID   string    `json:"launch_id"`
	InstanceID string    `json:"jobdb_instance"`
	TenantID   string    `json:"tenant"`
	JobID      string    `json:"job_id"`
	Phase      string    `json:"recorded_phase"`
	Outcome    string    `json:"recorded_outcome"`
	RecordedAt time.Time `json:"submission_recorded_at"`
	VM         *statusVM `json:"vm,omitempty"`
}
type imageStatus struct {
	Kind string    `json:"kind"`
	VM   *statusVM `json:"vm"`
}

// Only decode fields needed for status; never marshal raw pulse.json, native
// Spec, PairRecord, or Record into public output (they can contain private data).
type statusFile struct {
	Version int
	Scope   string
	Native  struct {
		UID      string
		Created  time.Time
		Spec     struct{ Name, OS string }
		Delivery *local.DeliveryStatus
	}
	Properties struct {
		Version  int
		Scope    string
		Revision int64
		Slot     *engine.SlotRecord
		Image    *engine.ImageRecord
		Job      *engine.Record
		Pair     *struct {
			ID, SlotID, Base, Phase, LaunchID, PrimaryUID, EngineUID string
			Created                                                  time.Time
			Primary, Engine                                          struct{ Name string }
		}
	}
}

func readPoolStatus(ctx context.Context, o serverOptions) (poolStatus, error) {
	scope := (config.Settings{Config: o.config, Kind: "vz", Endpoint: o.vmHome}).Scope()
	out := poolStatus{ObservedAt: time.Now().UTC(), Owner: o.owner, Home: o.vmHome, Scope: scope, Slots: []*slotStatus{}, Images: []imageStatus{}, Notes: []string{
		"Read-only snapshot; persisted phases may lag the running controller. In-memory preparation/download progress is not available here.",
		"Last recorded submission is the newest surviving job record, not a historical last-use time. Deleted jobs and declined submissions are not retained.",
	}}
	slots := map[string]*slotStatus{}
	getSlot := func(id string) *slotStatus {
		if s := slots[id]; s != nil {
			return s
		}
		s := &slotStatus{ID: id, State: "not prepared"}
		slots[id] = s
		return s
	}
	for _, g := range o.config.Slots {
		for i := 1; i <= g.Count; i++ {
			s := getSlot(fmt.Sprintf("%s-%04d", g.Name, i))
			s.Base = g.Base
			s.Configured = true
		}
	}
	entries, err := os.ReadDir(filepath.Join(o.vmHome, "vms"))
	if err != nil && !os.IsNotExist(err) {
		return out, err
	}
	files := map[string]statusFile{}
	vms := map[string]*statusVM{}
	store := &vz.Store{Home: o.vmHome}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		path := filepath.Join(o.vmHome, "vms", name, "pulse.json")
		info, e := os.Lstat(path)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return out, e
		}
		if !info.Mode().IsRegular() {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s: metadata is not a regular file", name))
			continue
		}
		data, e := readFile(path, 16<<20)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s: metadata unavailable", name))
			continue
		}
		var f statusFile
		if json.Unmarshal(data, &f) != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s: invalid metadata", name))
			continue
		}
		if f.Scope != scope {
			continue
		}
		if f.Version != 1 || f.Native.UID == "" || f.Native.Spec.Name != name || f.Properties.Version != 1 || f.Properties.Scope != scope {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s: incomplete or mismatched identity metadata", name))
			continue
		}
		vm := &statusVM{Name: name, UID: f.Native.UID, OS: f.Native.Spec.OS, Power: "unknown", Created: f.Native.Created, Delivery: f.Native.Delivery}
		imageVersion, versionErr := local.ReadImageVersion(filepath.Join(o.vmHome, "vms", name))
		vm.C2J = imageVersion
		if versionErr != nil && !os.IsNotExist(versionErr) {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s: c2j version metadata unavailable", name))
		}
		hw, e := store.Inspect(ctx, name)
		if e == nil {
			vm.Power = hw.State
		} else {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s: native state unavailable (VM may have changed during scan)", name))
		}
		files[name] = f
		vms[name] = vm
	}
	lookup := func(name, uid string) *statusVM {
		if name == "" {
			return nil
		}
		if vm := vms[name]; vm != nil && vm.UID == uid {
			return vm
		}
		out.Warnings = append(out.Warnings, fmt.Sprintf("%s: referenced VM missing or identity changed", name))
		return &statusVM{Name: name, UID: uid, Power: "unknown"}
	}
	// A pair is replicated on both members. Use the newest complete observation,
	// including its job, so a stale companion cannot resurrect an old assignment.
	pairs := map[string]statusFile{}
	jobs := map[string]*engine.Record{}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f := files[name]
		p := f.Properties
		vm := vms[name]
		if s := p.Slot; s != nil && s.Name == name && s.UID == vm.UID {
			copy := *vm
			copy.Phase = s.Phase
			row := getSlot(s.ID)
			row.Template = &copy
			row.State = "template " + s.Phase
			if s.Phase == "ready" {
				if vm.Power == "suspended" {
					row.State = "ready"
				} else {
					row.State = "needs reconciliation"
				}
			}
		}
		if i := p.Image; i != nil && i.Name == name && i.UID == vm.UID {
			copy := *vm
			copy.Phase = i.Phase
			out.Images = append(out.Images, imageStatus{Kind: i.Kind, VM: &copy})
		}
		if p.Pair != nil {
			pair := p.Pair
			if (name != pair.Primary.Name || vm.UID != pair.PrimaryUID) && (name != pair.Engine.Name || vm.UID != pair.EngineUID) {
				out.Warnings = append(out.Warnings, name+": allocation identity mismatch")
				continue
			}
			old, ok := pairs[pair.ID]
			if !ok || p.Revision > old.Properties.Revision {
				pairs[pair.ID] = f
			}
		} else if r := p.Job; r != nil && r.Name == name && r.UID == vm.UID {
			jobs[r.LaunchID] = r
		}
	}
	ids := make([]string, 0, len(pairs))
	for id := range pairs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		f := pairs[id]
		p := f.Properties.Pair
		s := getSlot(p.SlotID)
		if s.Base == "" {
			s.Base = p.Base
		}
		a := statusAllocation{ID: p.ID, Phase: p.Phase, LaunchID: p.LaunchID, Created: p.Created, Primary: lookup(p.Primary.Name, p.PrimaryUID), Companion: lookup(p.Engine.Name, p.EngineUID)}
		s.Allocations = append(s.Allocations, a)
		if p.LaunchID != "" {
			s.State = "assigned (" + p.Phase + ")"
		} else if p.Phase == "ready" && a.Primary != nil && a.Primary.Power == "running" && (a.Companion == nil || a.Companion.Power == "running") {
			s.State = "idle"
		} else {
			s.State = "allocation " + p.Phase
			if p.Phase == "ready" {
				s.State = "needs reconciliation"
			}
		}
		if r := f.Properties.Job; r != nil && r.PairID == p.ID && r.LaunchID == p.LaunchID {
			jobs[r.LaunchID] = r
		}
	}
	ids = ids[:0]
	for id := range jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		r := jobs[id]
		s := getSlot(r.SlotID)
		j := statusJob{LaunchID: r.LaunchID, InstanceID: r.Job.InstanceID, TenantID: r.Job.TenantID, JobID: r.Job.JobID, Phase: r.Phase, Outcome: string(r.Outcome), RecordedAt: r.Created, VM: lookup(r.Name, r.UID)}
		s.Jobs = append(s.Jobs, j)
		if !r.Created.IsZero() && (s.LastRecordedSubmission == nil || r.Created.After(*s.LastRecordedSubmission)) {
			stamp := r.Created
			s.LastRecordedSubmission = &stamp
		}
		if r.Phase != "done" {
			s.State = "assigned"
			if j.VM.Power != "running" {
				s.State = "assigned; " + j.VM.Power + " (awaiting reconciliation)"
			}
		}
	}
	ids = ids[:0]
	for id := range slots {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s := slots[id]
		if len(s.Allocations) > 1 {
			s.State = "multiple allocations; inspect details"
		}
		out.Slots = append(out.Slots, s)
	}
	return out, nil
}

func statusText(s string) string {
	if s == "" {
		return "-"
	}
	// Keep job identifiers and any diagnostic strings from injecting terminal controls.
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, s)
}
func statusTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}
func printPoolStatus(w io.Writer, r poolStatus) error {
	fmt.Fprintf(w, "Pool owner=%s home=%s (observed %s)\n", statusText(r.Owner), statusText(r.Home), statusTime(r.ObservedAt))
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SLOT\tBASE\tSTATE\tLAST RECORDED SUBMISSION (UTC)\tC2J VERSION\tTENANT\tJOB ID\tLAUNCH ID")
	for _, s := range r.Slots {
		stamp := "-"
		if s.LastRecordedSubmission != nil {
			stamp = statusTime(*s.LastRecordedSubmission)
		}
		if len(s.Jobs) == 0 {
			launch := "-"
			for _, a := range s.Allocations {
				if a.LaunchID != "" {
					launch = a.LaunchID
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t-\t-\t%s\n", statusText(s.ID), statusText(s.Base), statusText(s.State), stamp, slotC2J(s), statusText(launch))
		}
		for _, j := range s.Jobs {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", statusText(s.ID), statusText(s.Base), statusText(s.State), stamp, vmC2J(j.VM), statusText(j.TenantID), statusText(j.JobID), statusText(j.LaunchID))
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w, "\nStates: ready = suspended template available; idle = running worker available; assigned = job attached.")
	fmt.Fprintln(w, "VM and image details: pulse-macvz images")
	return printStatusNotes(w, r)
}

func printPoolImages(w io.Writer, r poolStatus) error {
	fmt.Fprintf(w, "Pool owner=%s home=%s (observed %s)\n", statusText(r.Owner), statusText(r.Home), statusTime(r.ObservedAt))
	seen := map[string]bool{}
	printVM := func(role string, vm *statusVM) {
		if vm == nil || seen[vm.Name] {
			return
		}
		seen[vm.Name] = true
		fmt.Fprintf(w, "    %s: %s\n", statusText(role), statusText(vm.Power))
		fmt.Fprintf(w, "      VM:      %s\n", statusText(vm.Name))
		version, revision := "unknown", "unknown"
		if v := vm.C2J; v != nil {
			if v.Version != "" {
				version = v.Version
			}
			if v.Revision != "" {
				revision = v.Revision
				if len(revision) > 7 {
					revision = revision[:7]
				}
			}
		}
		fmt.Fprintf(w, "      c2j:     %s (commit %s)\n", statusText(version), statusText(revision))
		fmt.Fprintf(w, "      Created: %s\n", statusTime(vm.Created))
		if vm.Phase != "" {
			fmt.Fprintf(w, "      Phase:   %s\n", statusText(vm.Phase))
		}
		if d := vm.Delivery; d != nil {
			fmt.Fprintf(w, "      Startup: %s; %d attempt(s); last %s\n", statusText(d.Stage), d.Attempts, statusTime(d.LastAttemptAt))
			if d.Cause != "" {
				fmt.Fprintf(w, "      Cause:   %s\n", statusText(d.Cause))
			}
		}
	}
	fmt.Fprintln(w, "\nSlot VMs")
	for _, s := range r.Slots {
		fmt.Fprintf(w, "\n  %s [%s] — %s\n", statusText(s.ID), statusText(s.Base), statusText(s.State))
		if len(s.Jobs) == 0 && len(s.Allocations) == 0 && s.Template == nil {
			fmt.Fprintln(w, "    No VM recorded")
		}
		for _, j := range s.Jobs {
			printVM("Job worker", j.VM)
		}
		for _, a := range s.Allocations {
			printVM("Worker", a.Primary)
			printVM("Docker companion", a.Companion)
		}
		printVM("Reusable template", s.Template)
	}
	for _, group := range []struct {
		title string
		base  bool
	}{{"Base images", true}, {"Other cached images and templates", false}} {
		heading := false
		for _, i := range r.Images {
			if i.VM == nil || seen[i.VM.Name] || (i.Kind == "base") != group.base {
				continue
			}
			if !heading {
				fmt.Fprintf(w, "\n%s\n", group.title)
				heading = true
			}
			fmt.Fprintln(w)
			printVM(i.Kind, i.VM)
		}
	}
	fmt.Fprintln(w, "\nStartup describes script delivery, not job progress. Unknown c2j metadata is not inferred from the configured version.")
	return printStatusNotes(w, r)
}

func printStatusNotes(w io.Writer, r poolStatus) error {
	for _, n := range r.Notes {
		fmt.Fprintln(w, "Note:", n)
	}
	for _, n := range r.Warnings {
		fmt.Fprintln(w, "Warning:", statusText(n))
	}
	return nil
}

func vmC2J(vm *statusVM) string {
	if vm == nil || vm.C2J == nil {
		return "unknown"
	}
	version := vm.C2J.Version
	if version == "" {
		version = "unknown"
	}
	if revision := vm.C2J.Revision; revision != "" {
		if len(revision) > 7 {
			revision = revision[:7]
		}
		version += " (" + revision + ")"
	}
	return statusText(version)
}

// Use the worker actually backing the slot, never a newer template to describe
// an older worker. Missing worker provenance must remain unknown.
func slotC2J(s *slotStatus) string {
	if len(s.Allocations) == 0 {
		return vmC2J(s.Template)
	}
	var versions []string
	seen := map[string]bool{}
	for _, a := range s.Allocations {
		version := vmC2J(a.Primary)
		if !seen[version] {
			versions = append(versions, version)
			seen[version] = true
		}
	}
	return strings.Join(versions, ", ")
}
