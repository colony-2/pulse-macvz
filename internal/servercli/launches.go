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

	"github.com/colony-2/pulse-macvz/guest/logstore"
	"github.com/colony-2/pulse-macvz/internal/config"
	"github.com/colony-2/pulse-macvz/internal/launchhistory"
	engine "github.com/colony-2/pulse-macvz/internal/provider"
	"github.com/spf13/cobra"
)

type launchView struct {
	launchhistory.Record
	Logs logInfo `json:"logs"`
}
type logInfo struct {
	State   string     `json:"state"`
	End     uint64     `json:"end_offset"`
	Updated *time.Time `json:"updated,omitempty"`
	Loss    bool       `json:"loss_reported"`
}
type launchReport struct {
	Owner    string       `json:"owner"`
	Scope    string       `json:"scope"`
	Launches []launchView `json:"launches"`
	Warnings []string     `json:"warnings,omitempty"`
}

func logRoot(o serverOptions) string {
	return launchhistory.Root(config.Settings{Config: o.config, Kind: "vz", Endpoint: o.vmHome})
}

func NewLaunchesCommand() *cobra.Command {
	cmd := launchCommand(false)
	cmd.AddCommand(launchCommand(true))
	return cmd
}
func launchCommand(detail bool) *cobra.Command {
	var tenant, job, since, id string
	var limit int
	var asJSON bool
	var cmd *cobra.Command
	name, short := "launches", "List retained launch history, including launches whose VMs were deleted"
	if detail {
		name, short = "show", "Show launch identity, timestamps, image version, and log availability"
	}
	cmd = providerCommand(name, short, func(ctx context.Context, o serverOptions) error {
		var after time.Time
		if since != "" {
			var e error
			after, e = parseSince(since, time.Now())
			if e != nil {
				return e
			}
		}
		if limit < 0 {
			return fmt.Errorf("--limit must be zero (all) or positive")
		}
		report, e := readLaunches(ctx, o)
		if e != nil {
			return e
		}
		filtered := make([]launchView, 0, len(report.Launches))
		for _, r := range report.Launches {
			if id != "" && r.LaunchID != id || tenant != "" && r.Tenant != tenant || job != "" && r.JobID != job {
				continue
			}
			if !after.IsZero() && (r.SubmittedAt == nil || r.SubmittedAt.Before(after)) {
				continue
			}
			filtered = append(filtered, r)
		}
		if detail && len(filtered) == 0 {
			return fmt.Errorf("launch %s not found in this owner's retained history or current VMs", id)
		}
		if !detail && limit > 0 && len(filtered) > limit {
			filtered = filtered[:limit]
		}
		report.Launches = filtered
		if asJSON {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			if detail {
				return enc.Encode(filtered[0])
			}
			return enc.Encode(report)
		}
		for _, w := range report.Warnings {
			fmt.Fprintln(cmd.ErrOrStderr(), "Warning:", w)
		}
		if detail {
			return printLaunch(cmd.OutOrStdout(), filtered[0])
		}
		return printLaunches(cmd.OutOrStdout(), report)
	})
	if detail {
		cmd.Use = "show LAUNCH_ID"
		cmd.Args = func(_ *cobra.Command, args []string) error {
			if len(args) != 1 || args[0] == "" {
				return fmt.Errorf("provide one launch ID")
			}
			id = args[0]
			return nil
		}
	}
	cmd.Flags().BoolVarP(&asJSON, "json", "j", false, "Print structured metadata as JSON")
	if !detail {
		cmd.Flags().StringVar(&tenant, "tenant", "", "Filter by exact tenant")
		cmd.Flags().StringVar(&job, "job", "", "Filter by exact job ID")
		cmd.Flags().StringVar(&since, "since", "", "Submissions since an RFC3339 timestamp or duration ago (e.g. 24h)")
		cmd.Flags().IntVar(&limit, "limit", 100, "Maximum results, newest submission first (0: all)")
	}
	return cmd
}
func parseSince(value string, now time.Time) (time.Time, error) {
	if t, e := time.Parse(time.RFC3339, value); e == nil {
		return t, nil
	}
	if d, e := time.ParseDuration(value); e == nil && d >= 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("--since must be an RFC3339 timestamp or nonnegative duration, such as 24h")
}

