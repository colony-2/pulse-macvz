package servercli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	tart "github.com/colony-2/pulse-tart"
)

type progressRow struct{ section, name, phase, detail string }

type preparationReporter struct {
	started, printed time.Time
	signature        string
	interactive      bool
	terminalSize     func() (int, int, error)
	frame            []string
	width, height    int
	previous         map[string]string
}

func (p *preparationReporter) print(w io.Writer, c tart.Config, r managedRuntime, force bool) error {
	now := time.Now()
	if p.started.IsZero() {
		p.started = now
	}
	rows, err := preparationRows(c, r, now)
	if err != nil {
		return err
	}
	var signature strings.Builder
	for _, row := range rows {
		fmt.Fprintf(&signature, "%s:%s\n", row.name, row.phase)
	}
	interval := 10 * time.Second
	if p.interactive {
		interval = time.Second
	}
	if !force && signature.String() == p.signature && now.Sub(p.printed) < interval {
		return nil
	}
	if err := p.render(w, rows, now.Sub(p.started), force); err != nil {
		return err
	}
	p.signature, p.printed = signature.String(), now
	return nil
}

func preparationRows(c tart.Config, r managedRuntime, now time.Time) ([]progressRow, error) {
	images, err := r.Images()
	if err != nil {
		return nil, err
	}
	slots, err := r.Slots()
	if err != nil {
		return nil, err
	}
	pairs, err := r.DockerAllocations()
	if err != nil {
		return nil, err
	}
	var progress tart.PreparationProgress
	if observer, ok := r.(interface {
		PreparationProgress() (tart.PreparationProgress, error)
	}); ok {
		progress, err = observer.PreparationProgress()
		if err != nil {
			return nil, err
		}
	}
	sources := map[string]tart.SourceProgress{}
	for _, s := range progress.Sources {
		sources[s.Source] = s
	}
	vms := map[string]tart.VMProgress{}
	for _, v := range progress.VMs {
		vms[v.Name] = v
	}
	bases := map[string]tart.ImageStatus{}
	for _, image := range images {
		if !image.Obsolete && image.Base != "" {
			bases[image.Base] = image
		}
	}
	templates := map[string]tart.SlotStatus{}
	for _, slot := range slots {
		templates[slot.ID] = slot
	}
	allocations := map[string]tart.DockerStatus{}
	for _, pair := range pairs {
		allocations[pair.Slot] = pair
	}
	wanted := map[string]bool{}
	for _, group := range c.Slots {
		if group.Count > 0 {
			wanted[group.Base] = true
		}
	}
	var rows []progressRow
	baseOS := map[string]string{}
	for _, base := range c.Bases {
		baseOS[base.Name] = base.OS
		if !wanted[base.Name] {
			continue
		}
		image, present := bases[base.Name]
		row := progressRow{section: "Base images", name: base.Name, phase: "queued", detail: "waiting to prepare base"}
		if present {
			row.phase = image.Phase
			row.detail = vmStage(image.Phase, vms[image.Name])
			row.phase += ":" + vms[image.Name].Stage + ":" + vms[image.Name].Cause
		} else if source, ok := sources[base.Image]; ok {
			row.phase = source.Stage
			row.detail = transferDetail(source, now)
		}
		rows = append(rows, row)
	}
	// Separate Docker companion software bases have no configured base label.
	for _, image := range images {
		if image.Companion && !image.Obsolete {
			v := vms[image.Name]
			rows = append(rows, progressRow{"Base images", "companion " + image.Name, image.Phase + ":" + v.Stage, vmStage(image.Phase, v)})
		}
	}
	for _, g := range c.Slots {
		for i := 1; i <= g.Count; i++ {
			id := fmt.Sprintf("%s-%04d", g.Name, i)
			row := progressRow{section: "Slots", name: id + " [" + g.Base + "]", phase: "waiting_base", detail: "waiting for base " + g.Base}
			if bases[g.Base].Phase == "ready" {
				row.phase, row.detail = "waiting_capacity", "waiting for a preparation VM position"
			}
			if slot, ok := templates[id]; ok {
				v := vms[slot.Template]
				row.phase, row.detail = slot.Phase+":"+v.Stage+":"+v.Cause, vmStage(slot.Phase, v)
				if slot.Phase == "ready" {
					row.detail = "ready (suspended template)"
				}
			}
			if g.DockerProfile != "" {
				if pair, ok := allocations[id]; ok {
					row.phase, row.detail = pair.Phase, "allocation: "+pair.Phase
					for _, member := range []struct{ label, name string }{{"primary", pair.Primary}, {"engine", pair.Engine}} {
						if v, ok := vms[member.name]; ok && pair.Phase != "ready" && pair.Phase != "running" {
							row.detail += "; " + member.label + " " + vmStage("preparing", v)
							row.phase += ":" + v.Stage + ":" + v.Cause
						}
					}
				} else if baseOS[g.Base] == "linux" && bases[g.Base].Phase == "ready" {
					row.phase, row.detail = "waiting_allocation", "source ready; waiting for shared Linux capacity"
				}
			}
			rows = append(rows, row)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	return rows, nil
}

func vmStage(phase string, v tart.VMProgress) string {
	if phase == "ready" || phase == "failed" || phase == "quarantine" {
		return phase
	}
	if phase == "suspending" {
		return "saving suspended VM snapshot"
	}
	stage := map[string]string{
		"creating": "cloning source image", "starting": "starting VM", "started": "waiting for guest",
		"observe_vm": "waiting for VM startup", "discover_ip": "discovering guest IP",
		"tcp": "connecting to guest SSH", "ssh_handshake": "authenticating guest SSH",
		"ssh_session": "opening guest session", "guest_exec": "running guest preparation command (ETA unavailable)",
		"delivered":           "guest command completed; waiting for readiness / shutdown",
		"expired_before_exec": "guest connection deadline expired", "uncertain": "guest execution outcome uncertain",
	}[v.Stage]
	if stage == "" {
		stage = phase
	}
	if v.Cause != "" {
		stage += "; last connection result: " + v.Cause
	}
	return stage
}

func transferDetail(s tart.SourceProgress, now time.Time) string {
	switch s.Stage {
	case "cached":
		return "registry cache hit; waiting to prepare guest"
	case "ready":
		return "registry import complete; waiting to prepare guest"
	case "queued", "waiting_cache":
		return "waiting for registry cache/import lock"
	case "manifest":
		return "fetching image manifest; total and ETA not yet known"
	case "failed":
		return "registry import failed"
	case "verifying":
		return fmt.Sprintf("downloaded %s / %s; verifying and publishing image", size(s.Current), size(s.Total))
	}
	total := "unknown total"
	if s.Total > 0 {
		total = size(s.Total)
	}
	detail := fmt.Sprintf("downloading/importing %s / %s", size(s.Current), total)
	if s.Total > 0 {
		detail += fmt.Sprintf(" (%.1f%%)", min(100, 100*float64(s.Current)/float64(s.Total)))
	}
	if !s.Updated.IsZero() && now.Sub(s.Updated) >= 15*time.Second {
		return detail + "; no new transfer data for " + elapsed(now.Sub(s.Updated)) + "; ETA unavailable"
	}
	seconds := now.Sub(s.Started).Seconds()
	if seconds < 1 || s.Current <= 0 {
		return detail + "; estimating rate and ETA"
	}
	rate := float64(s.Current) / seconds
	detail += fmt.Sprintf("; %s/s avg", size(int64(rate)))
	if s.Total > s.Current {
		detail += "; ETA ~" + elapsed(time.Duration(float64(s.Total-s.Current)/rate*float64(time.Second)))
	} else if s.Total > 0 {
		detail += "; finishing verification"
	} else {
		detail += "; ETA unavailable"
	}
	return detail
}

func size(n int64) string {
	if n >= 1<<30 {
		return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
	}
	if n >= 1<<20 {
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	if n >= 1<<10 {
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func elapsed(d time.Duration) string { return d.Round(time.Second).String() }
