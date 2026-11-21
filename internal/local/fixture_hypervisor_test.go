package local

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/colony-2/pulse-macvz/internal/config"
	"golang.org/x/sys/unix"
)

func open(c config.Local, common config.Config) (*Client, error) {
	h := &fixtureHypervisor{}
	b, e := openNative(c, common, "vz", h)
	if e != nil {
		return nil, e
	}
	h.b = b
	h.command = b.execute
	h.launch = b.start
	b.endpointArgs = func(target, ready string, port int) []string {
		return []string{"/test/runner", "__pulse-vm", "bridge", "--target", target, "--ready", ready, "--port", strconv.Itoa(port)}
	}
	return b, nil
}

// fixtureHypervisor translates the synthetic inventory used by lifecycle tests.
type fixtureHypervisor struct {
	b       *Client
	command command
	launch  func(string) (int, error)
	binary  string
}

func (t *fixtureHypervisor) Info(ctx context.Context) error {
	_, err := t.command(ctx, "--version")
	return err
}
func (t *fixtureHypervisor) List(ctx context.Context) ([]Hardware, error) {
	data, err := t.command(ctx, "list", "--source", "local", "--format", "json")
	if err != nil {
		return nil, err
	}
	var result []Hardware
	if err = json.Unmarshal(data, &result); err != nil {
		return nil, failure("decode inventory", err)
	}
	if result == nil {
		return nil, errors.New("inventory is null")
	}
	return result, nil
}
func (t *fixtureHypervisor) Inspect(ctx context.Context, name string) (Hardware, error) {
	var h Hardware
	data, err := t.command(ctx, "get", name, "--format", "json")
	if err != nil {
		return h, err
	}
	if err = json.Unmarshal(data, &h); err != nil {
		return h, failure("decode inspect", err)
	}
	return h, nil
}
func (t *fixtureHypervisor) Clone(ctx context.Context, src, dst string) error {
	_, err := t.command(ctx, "clone", src, dst)
	return err
}
func (t *fixtureHypervisor) Configure(ctx context.Context, name string, c MachineOptions) error {
	args := []string{"set", name, "--cpu", strconv.FormatInt(c.CPU, 10), "--memory", strconv.FormatInt(c.Memory, 10)}
	if c.RandomMAC {
		args = append(args, "--random-mac")
	}
	if c.RandomSerial {
		args = append(args, "--random-serial")
	}
	_, err := t.command(ctx, args...)
	return err
}
func (t *fixtureHypervisor) Start(name string) (int, error) { return t.launch(name) }
func (t *fixtureHypervisor) Suspend(ctx context.Context, name string) error {
	_, err := t.command(ctx, "suspend", name)
	return err
}
func (t *fixtureHypervisor) Stop(ctx context.Context, name string) error {
	_, err := t.command(ctx, "stop", name)
	return err
}
func (t *fixtureHypervisor) Delete(ctx context.Context, name string) error {
	_, err := t.command(ctx, "delete", name)
	return err
}
func (t *fixtureHypervisor) IP(ctx context.Context, name string) (string, error) {
	data, err := t.command(ctx, "ip", name, "--wait", "5")
	return string(data), err
}

func (b *Client) env() []string {
	out := []string{}
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "TART_HOME=") && !strings.HasPrefix(v, "TART_NO_AUTO_PRUNE=") {
			out = append(out, v)
		}
	}
	return append(out, "TART_HOME="+b.cfg.Home, "TART_NO_AUTO_PRUNE=true")
}

func (b *Client) execute(ctx context.Context, args ...string) ([]byte, error) {
	timeout := b.cfg.CommandTimeout
	if len(args) > 0 && args[0] == "clone" {
		timeout = b.timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.native.(*fixtureHypervisor).binary, args...)
	cmd.Env = b.env()
	var out boundedBuffer
	cmd.Stdout = &out
	var stderr stderrPrefix
	cmd.Stderr = &stderr
	if e := cmd.Run(); e != nil {
		op := "command"
		if len(args) > 0 {
			op = args[0]
		}
		return nil, failure("command "+op, errors.Join(e, ctx.Err(), stderr.cause()))
	}
	return out.Bytes(), nil
}

// The detached wrapper writes a durable exit observation even if the Go host
// exits. Neither argv nor inherited environment contains the launch payload.
func (b *Client) start(name string) (int, error) {
	lock, e := os.OpenFile(b.artifact(name, "run.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return 0, failure("open native run lock", e)
	}
	defer lock.Close()
	if e = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		return 0, failure("lock native run", e)
	}
	f, e := os.OpenFile(b.artifact(name, "run.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return 0, failure("start detached Tart run", e)
	}
	suspendable := true
	if r := b.records[name]; r != nil {
		suspendable = r.Spec.OS != "linux"
	}
	cmd := exec.Command("/bin/sh", "-c", `if [ "$4" = true ]; then "$1" run --no-graphics --suspendable "$2"; else "$1" run --no-graphics "$2"; fi
code=$?
umask 077
printf '%s\n' "$code" > "$3.tmp"
mv "$3.tmp" "$3"
`, "pulse-tart-run", b.native.(*fixtureHypervisor).binary, name, b.artifact(name, "exit"), strconv.FormatBool(suspendable))
	cmd.Env = b.env()
	cmd.ExtraFiles = []*os.File{lock}
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if e := cmd.Start(); e != nil {
		f.Close()
		return 0, failure("start detached Tart run", e)
	}
	f.Close()
	go func() { _ = cmd.Wait() }()
	return cmd.Process.Pid, nil
}
