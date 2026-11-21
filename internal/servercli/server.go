package servercli

import (
	"context"
	"errors"
	"fmt"
	"sync"

	macvz "github.com/colony-2/pulse-macvz"
	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-provider-api/plugin"
)

type runtimeProvider interface {
	api.Provider
	Check(context.Context) error
	Run(context.Context) error
	Close() error
}

type runtimeOpener func(serverOptions) (runtimeProvider, error)

// A fresh runtime is constructed for each SDK session, after authentication.
// Closing it releases controller ownership but never kills accepted native jobs.
type providerFactory struct {
	constructing bool
	options      serverOptions
	open         runtimeOpener
	fail         func(error)
	mu           sync.Mutex
	closing      bool
	active       *providerSession
}

type providerSession struct {
	runtime         runtimeProvider
	cancel          context.CancelFunc
	maintenanceDone chan struct{}
	closeOnce       sync.Once
	closeDone       chan struct{}
	closeErr        error
}

func (s *providerSession) close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.cancel()
		go func() {
			<-s.maintenanceDone
			s.closeErr = s.runtime.Close()
			close(s.closeDone)
		}()
	})
	select {
	case <-s.closeDone:
		return s.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *providerFactory) construct(ctx context.Context, in plugin.Init) (api.Provider, plugin.Cleanup, error) {
	if len(in.ExecutorEnv) != 0 {
		return nil, nil, fmt.Errorf("executor environment overrides are unsupported; configure the supplied-lease guest through the provider")
	}
	if len(in.ConfigYAML) > 0 {
		var empty struct{}
		if err := plugin.DecodeConfig(in.ConfigYAML, &empty); err != nil {
			return nil, nil, fmt.Errorf("provider configuration is supplied through server flags; SDK ConfigYAML must be empty or {}")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	f.mu.Lock()
	if f.closing || f.constructing {
		f.mu.Unlock()
		return nil, nil, context.Canceled
	}
	if f.active != nil {
		select {
		case <-f.active.closeDone:
			if f.active.closeErr != nil {
				f.mu.Unlock()
				return nil, nil, plugin.ErrCleanup
			}
		default:
			f.mu.Unlock()
			return nil, nil, fmt.Errorf("previous provider session is still active")
		}
	}
	f.constructing = true
	f.mu.Unlock()
	r, err := f.open(f.options)
	f.mu.Lock()
	f.constructing = false
	if err != nil {
		f.mu.Unlock()
		return nil, nil, err
	}
	lifetime, cancel := context.WithCancel(in.Lifetime)
	s := &providerSession{runtime: r, cancel: cancel, maintenanceDone: make(chan struct{}), closeDone: make(chan struct{})}
	f.active = s
	closing := f.closing
	f.mu.Unlock()
	if closing {
		cancel()
		close(s.maintenanceDone)
		return nil, s.close, context.Canceled
	}
	checkCtx, checkCancel := context.WithCancel(ctx)
	stopCheck := context.AfterFunc(lifetime, checkCancel)
	err = r.Check(checkCtx)
	stopCheck()
	checkCancel()
	if err != nil {
		close(s.maintenanceDone)
		return nil, s.close, fmt.Errorf("provider check: %w", err)
	}
	go func() {
		defer close(s.maintenanceDone)
		err := r.Run(lifetime)
		if lifetime.Err() == nil {
			if err == nil {
				err = fmt.Errorf("provider maintenance stopped unexpectedly")
			}
			f.fail(fmt.Errorf("provider reconciliation stopped: %w", err))
		}
	}()
	return r, s.close, nil
}

func (f *providerFactory) close(ctx context.Context) error {
	f.mu.Lock()
	f.closing = true
	active := f.active
	f.mu.Unlock()
	if active != nil {
		return active.close(ctx)
	}
	return nil
}

func runServer(ctx context.Context, o serverOptions) error {
	return runPlugin(ctx, o, openRuntime)
}

func runPlugin(ctx context.Context, o serverOptions, open runtimeOpener) error {
	if err := o.transport.validate(o.mode, o.owner); err != nil {
		return err
	}
	opts := o.transport.serveOptions(o.version)
	var outbound plugin.RunOptions
	if o.mode == "outbound" {
		clientTLS, err := o.transport.tlsConfig()
		if err != nil {
			return err
		}
		outbound = plugin.RunOptions{Address: o.transport.address, TLSConfig: clientTLS,
			Init: plugin.Initialization{Name: o.transport.name, BaseDir: o.stateDir}, Serve: opts,
			StartupTimeout: o.transport.startup, Reconnect: o.transport.reconnect}
	}
	lifetime, cancel := context.WithCancel(ctx)
	defer cancel()
	failures := make(chan error, 1)
	factory := &providerFactory{options: o, open: open, fail: func(err error) {
		select {
		case failures <- err:
		default:
		}
		cancel()
	}}
	if o.log != nil {
		fmt.Fprintf(o.log, "Starting %s provider server: backend=%s macos=%d linux=%d macos-docker=%d total=%d\n", o.mode, o.backend, o.capacity.macOS, o.capacity.linux, o.capacity.macDocker, *o.config.VMCounts.Total)
	}
	construct := func(ctx context.Context, input plugin.Init) (api.Provider, plugin.Cleanup, error) {
		p, cleanup, err := factory.construct(ctx, input)
		if err != nil && o.log != nil {
			fmt.Fprintf(o.log, "Provider initialization failed: %v\n", err)
		}
		return p, cleanup, err
	}
	var result error
	if o.mode == "outbound" {
		// Run bounds transport cleanup and fully disposes a session before reconnect.
		result = plugin.Run(lifetime, construct, outbound)
	} else {
		// Serve is the SDK's dedicated child-process entry point, not a TCP listener.
		// It has no context argument. On a CLI signal, close our runtime before main
		// exits; the process boundary disposes the remaining SDK transport goroutine.
		done := make(chan error, 1)
		go func() { done <- plugin.Serve(construct, opts) }()
		select {
		case result = <-done:
		case <-lifetime.Done():
			result = lifetime.Err()
		}
	}
	cancel()
	clean, stop := context.WithTimeout(context.Background(), o.transport.shutdown)
	defer stop()
	cleanupErr := factory.close(clean)
	select {
	case err := <-failures:
		result = err
	default:
	}
	if errors.Is(result, context.Canceled) && !errors.Is(result, plugin.ErrCleanup) {
		result = nil
	}
	return errors.Join(result, cleanupErr)
}

func openRuntime(o serverOptions) (runtimeProvider, error) {
	return macvz.NewVZ(o.config, macvz.VZConfig{Home: o.vmHome, Runner: o.runner, DockerEndpoint: o.endpoint})
}
