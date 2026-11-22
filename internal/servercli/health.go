package servercli

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/colony-2/pulse-macvz/internal/local"
	"github.com/colony-2/pulse-macvz/internal/vz"
	"github.com/colony-2/pulse-macvz/internal/workerhealth"
	"github.com/spf13/cobra"
)

type launchHealthReport struct {
	LaunchID   string                   `json:"launch_id"`
	VM         string                   `json:"vm"`
	Tenant     string                   `json:"tenant"`
	JobID      string                   `json:"job_id"`
	ObservedAt time.Time                `json:"observed_at"`
	Monitor    *workerhealth.State      `json:"monitor,omitempty"`
	Probe      workerhealth.Observation `json:"probe"`
}

// NewLaunchHealthCommand probes an active worker without opening a controller
// or changing its monitoring/cleanup decisions.
func NewLaunchHealthCommand() *cobra.Command {
	var id string
	var asJSON bool
	var cmd *cobra.Command
	cmd = providerCommand("health", "Probe an active launch and show its recorded heartbeat state", func(ctx context.Context, o serverOptions) error {
		report, err := readLaunches(ctx, o)
		if err != nil {
			return err
		}
		var launch *launchView
		for i := range report.Launches {
			if report.Launches[i].LaunchID == id {
				launch = &report.Launches[i]
				break
			}
		}
		if launch == nil {
			return fmt.Errorf("launch %s not found", statusText(id))
		}
		store := &vz.Store{Home: o.vmHome}
		hw, err := store.Inspect(ctx, launch.VMName)
		if err != nil || !hw.Running {
			return fmt.Errorf("launch %s has no running VM; use launches show for retained history", statusText(id))
		}
		probe, err := local.ProbeHealth(ctx, o.vmHome, report.Scope, launch.VMName, launch.VMUID, launch.Tenant, launch.JobID, store.IP)
		if err != nil {
			return fmt.Errorf("cannot inspect launch health: %w", err)
		}
		h := launchHealthReport{LaunchID: id, VM: launch.VMName, Tenant: launch.Tenant, JobID: launch.JobID, ObservedAt: time.Now().UTC(), Monitor: launch.Health, Probe: probe}
		if asJSON {
			e := json.NewEncoder(cmd.OutOrStdout())
			e.SetIndent("", "  ")
			return e.Encode(h)
		}
		w := cmd.OutOrStdout()
		fmt.Fprintf(w, "Launch: %s\nVM: %s\nJob: %s / %s\n", statusText(id), statusText(h.VM), statusText(h.Tenant), statusText(h.JobID))
		if h.Monitor == nil {
			fmt.Fprintln(w, "Monitor: not enabled for this older launch")
		} else {
			fmt.Fprintf(w, "Monitor: %s (last checked %s)\n", statusText(h.Monitor.Status), h.Monitor.CheckedAt.Format(time.RFC3339))
			if h.Monitor.LastSuccess != nil {
				fmt.Fprintln(w, "Last successful probe: "+h.Monitor.LastSuccess.Format(time.RFC3339))
			}
			if h.Monitor.FailingSince != nil {
				fmt.Fprintf(w, "Failing since: %s (%d consecutive failures)\n", h.Monitor.FailingSince.Format(time.RFC3339), h.Monitor.Failures)
			}
			if h.Monitor.Reason != "" {
				fmt.Fprintln(w, "  "+statusText(h.Monitor.Reason))
			}
		}
		if !probe.Supported {
			fmt.Fprintln(w, "Probe: unsupported by this image; upgrade prepared images")
			return nil
		}
		if probe.Snapshot == nil {
			fmt.Fprintln(w, "Probe: "+probe.Failure)
			return nil
		}
		s := probe.Snapshot
		fmt.Fprintf(w, "Probe: healthy\nWorker started: %s\nCurrent task: %s (since %s)\n", s.StartedAt.Format(time.RFC3339), statusText(s.TaskType), s.CurrentTaskStartedAt.Format(time.RFC3339))
		if s.ExtensionOp != nil {
			fmt.Fprintln(w, "Extension: "+statusText(*s.ExtensionOp))
		}
		if s.LeaseExpiresAt != nil {
			fmt.Fprintln(w, "Last known lease expiry: "+s.LeaseExpiresAt.Format(time.RFC3339))
		}
		return nil
	})
	cmd.Use = "health LAUNCH_ID"
	cmd.Args = func(_ *cobra.Command, args []string) error {
		if len(args) != 1 || args[0] == "" {
			return fmt.Errorf("provide one launch ID")
		}
		id = args[0]
		return nil
	}
	cmd.Flags().BoolVarP(&asJSON, "json", "j", false, "Print the probe and recorded monitoring state as JSON")
	return cmd
}
