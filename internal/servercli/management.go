package servercli

import (
	"context"
	"errors"
	"fmt"
	"time"

	macvz "github.com/colony-2/pulse-macvz"
	"github.com/colony-2/pulse-macvz/internal/backend"
	"github.com/colony-2/pulse-macvz/internal/config"
	api "github.com/colony-2/pulse-provider-api"
)

type managedRuntime interface {
	Check(context.Context) error
	Reconcile(context.Context) error
	Drain(context.Context) error
	PurgeImages(context.Context) error
	Close() error
	Slots() ([]macvz.SlotStatus, error)
	Images() ([]macvz.ImageStatus, error)
	DockerAllocations() ([]macvz.DockerStatus, error)
	List(context.Context, api.ListRequest) (api.ListResponse, error)
}

type recoverableUpgrade interface {
	ImageUpgradeState() (string, error)
	BeginImageUpgrade(context.Context) error
	ActivateImageUpgrade(context.Context) error
	CommitImageUpgrade(context.Context) error
	RollbackImageUpgrade(context.Context) (bool, error)
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
			retrying := false
			if preparing {
				if repair, supported := r.(interface {
					RepairPreparations(context.Context) (bool, error)
				}); supported {
					var repairErr error
					retrying, repairErr = repair.RepairPreparations(ctx)
					if repairErr != nil {
						return repairErr
					}
				}
			}
			ok, err := false, error(nil)
			if !retrying {
				ok, err = ready()
			}
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
	rollback := func() error {
		recovery, ok := r.(recoverableUpgrade)
		if !ok {
			return errors.New("runtime cannot restore previous bases")
		}
		fmt.Fprintln(o.log, "Restoring previous ready base images")
		for {
			done, err := recovery.RollbackImageUpgrade(ctx)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(o.poll):
			}
		}
	}
	phase := ""
	if recovery, ok := r.(recoverableUpgrade); ok {
		phase, err = recovery.ImageUpgradeState()
		if err != nil {
			return err
		}
		if phase == "rollback" {
			if err = rollback(); err != nil {
				return err
			}
			phase = ""
			upgrade = false
		}
		if phase != "" {
			fmt.Fprintln(o.log, "Resuming interrupted image upgrade; previous bases remain checkpointed")
		}
	}
	if upgrade && phase == "" || phase == "staging" || phase == "staged" {
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
		if recovery, ok := r.(recoverableUpgrade); ok {
			fmt.Fprintln(o.log, "Checkpointing previous ready bases before replacement")
			if err = recovery.BeginImageUpgrade(ctx); err != nil {
				return err
			}
		} else {
			return errors.New("runtime cannot protect previous bases during upgrade")
		}
		fmt.Fprintln(o.log, "Retiring unused preparation VMs")
		for {
			err = r.PurgeImages(ctx)
			if err == nil {
				break
			}
			if !errors.Is(err, backend.ErrBusy) {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(o.poll):
			}
		}
		if recovery, ok := r.(recoverableUpgrade); ok {
			if err = recovery.ActivateImageUpgrade(ctx); err != nil {
				return err
			}
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
		// Cancellation preserves partial progress. A terminal build failure rolls
		// back saved sources, then warms the pool through the ordinary setup path.
		var failed *preparationFailure
		if errors.As(err, &failed) && ctx.Err() == nil {
			if recovery, ok := r.(recoverableUpgrade); ok {
				state, stateErr := recovery.ImageUpgradeState()
				if stateErr != nil {
					return errors.Join(err, stateErr)
				}
				if state != "" {
					original := err
					if err = rollback(); err != nil {
						return errors.Join(original, fmt.Errorf("rollback incomplete; rerun setup: %w", err))
					}
					if err = wait("Preparing configured capacity", func() (bool, error) { return capacityReady(o.config, r) }); err != nil {
						return errors.Join(original, err)
					}
					return fmt.Errorf("upgrade failed; previous bases restored and capacity is ready: %w", original)
				}
			}
		}
		return err
	}
	if recovery, ok := r.(recoverableUpgrade); ok {
		if err = recovery.CommitImageUpgrade(ctx); err != nil {
			return err
		}
	}
	fmt.Fprintln(o.log, "Configured capacity is ready")
	return nil
}

// preparationFailure distinguishes terminal branch failures from unsafe
// inventory/identity observations. Only the former may trigger automatic rollback.
type preparationFailure struct{ err error }

func (e *preparationFailure) Error() string { return e.err.Error() }
func (e *preparationFailure) Unwrap() error { return e.err }

