// Package macvz provides macOS and Linux ARM64 concurrency slots through
// Virtualization.framework. It implements the Pulse provider API as an
// in-process Go library.
package macvz

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/colony-2/pulse-macvz/internal/backend"
	"github.com/colony-2/pulse-macvz/internal/bundle"
	"github.com/colony-2/pulse-macvz/internal/config"
	"github.com/colony-2/pulse-macvz/internal/launchlogs"
	"github.com/colony-2/pulse-macvz/internal/local"
	engine "github.com/colony-2/pulse-macvz/internal/provider"
	"github.com/colony-2/pulse-macvz/internal/releases"
	api "github.com/colony-2/pulse-provider-api"
)

type Config = config.Config
type LoggingConfig = config.Logging
type Base = config.Base
type SlotGroup = config.SlotGroup
type VMCounts = config.VMCounts
type DockerConfig = config.Docker
type DockerEngineProfile = config.EngineProfile
type DockerEndpoint = config.Endpoint
type Secret = config.Secret
type SlotStatus = engine.SlotStatus
type ImageStatus = engine.ImageStatus
type DeliveryObservation = local.DeliveryObservation

const DefaultCPU = config.DefaultCPU
const DefaultMemoryMiB = config.DefaultMemoryMiB
const DefaultMacOSVMCount = config.DefaultMacOSVMCount
const DefaultLinuxVMCount = config.DefaultLinuxVMCount

var ErrConfig = config.Err
var ErrClosed = errors.New("provider closed")
var _ api.Provider = (*Runtime)(nil)
var _ api.ProviderWithLaunchLogs = (*Runtime)(nil)

// Runtime owns one backend/owner scope exclusively. Run or Reconcile prepares slots
// and cleans up completed instances. GetCapabilities, Submit and List implement
// the shared API.
// Close releases local resources; running VMs survive it. A new Runtime using
// the same VZ home and owner recovers properties from existing VM directories.
// Never run two hosts for
// the same backend/owner scope.
type Runtime struct {
	logs        *launchlogs.Manager
	archive     *launchlogs.Archive
	loggingOnce sync.Once
	mu          sync.RWMutex
	p           *engine.Provider
	closed      bool
	period      time.Duration
	profile     string
}

func prepare(c Config) (Config, error) {
	return ResolveConfig(c)
}

func newRuntime(p *engine.Provider, c Config) *Runtime {
	r := &Runtime{p: p, period: c.PollInterval, profile: "default"}
	if c.Docker != nil {
		r.profile = "docker"
	}
	p.Assets = assets
	p.BaseScript, _ = assets.ReadFile("scripts/prepare-base.sh")
	var setup []byte
	p.Setup = func(ctx context.Context) ([]byte, error) {
		if setup != nil {
			return setup, nil
		}
		revision, err := releases.Resolve(ctx, c.C2JVersion)
		if err != nil {
			return nil, err
		}
		setup, err = bundle.Build(assets, r.profile, revision)
		return setup, err
	}
	r.initLogging()
	return r
}

func (r *Runtime) Submit(ctx context.Context, launches []api.Launch, options api.SubmitOptions) ([]api.Submission, api.LogSubscription, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		m := launchlogs.New(nil)
		m.Close()
		return nil, m.Subscribe(launches, options), ErrClosed
	}
	r.initLogging()
	subscription := r.logs.Subscribe(launches, options)
	results, err := r.p.Submit(ctx, launches)
	r.logs.Admitted(subscription, results, err)
	return results, subscription, err
}

// GetCapabilities returns the complete configured catalog in Config.Bases order.
// It does not inspect slot availability or contact the backend. Returned slices
// belong to the caller; no SSH credentials or image configuration are exposed.
func (r *Runtime) GetCapabilities(ctx context.Context) (api.Capabilities, error) {
	if err := ctx.Err(); err != nil {
		return api.Capabilities{}, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return api.Capabilities{}, err
	}
	if r.closed {
		return api.Capabilities{}, ErrClosed
	}
	out := api.Capabilities{BasesComplete: true, Bases: make([]api.BaseCapabilities, 0, len(r.p.Config.Bases))}
	for _, b := range r.p.Config.Bases {
		out.Bases = append(out.Bases, api.BaseCapabilities{Name: b.Name, Capabilities: append([]string{}, r.p.Config.BaseCapabilities(b.Name)...)})
	}
	return out, nil
}
func (r *Runtime) List(ctx context.Context, q api.ListRequest) (api.ListResponse, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return api.ListResponse{}, ErrClosed
	}
	return r.p.List(ctx, q)
}

// GetLaunchLog retrieves retained host output without affecting execution.
func (r *Runtime) GetLaunchLog(ctx context.Context, q api.LaunchLogRequest) (api.LaunchLogResponse, error) {
	if err := ctx.Err(); err != nil {
		return api.LaunchLogResponse{}, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return api.LaunchLogResponse{}, api.ErrProviderClosed
	}
	return r.archive.GetLaunchLog(ctx, q)
}
func (r *Runtime) Check(ctx context.Context) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return ErrClosed
	}
	return r.p.Check(ctx)
}
func (r *Runtime) Slots() ([]SlotStatus, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrClosed
	}
	return r.p.Slots()
}

func (r *Runtime) Images() ([]ImageStatus, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrClosed
	}
	return r.p.Images()
}