func readLaunches(ctx context.Context, o serverOptions) (launchReport, error) {
	root := logRoot(o)
	report := launchReport{Owner: o.owner, Scope: filepath.Base(root), Launches: []launchView{}}
	records := map[string]launchhistory.Record{}
	merge := func(r launchhistory.Record) {
		old, e := launchhistory.Merge(records[r.LaunchID], r)
		if e != nil {
			report.Warnings = append(report.Warnings, "conflicting launch identity: "+statusText(r.LaunchID))
			return
		}
		records[r.LaunchID] = old
	}
	entries, e := os.ReadDir(filepath.Join(root, "history"))
	if e != nil && !os.IsNotExist(e) {
		return report, e
	}
	for _, entry := range entries {
		if e = ctx.Err(); e != nil {
			return report, e
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := launchhistory.ReadJSON(filepath.Join(root, "history", entry.Name()))
		var r launchhistory.Record
		if err != nil || json.Unmarshal(data, &r) != nil || r.Version != 1 || r.LaunchID == "" || entry.Name() != logstore.Key(r.LaunchID)+".json" {
			report.Warnings = append(report.Warnings, "invalid launch history file: "+statusText(entry.Name()))
			continue
		}
		merge(r)
	}
	// Legacy archives remain readable without fabricating missing submission dates.
	entries, e = os.ReadDir(root)
	if e != nil && !os.IsNotExist(e) {
		return report, e
	}
	for _, entry := range entries {
		if e = ctx.Err(); e != nil {
			return report, e
		}
		if !entry.IsDir() || len(entry.Name()) != 64 {
			continue
		}
		data, err := launchhistory.ReadJSON(filepath.Join(root, entry.Name(), "identity.json"))
		if err != nil {
			continue
		}
		var t engine.LogTarget
		if json.Unmarshal(data, &t) != nil || t.LaunchID == "" || entry.Name() != logstore.Key(t.LaunchID) {
			report.Warnings = append(report.Warnings, "invalid archived launch identity: "+entry.Name())
			continue
		}
		merge(launchhistory.Record{LaunchID: t.LaunchID, VMName: t.Name, VMUID: t.UID, InstanceID: t.Job.InstanceID, Tenant: t.Job.TenantID, JobID: t.Job.JobID, Phase: t.Phase})
	}
	live, e := readPoolStatus(ctx, o)
	if e != nil {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		report.Warnings = append(report.Warnings, "current VM metadata unavailable; showing retained history only")
		live = poolStatus{}
	}
	report.Warnings = append(report.Warnings, live.Warnings...)
	for _, s := range live.Slots {
		for _, j := range s.Jobs {
			r := launchhistory.Record{LaunchID: j.LaunchID, InstanceID: j.InstanceID, Tenant: j.TenantID, JobID: j.JobID, Base: j.Base, Slot: s.ID, Phase: j.Phase, Admission: j.Outcome, ObservedAt: &live.ObservedAt}
			if !j.RecordedAt.IsZero() {
				t := j.RecordedAt
				r.SubmittedAt = &t
			}
			if j.VM != nil {
				r.VMName = j.VM.Name
				r.VMUID = j.VM.UID
				r.C2J = j.VM.C2J
			}
			merge(r)
		}
	}
	for _, r := range records {
		info, err := readLogInfo(root, r.LaunchID)
		if err != nil {
			info.State = "unreadable"
			report.Warnings = append(report.Warnings, "log metadata unavailable: "+statusText(r.LaunchID))
		}
		report.Launches = append(report.Launches, launchView{Record: r, Logs: info})
	}
	sort.Slice(report.Launches, func(i, j int) bool {
		a, b := report.Launches[i], report.Launches[j]
		if a.SubmittedAt == nil && b.SubmittedAt != nil {
			return false
		}
		if a.SubmittedAt != nil && b.SubmittedAt == nil {
			return true
		}
		if a.SubmittedAt != nil && !a.SubmittedAt.Equal(*b.SubmittedAt) {
			return a.SubmittedAt.After(*b.SubmittedAt)
		}
		return a.LaunchID < b.LaunchID
	})
	return report, nil
}
func readLogInfo(root, id string) (logInfo, error) {
	m, e := logstore.Load(logstore.Dir(root, id), id)
	if os.IsNotExist(e) {
		return logInfo{State: "not retained"}, nil
	}
	if e != nil {
		return logInfo{}, e
	}
	state := "open"
	if m.Closed {
		state = "closed"
	}
	if m.Interrupted {
		state = "interrupted"
	}
	if m.Unavailable {
		state = "helper unavailable"
	}
	return logInfo{State: state, End: m.End, Updated: &m.Updated, Loss: m.Lost}, nil
}
func historyTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
}
func historyText(v string) string {
	if v == "" {
		return "unknown"
	}
	return statusText(v)
}
func printLaunches(w io.Writer, r launchReport) error {
	fmt.Fprintf(w, "Launch history: owner=%s; %d shown\n", statusText(r.Owner), len(r.Launches))
	if len(r.Launches) == 0 {
		_, e := fmt.Fprintln(w, "No retained launch history.")
		return e
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SUBMITTED (UTC)\tTENANT\tJOB ID\tC2J VERSION\tBASE\tLOGS\tLAUNCH ID")
	for _, l := range r.Launches {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", historyTime(l.SubmittedAt), historyText(l.Tenant), historyText(l.JobID), vmC2J(&statusVM{C2J: l.C2J}), historyText(l.Base), l.Logs.State, statusText(l.LaunchID))
	}
	if e := tw.Flush(); e != nil {
		return e
	}
	_, e := fmt.Fprintln(w, "Unknown fields were not retained. Log closure does not imply job success.")
	return e
}
func printLaunch(w io.Writer, r launchView) error {
	_, e := fmt.Fprintf(w, "Launch: %s\n  Submitted: %s\n  Tenant: %s\n  Job ID: %s\n  JobDB instance: %s\n  c2j: %s\n  Base: %s\n  Slot: %s\n  VM: %s\n  VM UID: %s\n  Recorded admission: %s\n  Last recorded phase: %s\n  Metadata observed: %s\n  VM cleanup observed: %s\n  Logs: %s\n  Log end offset: %d bytes\n  Logs updated: %s\n  Log loss reported: %t\n", statusText(r.LaunchID), historyTime(r.SubmittedAt), historyText(r.Tenant), historyText(r.JobID), historyText(r.InstanceID), vmC2J(&statusVM{C2J: r.C2J}), historyText(r.Base), historyText(r.Slot), historyText(r.VMName), historyText(r.VMUID), historyText(r.Admission), historyText(r.Phase), historyTime(r.ObservedAt), historyTime(r.CleanupObservedAt), r.Logs.State, r.Logs.End, historyTime(r.Logs.Updated), r.Logs.Loss)
	return e
}
