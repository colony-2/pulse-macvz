package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/colony-2/pulse-macvz/internal/backend"
	"github.com/colony-2/pulse-macvz/internal/config"
	"github.com/colony-2/pulse-macvz/internal/vz"
)

// This helper operates only on a disposable private home supplied by the
// verification driver. It uses normal provider adoption/creation, not raw start.
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "network preflight VM:", err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) != 4 {
		return errors.New("usage: start|stop HOME RUNNER")
	}
	if (os.Args[1] != "start" && os.Args[1] != "stop") || !filepath.IsAbs(os.Args[2]) || !filepath.IsAbs(os.Args[3]) {
		return errors.New("requires start|stop and absolute home/runner paths")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := &vz.Store{Home: os.Args[2], Runner: os.Args[3], Timeout: 20 * time.Second, PullTimeout: time.Minute}
	if os.Args[1] == "stop" {
		v, err := s.Inspect(ctx, "network-check")
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if v.Running {
			if err := s.Stop(ctx, "network-check"); err != nil {
				return err
			}
		}
		for {
			v, err = s.Inspect(ctx, "network-check")
			if err != nil {
				return err
			}
			if !v.Running {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
		if err := s.Delete(ctx, "network-check"); err != nil {
			return err
		}
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(os.Args[2], "vms", "seed", "config.json"))
	if err != nil {
		return err
	}
	var resources struct {
		CPU    int64 `json:"cpuCount"`
		Memory int64 `json:"memorySize"`
	}
	if err := json.Unmarshal(raw, &resources); err != nil {
		return err
	}
	b, err := vz.New(config.VZ{Home: os.Args[2], Runner: os.Args[3], SSHTimeout: 5 * time.Second}, config.Config{OwnerID: "network-check", StateDir: filepath.Join(os.Args[2], "state"), BootstrapTimeout: time.Minute})
	if err != nil {
		return err
	}
	defer b.Close()
	spec := backend.Spec{Name: "seed", OS: "linux", Arch: "arm64", CPU: resources.CPU, Memory: resources.Memory / (1 << 20), Username: "admin", Password: "admin"}
	if _, err := b.AdoptSource(ctx, spec, nil); err != nil {
		return err
	}
	spec.Name = "network-check"
	spec.Image = "seed"
	spec.CPU = 2
	spec.Memory = 2048
	if _, err := b.Create(ctx, spec); err != nil {
		return err
	}
	for ctx.Err() == nil {
		observed, err := b.Get(ctx, "network-check")
		if err != nil {
			return err
		}
		if observed.Status == "failed" {
			return errors.New("runner failed; inspect network-home/vms/network-check/pulse.run.log")
		}
		ip, err := s.IP(ctx, "network-check")
		if err == nil {
			c, e := net.DialTimeout("tcp", net.JoinHostPort(ip, "22"), time.Second)
			if e == nil {
				c.SetReadDeadline(time.Now().Add(2 * time.Second))
				banner, readErr := bufio.NewReader(io.LimitReader(c, 256)).ReadString('\n')
				c.Close()
				if readErr != nil || !strings.HasPrefix(banner, "SSH-2.0-") {
					time.Sleep(time.Second)
					continue
				}
				if e = os.WriteFile(filepath.Join(os.Args[2], "target"), []byte(net.JoinHostPort(ip, "22")), 0600); e != nil {
					return e
				}
				fmt.Println(ip + ":22")
				return nil
			}
		}
		time.Sleep(time.Second)
	}
	return ctx.Err()
}
