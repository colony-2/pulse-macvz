package vz

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/colony-2/pulse-tart/internal/vz/bridge"
)

// RunnerMain must be called at the beginning of the host's main function. It
// returns normally except for the private runner invocation, which exits.
func RunnerMain() {
	if len(os.Args) < 2 || os.Args[1] != "__pulse-vm" {
		return
	}
	if err := runnerCommand(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "VZ runner:", err)
		os.Exit(1)
	}
	os.Exit(0)
}
func runnerCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("missing runner operation")
	}
	switch args[0] {
	case "probe":
		if err := checkNative(); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(runnerProtocol)
	case "identity":
		if err := checkNative(); err != nil {
			return err
		}
		id, err := newIdentity()
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(id)
	case "run":
		if len(args) != 2 || !filepath.IsAbs(args[1]) {
			return errors.New("run requires absolute VM directory")
		}
		return runVM(args[1])
	case "bridge":
		fs := flag.NewFlagSet("bridge", flag.ContinueOnError)
		target := fs.String("target", "", "guest SSH endpoint")
		ready := fs.String("ready", "", "readiness file")
		port := fs.Int("port", 0, "worker port")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("unexpected bridge arguments")
		}
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer cancel()
		return bridge.Run(ctx, bridge.Config{Target: *target, Ready: *ready, Port: *port})
	default:
		return errors.New("unknown runner operation")
	}
}
func validateSnapshot(dir string, c diskConfig) error {
	if !c.RestoreRequired || c.Snapshot == nil {
		return errors.New("snapshot metadata missing")
	}
	host, err := hostBuild()
	if err != nil {
		return err
	}
	if c.Snapshot.Host != host || c.Snapshot.Layout != deviceLayout || c.Snapshot.ConfigHash != configHash(c) {
		return errors.New("snapshot is incompatible with host, device layout, or VM identity")
	}
	info, err := os.Lstat(filepath.Join(dir, "state.vzvmsave"))
	if err != nil {
		return fmt.Errorf("required saved state unavailable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return errors.New("required saved state is not a nonempty regular file")
	}
	return nil
}

type runControl struct{ Socket, Token string }
type controlRequest struct{ Token, Operation string }

func controlPath(dir string) (string, error) {
	root := filepath.Join("/tmp", "pulse-vz-"+strconv.Itoa(os.Getuid()))
	if err := os.Mkdir(root, 0700); err != nil && !os.IsExist(err) {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0700 || st.Uid != uint32(os.Getuid()) {
		return "", errors.New("control directory must be private and owned by this user")
	}
	hash := sha256.Sum256([]byte(dir))
	return filepath.Join(root, hex.EncodeToString(hash[:16])+".sock"), nil
}
func runVM(dir string) error {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return ErrPlatform
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("VM path must be a directory")
	}
	lock, err := lockFile(context.Background(), filepath.Join(dir, "vz.lock"), false, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	if marker, err := readRegular(filepath.Join(dir, ".pulse-owner"), 128); err != nil || len(marker) != 32 {
		return errors.New("runner requires a Pulse-owned VM")
	}
	c, err := readConfig(dir)
	if err != nil {
		return err
	}
	if err = validateOwned(c); err != nil {
		return err
	}
	if c.RestoreRequired {
		if err = validateSnapshot(dir, c); err != nil {
			return err
		}
	}
	socket, err := controlPath(dir)
	if err != nil {
		return err
	}
	// The VM lock excludes an older runner for this exact directory.
	if err = os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(socket)
	if err = os.Chmod(socket, 0600); err != nil {
		return err
	}
	token := make([]byte, 32)
	if _, err = rand.Read(token); err != nil {
		return err
	}
	control := runControl{Socket: socket, Token: hex.EncodeToString(token)}
	if err = atomicJSON(filepath.Join(dir, "vz-control.json"), control); err != nil {
		return err
	}
	requests := make(chan string, 1)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			handleControl(conn, control.Token, c.OS, requests)
		}
	}()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-signals:
			select {
			case requests <- "stop":
			case <-done:
			}
		case <-done:
		}
	}()
	return runNative(dir, c, requests)
}

// Bound each request and authenticate before enqueuing any operation.
func handleControl(conn net.Conn, token, guestOS string, requests chan<- string) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	var req controlRequest
	if json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&req) != nil || subtle.ConstantTimeCompare([]byte(req.Token), []byte(token)) != 1 {
		return
	}
	if req.Operation != "stop" && req.Operation != "suspend" {
		return
	}
	if req.Operation == "suspend" && guestOS != "darwin" {
		_, _ = io.WriteString(conn, "unsupported\n")
		return
	}
	select {
	case requests <- req.Operation:
		_, _ = io.WriteString(conn, "accepted\n")
	default:
		_, _ = io.WriteString(conn, "busy\n")
	}
}