func capacityReady(c macvz.Config, r managedRuntime) (bool, error) {
	images, err := r.Images()
	if err != nil {
		return false, err
	}
	slots, err := r.Slots()
	if err != nil {
		return false, err
	}
	pairs, err := r.DockerAllocations()
	if err != nil {
		return false, err
	}
	failedVM := map[string]string{}
	failedSources := map[string]bool{}
	if observer, ok := r.(interface {
		PreparationProgress() (macvz.PreparationProgress, error)
	}); ok {
		progress, err := observer.PreparationProgress()
		if err != nil {
			return false, err
		}
		for _, source := range progress.Sources {
			if source.Stage == "failed" {
				failedSources[source.Source] = true
			}
		}
		for _, v := range progress.VMs {
			if v.Stage == "base_failed" {
				failedVM[v.Name] = vmStage("preparing", v)
			}
		}
	}
	var failures []error
	pending := false
	blocked := map[string]bool{}
	for _, image := range images {
		if image.Obsolete {
			continue
		}
		if image.Phase == "ready" {
			continue
		}
		detail := failedVM[image.Name]
		if detail != "" || image.Phase == "failed" || image.Phase == "quarantine" {
			failures = append(failures, fmt.Errorf("image preparation failed: %s (%s): %s", image.Name, image.Phase, detail))
			blocked[image.Base] = true
		} else {
			pending = true
		}
	}
	readyBase := map[string]bool{}
	for _, image := range images {
		if image.Phase == "ready" && !image.Obsolete {
			readyBase[image.Base] = true
		}
	}
	neededBase := map[string]bool{}
	for _, g := range c.Slots {
		if g.Count > 0 {
			neededBase[g.Base] = true
		}
	}
	for _, base := range c.Bases {
		if neededBase[base.Name] && !readyBase[base.Name] && failedSources[base.Image] {
			blocked[base.Name] = true
			failures = append(failures, fmt.Errorf("registry preparation failed for base %s", base.Name))
		}
	}
	macBases := map[string]bool{}
	for _, b := range c.Bases {
		macBases[b.Name] = b.OS != "linux"
	}
	byImage := map[string]macvz.ImageStatus{}
	for _, image := range images {
		byImage[image.Name] = image
	}
	companions := map[string]string{}
	for _, g := range c.Slots {
		if g.Count > 0 && macBases[g.Base] && g.DockerProfile != "" {
			ep := c.Engine(g.DockerProfile)
			companions[config.EngineBaseImageName(c.OwnerID, ep)] = ep.Image
		}
	}
	for name, source := range companions {
		if _, present := byImage[name]; present {
			continue
		}
		if failedSources[source] {
			failures = append(failures, fmt.Errorf("registry preparation failed for companion %s", name))
		} else {
			pending = true
		}
	}
	bySlot := map[string]macvz.SlotStatus{}
	for _, slot := range slots {
		bySlot[slot.ID] = slot
	}
	byPair := map[string]macvz.DockerStatus{}
	for _, pair := range pairs {
		byPair[pair.Slot] = pair
	}
	linuxWanted, linuxReady := 0, 0
	for _, g := range c.Slots {
		for i := 1; i <= g.Count; i++ {
			id := fmt.Sprintf("%s-%04d", g.Name, i)
			if blocked[g.Base] {
				continue
			}
			if macBases[g.Base] {
				slot := bySlot[id]
				if slot.Phase == "ready" {
					continue
				}
				if slot.Busy {
					pending = true
					continue
				}
				if detail := failedVM[slot.Template]; detail != "" || slot.Phase == "failed" || slot.Phase == "quarantine" {
					failures = append(failures, fmt.Errorf("template preparation failed: %s (%s): %s", id, slot.Phase, detail))
				} else {
					pending = true
				}
			} else if g.DockerProfile != "" {
				pair := byPair[id]
				if pair.Phase == "quarantine" {
					failures = append(failures, fmt.Errorf("allocation preparation failed: %s", id))
					continue
				}
				linuxWanted++
				if pair.Phase == "ready" || pair.Phase == "running" {
					linuxReady++
				}
			}
		}
	}
	pending = pending || linuxReady < min(linuxWanted, *c.VMCounts.Linux, *c.VMCounts.Total)
	// A terminal failure must not abandon healthy builders still making progress.
	if pending {
		return false, nil
	}
	if len(failures) > 0 {
		return false, &preparationFailure{errors.Join(failures...)}
	}
	return true, nil
}