type PreparationProgress = backend.PreparationProgress
type SourceProgress = backend.SourceProgress
type VMProgress = backend.VMProgress

// PreparationProgress returns bounded, credential-free observations. Backends
// without transfer telemetry return an empty snapshot; no readiness is inferred.
func (r *Runtime) PreparationProgress() (PreparationProgress, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return PreparationProgress{}, ErrClosed
	}
	if b, ok := r.p.Backend.(interface {
		PreparationProgress() backend.PreparationProgress
	}); ok {
		return b.PreparationProgress(), nil
	}
	return PreparationProgress{}, nil
}
func (r *Runtime) initLogging() {
	r.loggingOnce.Do(func() {
		r.p.EnableHistory()
		c, _ := r.p.Config.Logging.Normalize()
		r.archive = launchlogs.NewArchive(r.p, c)
		r.logs = launchlogs.New(r.archive.Read)
	})
}

func (r *Runtime) Reconcile(ctx context.Context) error {
	return r.reconcile(ctx, true)
}

// ReconcileMaintenance advances cleanup and preparation without treating a
// retained delivery-health observation as a reconciliation failure. Setup must
// still check required Images/Slots/allocation readiness before reporting success.
// Recovery and reconciliation errors remain fatal; DeliveryHealth stays available.
func (r *Runtime) ReconcileMaintenance(ctx context.Context) error {
	return r.reconcile(ctx, false)
}

func (r *Runtime) reconcile(ctx context.Context, reportHealth bool) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return ErrClosed
	}
	r.initLogging()
	var recovery error
	if b, ok := r.p.Backend.(interface{ Recover(context.Context) error }); ok {
		recovery = b.Recover(ctx)
	}
	// Health failures must not prevent the reconciliation that releases expired
	// owned instances. The host can report this error while continuing maintenance.
	reconciled := r.p.Reconcile(ctx)
	var health error
	if b, ok := r.p.Backend.(interface{ DeliveryHealth(context.Context) error }); ok && reportHealth {
		health = b.DeliveryHealth(ctx)
	}
	return errors.Join(recovery, reconciled, health)
}

// DeliveryHealth reports observed delivery failures, independently of controller
// liveness. A nil result does not certify connectivity when no guest was probed.
func (r *Runtime) DeliveryHealth(ctx context.Context) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return ErrClosed
	}
	if b, ok := r.p.Backend.(interface{ DeliveryHealth(context.Context) error }); ok {
		return b.DeliveryHealth(ctx)
	}
	return nil
}

func (r *Runtime) DeliveryObservations(ctx context.Context) ([]DeliveryObservation, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrClosed
	}
	if b, ok := r.p.Backend.(interface {
		DeliveryObservations(context.Context) ([]local.DeliveryObservation, error)
	}); ok {
		return b.DeliveryObservations(ctx)
	}
	return nil, nil
}

// Run reconciles immediately, then every PollInterval. It returns the first
// observation error so the host can log it and decide its own retry policy.
// Cancellation stops this loop, not running VM jobs.
func (r *Runtime) Run(ctx context.Context) error {
	for {
		if e := r.Reconcile(ctx); e != nil {
			return e
		}
		timer := time.NewTimer(r.period)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Drain disables new admissions and retires slot reservations as jobs finish.
// Published images remain available for reuse or explicit PurgeImages.
// Call Reconcile until Slots and DockerAllocations are empty. It does not cancel active work.
func (r *Runtime) Drain(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	for i := range r.p.Config.Slots {
		r.p.Config.Slots[i].Count = 0
	}
	return r.p.Reconcile(ctx)
}
func (r *Runtime) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if r.logs != nil {
		r.logs.Close()
		r.archive.Close()
	}
	if b, ok := r.p.Backend.(interface{ Close() error }); ok {
		_ = b.Close()
	}
	return r.p.Close()
}

// PurgeImages removes this owner's inactive prepared sources after Drain.
// Normal reconciliation, Close, and Drain leave published images available.
func (r *Runtime) PurgeImages(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	return r.p.PurgeImages(ctx)
}

// DockerAllocations reports Linux workers and requested macOS Docker pairs,
// including their preparation, execution and reclamation phases.
func (r *Runtime) DockerAllocations() ([]DockerStatus, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrClosed
	}
	return r.p.DockerAllocations()
}

type DockerStatus = engine.DockerStatus

// DefaultDockerEngine is the digest-pinned Ubuntu ARM64 profile qualified by the
// prototype. Enabling it and assigning a slot group remain explicit decisions.
func DefaultDockerEngine() DockerEngineProfile {
	return DockerEngineProfile{Name: "standard", Image: "ghcr.io/cirruslabs/ubuntu@sha256:439ba447488fa19f9b1ed091602546b8cb32442abdb60bac4943a4d6c3dd2ffa", SSHUsername: "admin", SSHPassword: "admin", CPU: 4, MemoryMiB: 8192, DiskGiB: 32, EngineVersion: "29.1.3"}
}
func DefaultLinuxBase() Base {
	p := DefaultDockerEngine()
	return Base{Name: "ubuntu-24.04-arm64", OS: "linux", OSVersion: "24.04", Image: p.Image, SSHUsername: p.SSHUsername, SSHPassword: p.SSHPassword, Capabilities: []string{"nix", "git", "node", "pnpm", "uv", "python"}}
}