type runnerOutput struct{ bytes.Buffer }

func (b *runnerOutput) Write(data []byte) (int, error) {
	n := len(data)
	if remaining := 16384 - b.Len(); remaining > 0 {
		b.Buffer.Write(data[:min(remaining, n)])
	}
	return n, nil
}

func (s *Store) command(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.Runner, append([]string{"__pulse-vm"}, args...)...)
	var output runnerOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	out := output.Bytes()
	if err != nil {
		return nil, fmt.Errorf("VZ runner %s: %w: %s", args[0], errors.Join(err, ctx.Err()), out)
	}
	return out, nil
}
func (s *Store) Info(ctx context.Context) error {
	out, err := s.command(ctx, "probe")
	if err != nil {
		return err
	}
	var protocol string
	if json.Unmarshal(out, &protocol) != nil || protocol != runnerProtocol {
		return errors.New("runner protocol mismatch; call RunnerMain at the start of main")
	}
	return nil
}
func (s *Store) Start(name string) (int, error) {
	dir, err := s.path(name)
	if err != nil {
		return 0, err
	}
	nativeLock, err := lockFile(context.Background(), filepath.Join(dir, "vz.lock"), false, false)
	if err != nil {
		return 0, err
	}
	defer nativeLock.Close()
	lock, err := lockFile(context.Background(), filepath.Join(dir, "pulse.run.lock"), false, false)
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	c, err := readConfig(dir)
	if err != nil {
		return 0, err
	}
	if err = validateOwned(c); err != nil {
		return 0, err
	}
	if c.RestoreRequired {
		if err = validateSnapshot(dir, c); err != nil {
			return 0, err
		}
	}
	fence, err := os.OpenFile(filepath.Join(dir, "vz.started"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return 0, fmt.Errorf("VM start already dispatched or unavailable: %w", err)
	}
	err = fence.Sync()
	fence.Close()
	if err != nil {
		return 0, err
	}
	if err = syncDir(dir); err != nil {
		return 0, err
	}
	log, err := os.OpenFile(filepath.Join(dir, "pulse.run.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return 0, err
	}
	defer log.Close()
	cmd := exec.Command("/bin/sh", "-c", `"$1" __pulse-vm run "$2"
code=$?
umask 077
printf '%s\n' "$code" > "$3.tmp"
mv "$3.tmp" "$3"
exit "$code"
`, "pulse-vz-run", s.Runner, dir, filepath.Join(dir, "pulse.exit"))
	cmd.ExtraFiles = []*os.File{lock}
	cmd.Stdout = log
	cmd.Stderr = log
	// Native children inherit the terminal's local-network attribution.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// The wrapper lock now excludes clones/configuration through runner startup.
	if err = nativeLock.Close(); err != nil {
		return 0, err
	}
	if err = cmd.Start(); err != nil {
		return 0, err
	}
	go func() { _ = cmd.Wait() }()
	return cmd.Process.Pid, nil
}
func (s *Store) control(ctx context.Context, name, op string) error {
	dir, err := s.path(name)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	for {
		held, err := lockHeld(filepath.Join(dir, "pulse.run.lock"))
		if err != nil {
			return err
		}
		if !held {
			return nil
		}
		data, err := readRegular(filepath.Join(dir, "vz-control.json"), 4096)
		if err == nil {
			var control runControl
			if err = json.Unmarshal(data, &control); err != nil {
				return err
			}
			expected, e := controlPath(dir)
			if e != nil {
				return e
			}
			if control.Socket != expected || len(control.Token) != 64 {
				return errors.New("invalid runner control metadata")
			}
			conn, e := (&net.Dialer{}).DialContext(ctx, "unix", control.Socket)
			if e == nil {
				deadline, _ := ctx.Deadline()
				_ = conn.SetDeadline(deadline)
				err = json.NewEncoder(conn).Encode(controlRequest{Token: control.Token, Operation: op})
				if err == nil {
					answer, e := io.ReadAll(io.LimitReader(conn, 64))
					err = e
					if err == nil && strings.TrimSpace(string(answer)) != "accepted" {
						err = errors.New("runner did not accept control operation")
					}
				}
				conn.Close()
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
func (s *Store) Stop(ctx context.Context, name string) error { return s.control(ctx, name, "stop") }
func (s *Store) Suspend(ctx context.Context, name string) error {
	return s.control(ctx, name, "suspend")
}
