// Package bridge forwards the companion VM's authenticated SSH endpoint through
// a worker port. launchd owns this process independently of the controller.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Config struct {
	Target string
	Ready  string
	Port   int
}

func Run(ctx context.Context, c Config) error {
	if _, _, err := net.SplitHostPort(c.Target); err != nil {
		return fmt.Errorf("bridge target: %w", err)
	}
	if c.Port < 1 || c.Port > 65535 || !filepath.IsAbs(c.Ready) {
		return errors.New("bridge requires a fixed TCP port and absolute ready path")
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", fmt.Sprintf("0.0.0.0:%d", c.Port))
	if err != nil {
		return err
	}
	return serve(ctx, listener, c)
}

func serve(ctx context.Context, listener net.Listener, c Config) error {
	defer listener.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	ready := struct {
		Kind       string `json:"helper_kind"`
		Port       int    `json:"port"`
		PID        int    `json:"pid"`
		Executable string `json:"executable"`
		Target     string `json:"target"`
		Ready      string `json:"ready"`
	}{"pulse-vz-worker-endpoint", listener.Addr().(*net.TCPAddr).Port, os.Getpid(), executable, c.Target, c.Ready}
	data, err := json.Marshal(ready)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(c.Ready), ".ready-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), c.Ready); err != nil {
		return err
	}
	var workers sync.WaitGroup
	defer workers.Wait()
	// Cancel active streams before waiting for their goroutines on every exit.
	defer cancel()
	for {
		client, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := forward(ctx, client, c.Target); err != nil && ctx.Err() == nil {
				log.Printf("bridge forwarding failed: %v", err)
			}
		}()
	}
}

func forward(ctx context.Context, client net.Conn, target string) error {
	defer client.Close()
	stopClient := context.AfterFunc(ctx, func() { client.Close() })
	defer stopClient()
	remote, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", target)
	if err != nil {
		return fmt.Errorf("connect to engine: %w", err)
	}
	defer remote.Close()
	stopRemote := context.AfterFunc(ctx, func() { remote.Close() })
	defer stopRemote()
	pump := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if half, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = half.CloseWrite()
		}
	}
	done := make(chan struct{})
	go func() { defer close(done); pump(remote, client) }()
	pump(client, remote)
	<-done
	return nil
}
