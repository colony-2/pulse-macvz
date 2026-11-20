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
		progress := time.Now()
		for {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("%s: %w; accepted jobs were not cancelled", stage, err)
			}
			if err := r.Reconcile(ctx); err != nil {
				return err
			}
			ok, err := ready()
			if err != nil {
				return err
			}
			if ok {
				return nil
			}
			if time.Since(progress) >= 30*time.Second {
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
	for _, image := range images {
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
	ordinary := map[string]bool{}
	for _, g := range c.Slots {
		if g.DockerProfile == "" {
			for i := 1; i <= g.Count; i++ {
				ordinary[fmt.Sprintf("%s-%04d", g.Name, i)] = true
			}
		}
	}
	ready := 0
	for _, slot := range slots {
		if slot.Phase == "failed" || slot.Phase == "quarantine" {
			return false, fmt.Errorf("template preparation failed: %s", slot.ID)
		}
		if ordinary[slot.ID] && slot.Phase == "ready" {
			ready++
		}
	}
	for _, pair := range pairs {
		if pair.Phase == "quarantine" {
			return false, fmt.Errorf("allocation preparation failed: %s", pair.Slot)
		}
		if pair.Phase == "ready" || pair.Phase == "running" {
			ready++
		}
	}
	return ready >= min(c.Capacity(), *c.VMCounts.Total), nil
}
