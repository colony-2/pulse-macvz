package servercli

import (
	"context"
	"errors"
	"fmt"
	"time"

	api "github.com/colony-2/pulse-provider-api"
	tart "github.com/colony-2/pulse-tart"
)

type managedRuntime interface {
	Check(context.Context) error
	Reconcile(context.Context) error
	Drain(context.Context) error
	PurgeImages(context.Context) error
	Close() error
	Slots() ([]tart.SlotStatus, error)
	Images() ([]tart.ImageStatus, error)
	DockerAllocations() ([]tart.DockerStatus, error)
	List(context.Context, api.ListRequest) (api.ListResponse, error)
}

func openManaged(o serverOptions) (managedRuntime, error) {
	r, err := openRuntime(o)
	if err != nil {
		return nil, err
	}
	return r.(managedRuntime), nil
}

func runSetup(ctx context.Context, o serverOptions) error   { return manage(ctx, o, false, openManaged) }
func runUpgrade(ctx context.Context, o serverOptions) error { return manage(ctx, o, true, openManaged) }

func manage(parent context.Context, o serverOptions, upgrade bool, open func(serverOptions) (managedRuntime, error)) (err error) {
	if o.managementTimeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(parent, o.managementTimeout)
	defer cancel()
	fmt.Fprintf(o.log, "Pool owner=%s backend=%s home=%s; shared running limits: macOS=%d Linux=%d total=%d\n", o.config.OwnerID, o.backend, o.vmHome, *o.config.VMCounts.MacOS, *o.config.VMCounts.Linux, *o.config.VMCounts.Total)
	r, err := open(o)
	if err != nil {
		return fmt.Errorf("open provider for maintenance (stop its server first): %w", err)
	}
	defer func() {
		if r != nil {
			err = errors.Join(err, r.Close())
		}
	}()
	if err = r.Check(ctx); err != nil {
		return err
	}
	if o.backend == "vz" && o.config.NeedsDockerEndpoint() {
		if err = registerApplication(o.runner); err != nil {
			return err
		}
	}
	wait := func(stage string, ready func() (bool, error)) error {
		fmt.Fprintln(o.log, stage)
		reporter := newPreparationReporter(o.log, o.progressMode)
		preparing := stage == "Preparing configured capacity"
		if preparing {
			if err := reporter.print(o.log, o.config, r, true); err != nil {
				return err
			}
		}
		progress := time.Now()
		for {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("%s: %w; accepted jobs were not cancelled", stage, err)
			}
			reconcile := r.Reconcile
			if maintenance, ok := r.(interface{ ReconcileMaintenance(context.Context) error }); ok {
				reconcile = maintenance.ReconcileMaintenance
			}
			if err := reconcile(ctx); err != nil {
				if preparing {
					_ = reporter.print(o.log, o.config, r, true)
				}
				return err
			}
			ok, err := ready()
			if err != nil {
				if preparing {
					_ = reporter.print(o.log, o.config, r, true)
				}
				return err
			}
			if preparing {
				if err := reporter.print(o.log, o.config, r, ok); err != nil {
					return err
				}
			}
			if ok {
				return nil
			}
			if !preparing && time.Since(progress) >= 30*time.Second {
				fmt.Fprintln(o.log, "Still waiting:", stage)
				progress = time.Now()
			}
			select {
			case <-ctx.Done():
			case <-time.After(o.poll):
			}
		}
	}
	if upgrade {
		if err = r.Drain(ctx); err != nil {
			return err
		}
		if err = wait("Waiting for accepted jobs and allocations to drain", func() (bool, error) {
			slots, err := r.Slots()
			if err != nil {
				return false, err
			}
			pairs, err := r.DockerAllocations()
			if err != nil {
				return false, err
			}
			jobs, err := r.List(ctx, api.ListRequest{PageSize: 1})
			if err != nil {
				return false, err
			}
			return len(slots) == 0 && len(pairs) == 0 && len(jobs.Items) == 0, nil
		}); err != nil {
			return err
		}
		if err = r.PurgeImages(ctx); err != nil {
			return err
		}
		closed := r
		r = nil
		if err = closed.Close(); err != nil {
			return err
		}
		r, err = open(o)
		if err != nil {
			return err
		}
		if err = r.Check(ctx); err != nil {
			return err
		}
	}
	if err = wait("Preparing configured capacity", func() (bool, error) { return capacityReady(o.config, r) }); err != nil {
		return err
	}
	fmt.Fprintln(o.log, "Configured capacity is ready")
	return nil
}

func capacityReady(c tart.Config, r managedRuntime) (bool, error) {
	images, err := r.Images()
	if err != nil {
		return false, err
	}
	// A failed guest observation belongs to its base; it must not prevent
	// reconciliation of unrelated ready capacity. Setup still fails explicitly.
	if observer, ok := r.(interface {
		PreparationProgress() (tart.PreparationProgress, error)
	}); ok {
		progress, err := observer.PreparationProgress()
		if err != nil {
			return false, err
		}
		wanted := map[string]bool{}
		for _, image := range images {
			if !image.Obsolete && image.Phase != "ready" {
				wanted[image.Name] = true
			}
		}
		for _, vm := range progress.VMs {
			if wanted[vm.Name] && vm.Stage == "base_failed" {
				return false, fmt.Errorf("image preparation failed: %s: %s", vm.Name, vmStage("preparing", vm))
			}
		}
	}
	for _, image := range images {
		if image.Obsolete {
			continue
		}
		if image.Phase == "failed" || image.Phase == "quarantine" {
			return false, fmt.Errorf("image preparation failed: %s", image.Name)
		}
		if image.Phase != "ready" {
			return false, nil
		}
	}
	slots, err := r.Slots()
	if err != nil {
		return false, err
	}
	pairs, err := r.DockerAllocations()
	if err != nil {
		return false, err
	}
	// Every configured macOS version needs its suspended templates, even when
	// their combined count exceeds the shared running-VM limit.
	macBases := map[string]bool{}
	for _, b := range c.Bases {
		macBases[b.Name] = b.OS != "linux"
	}
	wanted := map[string]bool{}
	dockerSlots := map[string]bool{}
	linuxWanted := 0
	for _, g := range c.Slots {
		for i := 1; i <= g.Count; i++ {
			id := fmt.Sprintf("%s-%04d", g.Name, i)
			if macBases[g.Base] {
				wanted[id] = true
			}
			if g.DockerProfile != "" && !macBases[g.Base] {
				dockerSlots[id] = true
			}
		}
		if g.DockerProfile != "" {
			if !macBases[g.Base] {
				linuxWanted += g.Count
			}
		}
	}
	for _, slot := range slots {
		if slot.Phase == "failed" || slot.Phase == "quarantine" {
			return false, fmt.Errorf("template preparation failed: %s", slot.ID)
		}
		if slot.Phase == "ready" {
			delete(wanted, slot.ID)
		}
	}
	if len(wanted) != 0 {
		return false, nil
	}
	ready := map[string]bool{}
	for _, pair := range pairs {
		if pair.Phase == "quarantine" {
			return false, fmt.Errorf("allocation preparation failed: %s", pair.Slot)
		}
		if dockerSlots[pair.Slot] && (pair.Phase == "ready" || pair.Phase == "running") {
			ready[pair.Slot] = true
		}
	}
	// macOS templates stay suspended. Linux workers warm with Docker stopped.
	warm := min(linuxWanted, *c.VMCounts.Linux, *c.VMCounts.Total)
	return len(ready) >= warm, nil
}
