// Package local implements shared VM ownership, guest delivery and recovery.
package local

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/colony-2/pulse-tart/internal/backend"
	"github.com/colony-2/pulse-tart/internal/config"
	"github.com/colony-2/pulse-tart/internal/ctxlock"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

var Err = errors.New("local VM operation failed")
var ErrPlatform = errors.New("direct Tart requires macOS with native guest architecture")

type command func(context.Context, ...string) ([]byte, error)
type record struct {
	Spec             backend.Spec    `json:"spec"`
	UID              string          `json:"uid"`
	Phase            string          `json:"phase"`
	PID              int             `json:"pid"`
	Device           uint64          `json:"device"`
	VolumeUUID       string          `json:"volume_uuid,omitempty"`
	Inode            uint64          `json:"inode"`
	MAC              string          `json:"mac"`
	HostKey          []byte          `json:"host_key,omitempty"`
	Created          time.Time       `json:"created"`
	Delivered        bool            `json:"delivered"`
	Reusable         bool            `json:"reusable,omitempty"`          // Published source; replacement under this name is intentional.
	SuspendRequested bool            `json:"suspend_requested,omitempty"` // Tart suspend signals asynchronously; never signal a save twice.
	Delivery         *DeliveryStatus `json:"delivery,omitempty"`
}
type Hardware struct {
	OS         string
	CPU        int64
	Memory     int64
	MACAddress string
	State      string
	Running    bool
	Name       string
}
type hardware = Hardware

type Client struct {
	kind                string
	native              Hypervisor
	endpointArgs        func(target, ready string, port int) []string
	mu                  ctxlock.Mutex
	cfg                 config.Tart
	dir                 string
	scope               string
	legacyDir           string
	locks               []*os.File
	properties          map[string]json.RawMessage
	staged              map[string]json.RawMessage // Pre-create properties; never returned as inventory.
	bases               []config.Base
	timeout             time.Duration
	command             command
	volumeUUID          func(string) (string, error)
	service             command // launchctl; injectable for lifecycle tests
	launch              func(string) (int, error)
	records             map[string]*record
	ctx                 context.Context
	cancel              context.CancelFunc
	workers             map[string]bool
	wg                  sync.WaitGroup
	closed              bool
	lastDeliveryFailure error
	deliveryRetryAt     time.Time
}

func New(c config.Tart, common config.Config) (*Client, error) {
	if runtime.GOOS != "darwin" || (runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64") {
		return nil, ErrPlatform
	}
	for _, b := range common.Bases {
		if b.Architecture() != runtime.GOARCH {
			return nil, fmt.Errorf("%w: base %s requires %s, host is %s", ErrPlatform, b.Name, b.Architecture(), runtime.GOARCH)
		}
	}
	if c.Binary == "" {
		c.Binary = "tart"
	}
	binary, e := exec.LookPath(c.Binary)
	if e != nil {
		return nil, failure("resolve Tart executable/home", e)
	}
	c.Binary, e = filepath.Abs(binary)
	if e != nil {
		return nil, failure("resolve Tart executable/home", e)
	}
	if c.Home == "" {
		home, e := os.UserHomeDir()
		if e != nil {
			return nil, failure("resolve Tart executable/home", e)
		}
		c.Home = filepath.Join(home, ".tart")
	}
	if !filepath.IsAbs(c.Home) {
		return nil, config.Err
	}
	c.Home = filepath.Clean(c.Home)
	if c.CommandTimeout == 0 {
		c.CommandTimeout = 2 * time.Minute
	}
	if c.SSHTimeout == 0 {
		c.SSHTimeout = 15 * time.Second
	}
	if c.CommandTimeout < time.Second || c.SSHTimeout < time.Second {
		return nil, config.Err
	}
	if c.DeliveryTimeout < 0 || (c.DeliveryTimeout > 0 && c.DeliveryTimeout < time.Second) {
		return nil, config.Err
	}
	client, e := open(c, common)
	if e != nil {
		return nil, e
	}
	client.command = client.execute
	client.launch = client.start
	return client, nil
}
func open(c config.Tart, common config.Config) (*Client, error) {
	return openNative(c, common, "tart", nil)
}
func openNative(c config.Tart, common config.Config, kind string, native Hypervisor) (*Client, error) {
	ctx, cancel := context.WithCancel(context.Background())
	if common.Docker != nil {
		for _, p := range common.Docker.Profiles {
			common.Bases = append(append([]config.Base{}, common.Bases...), config.Base{Image: p.Image, SSHHostKey: p.SSHHostKey})
		}
	}
	scope := (config.Settings{Config: common, Kind: kind, Endpoint: c.Home}).Scope()
	b := &Client{kind: kind, native: native, service: service, cfg: c, dir: filepath.Join(c.Home, ".pulse", scope), scope: scope, legacyDir: common.StateDir, bases: common.Bases, timeout: common.BootstrapTimeout, records: map[string]*record{}, properties: map[string]json.RawMessage{}, workers: map[string]bool{}, ctx: ctx, cancel: cancel}
	b.command, b.volumeUUID = b.execute, nativeVolumeUUID
	if err := b.openProperties(); err != nil {
		b.Close()
		return nil, failure("open VM properties", err)
	}
	return b, nil
}

func (b *Client) Home() string { return b.cfg.Home }
func (b *Client) env() []string {
	out := []string{}
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "TART_HOME=") && !strings.HasPrefix(v, "TART_NO_AUTO_PRUNE=") {
			out = append(out, v)
		}
	}
	return append(out, "TART_HOME="+b.cfg.Home, "TART_NO_AUTO_PRUNE=true")
}

type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 4<<20 {
		return 0, Err
	}
	return b.Buffer.Write(p)
}
func (b *Client) execute(ctx context.Context, args ...string) ([]byte, error) {
	timeout := b.cfg.CommandTimeout
	if len(args) > 0 && args[0] == "clone" {
		timeout = b.timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.cfg.Binary, args...)
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

func (b *Client) path(name string) string { return filepath.Join(b.cfg.Home, "vms", name) }
func (b *Client) binding(r *record, claim bool) error {
	st, e := os.Lstat(b.path(r.Spec.Name))
	if e != nil {
		return failure("observe native directory ownership", e)
	}
	if !st.IsDir() {
		return failure("native ownership/state condition failed", nil)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return failure("native ownership/state condition failed", nil)
	}
	marker := filepath.Join(b.path(r.Spec.Name), ".pulse-owner")
	volume, e := b.volumeUUID(b.path(r.Spec.Name))
	if e != nil {
		return failure("observe native volume identity", e)
	}
	if claim {
		if e := os.WriteFile(marker, []byte(r.UID), 0600); e != nil {
			return failure("write ownership marker", e)
		}
		r.Device = uint64(sys.Dev)
		r.VolumeUUID = volume
		r.Inode = uint64(sys.Ino)
		return nil
	}
	got, e := os.ReadFile(marker)
	if e != nil {
		return failure("observe native directory ownership", e)
	}
	if string(got) != r.UID || r.Inode != uint64(sys.Ino) {
		return failure("native ownership marker or directory inode mismatch for "+r.Spec.Name, nil)
	}
	if r.VolumeUUID != "" {
		if volume != r.VolumeUUID {
			return failure("native volume UUID mismatch for "+r.Spec.Name, nil)
		}
	} else if r.Device != uint64(sys.Dev) {
		// Old records predate volume UUIDs. Permit a one-time upgrade only with
		// the same owner token, inode and fully observed hardware. A partial
		// creation has no MAC and cannot be safely migrated after renumbering.
		if volume == "" || r.MAC == "" {
			return failure("legacy native device mismatch for "+r.Spec.Name, nil)
		}
		h, err := b.inspect(b.ctx, r.Spec.Name)
		if err != nil {
			return failure("verify legacy native hardware for "+r.Spec.Name, err)
		}
		if h.CPU != r.Spec.CPU || h.Memory != r.Spec.Memory || h.MACAddress != r.MAC {
			return failure("legacy native hardware mismatch for "+r.Spec.Name, nil)
		}
	}
	// Keep device as a current observation for older diagnostic tooling. UUID
	// is authoritative once populated; never replace a mismatched saved UUID.
	r.Device, r.VolumeUUID = uint64(sys.Dev), volume
	return nil
}
func (b *Client) hypervisor() Hypervisor {
	if b.native != nil {
		return b.native
	}
	return tartHypervisor{b}
}
func (b *Client) inspect(ctx context.Context, name string) (Hardware, error) {
	h, err := b.hypervisor().Inspect(ctx, name)
	if err == nil && h.State == "" {
		err = failure("inspect state missing", nil)
	}
	return h, err
}
func (b *Client) Info(ctx context.Context) error { return b.hypervisor().Info(ctx) }
func (b *Client) runExited(r *record) bool {
	_, err := os.Stat(b.artifact(r.Spec.Name, "exit"))
	if err == nil {
		return true
	}
	// The detached shell inherits this flock. Unlike a saved PID it cannot be
	// confused with PID reuse, and it covers the gap before Tart acquires its
	// own running-VM lock. The file may remain; only the kernel lock matters.
	f, err := os.OpenFile(b.artifact(r.Spec.Name, "run.lock"), os.O_RDWR, 0)
	if err == nil {
		defer f.Close()
		if unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
			return false
		}
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		return true
	}
	if !os.IsNotExist(err) {
		return false
	}
	// Compatibility with already-running VMs whose old wrapper has no flock.
	return (r.PID > 0 && syscall.Kill(r.PID, 0) == syscall.ESRCH) || (r.Phase == "starting" && r.PID == 0)
}

func (b *Client) terminalSuspension(r *record) bool {
	if r.Reusable {
		return false
	}
	if r.Phase == "creating" || r.Phase == "prepared" {
		return true
	}
	if !b.runExited(r) {
		return false
	}
	// Builders deliberately exit successfully when producing a snapshot. Jobs
	// do not: an exited job clone must release its slot even if Tart retains
	// the snapshot file. Failed builder restores are terminal too.
	var props struct{ Slot, Image json.RawMessage }
	_ = json.Unmarshal(b.properties[r.Spec.Name], &props)
	if len(props.Slot) == 0 && len(props.Image) == 0 {
		return true
	}
	exit, err := os.ReadFile(b.artifact(r.Spec.Name, "exit"))
	return err == nil && strings.TrimSpace(string(exit)) != "0"
}
func (b *Client) get(ctx context.Context, name string) (backend.VM, error) {
	r := b.records[name]
	if r == nil {
		return backend.VM{}, &backend.Error{Code: 404}
	}
	if _, e := os.Lstat(b.path(name)); os.IsNotExist(e) {
		// Before run is dispatched, absence is uncertain, not deletion evidence.
		if r.Phase == "creating" {
			return backend.VM{}, failure("Get creating instance directory is absent; dispatch remains uncertain", e)
		}
		return backend.VM{}, &backend.Error{Code: 404}
	}
	if !r.Reusable {
		if e := b.binding(r, false); e != nil {
			return backend.VM{}, e
		}
	}
	h, e := b.inspect(ctx, name)
	if e != nil {
		return backend.VM{}, failure("Get native instance", e)
	}
	if r.Phase != "creating" && (h.CPU != r.Spec.CPU || h.Memory != r.Spec.Memory || (!r.Reusable && h.MACAddress != r.MAC)) {
		return backend.VM{}, failure("native identity, hardware or state mismatch", nil)
	}
	v := backend.VM{Spec: r.Spec, UID: r.UID, Worker: "local", LocalName: name, CreatedAt: r.Created, PowerState: h.State, Generation: 1, ObservedGeneration: 1, Status: "pending"}
	running := "false"
	switch h.State {
	case "running":
		v.Status = "running"
		running = "true"
	case "suspended":
		v.Status = "running"
		// A reusable template is intentionally suspended. An ordinary clone
		// whose run finished (including failed restore) is terminal.
		if b.terminalSuspension(r) {
			v.Status = "failed"
		}
	case "stopped":
		if r.Reusable || b.runExited(r) || r.Phase == "prepared" || r.Phase == "creating" {
			v.Status = "failed"
		} else {
			running = "true"
		}
	default:
		return backend.VM{}, failure("Get native instance: unrecognized power state", nil)
	}
	v.Conditions = []backend.Condition{{Type: "running", State: running}}
	if r.Delivery != nil {
		if r.Delivery.Cause != "" {
			v.StatusMessage = deliveryError(r).Error()
		}
		if r.Delivery.Expired && v.Stopped() {
			v.Status = "failed"
		}
	}
	return v, nil
}
func (b *Client) Get(ctx context.Context, name string) (backend.VM, error) {
	if e := b.mu.LockContext(ctx); e != nil {
		return backend.VM{}, e
	}
	defer b.mu.Unlock()
	return b.get(ctx, name)
}
func (b *Client) List(ctx context.Context) ([]backend.VM, error) {
	if e := b.mu.LockContext(ctx); e != nil {
		return nil, e
	}
	defer b.mu.Unlock()
	inventory, e := b.hypervisor().List(ctx)
	if e != nil {
		return nil, failure("List native inventory", e)
	}
	out := []backend.VM{}
	for _, h := range inventory {
		if b.records[h.Name] == nil {
			// Preserve observed power without adopting the VM. Inventory consumers
			// must distinguish stopped foreign templates from running instances.
			// Missing/unknown state remains conservatively active; Running wins
			// over a contradictory stopped/suspended state.
			running := "true"
			if !h.Running && (h.State == "stopped" || h.State == "suspended") {
				running = "false"
			}
			if running == "true" {
				// Tart's list output omits OS. Inspect active foreign VMs so Linux
				// workers do not consume the two physical macOS positions. This
				// observation never adopts the VM or permits lifecycle mutations.
				details, err := b.hypervisor().Inspect(ctx, h.Name)
				if err != nil {
					return nil, failure("inspect foreign VM OS", err)
				}
				h.OS = details.OS
			}
			out = append(out, backend.VM{Spec: backend.Spec{Name: h.Name, OS: h.OS}, UID: "foreign:" + h.Name,
				PowerState: h.State, Conditions: []backend.Condition{{Type: "running", State: running}}})
			continue
		}
		v, e := b.get(ctx, h.Name)
		if e != nil {
			return nil, failure("List native inventory", e)
		}
		out = append(out, v)
	}
	return out, nil
}
func (b *Client) Create(ctx context.Context, s backend.Spec) (backend.VM, error) {
	if e := b.mu.LockContext(ctx); e != nil {
		return backend.VM{}, e
	}
	defer b.mu.Unlock()
	if b.closed {
		return backend.VM{}, failure("create native clone: client closed", nil)
	}
	if e := ctx.Err(); e != nil {
		return backend.VM{}, failure("create native clone canceled", e)
	}
	if !config.Name(s.Name) {
		return backend.VM{}, failure("create native clone: invalid name", nil)
	}
	if b.records[s.Name] != nil {
		return backend.VM{}, failure("create native clone: name already reserved; no redispatch", nil)
	}
	if _, e := os.Lstat(b.path(s.Name)); !os.IsNotExist(e) {
		return backend.VM{}, failure("create destination must be absent", e)
	}
	var key []byte
	template := strings.Contains(s.Image, "@sha256:")
	cold := template
	if template {
		found := false
		for _, base := range b.bases {
			if base.Image == s.Image {
				key = base.SSHHostKey
				found = true
				break
			}
		}
		if !found {
			return backend.VM{}, failure("clone digest source is not configured", nil)
		}
	} else {
		parent := b.records[s.Image]
		if parent == nil {
			return backend.VM{}, failure("clone source has no native ownership record", nil)
		}
		v, e := b.get(ctx, s.Image)
		if e != nil {
			return backend.VM{}, failure("create native clone", e)
		}
		if v.PowerState != "suspended" && !(parent.Reusable && v.PowerState == "stopped") {
			return backend.VM{}, failure("native identity, hardware or state mismatch", nil)
		}
		cold = v.PowerState == "stopped"
		key = parent.HostKey
		sourceHardware, err := b.inspect(ctx, s.Image)
		if err != nil {
			return backend.VM{}, failure("create native clone", err)
		}
		inventory, e := b.hypervisor().List(ctx)
		if e != nil {
			return backend.VM{}, failure("create native clone", e)
		}
		for _, vm := range inventory {
			if vm.State != "running" && !vm.Running {
				continue
			}
			h, e := b.inspect(ctx, vm.Name)
			// The published source can be replaced between observation and clone.
			// Collision checks against the actual cloned MAC happen below.
			if e != nil {
				return backend.VM{}, failure("create native clone", e)
			}
			if !cold && h.MACAddress == sourceHardware.MACAddress {
				return backend.VM{}, failure("native identity, hardware or state mismatch", nil)
			}
		}
	}
	var uid [16]byte
	if _, e := rand.Read(uid[:]); e != nil {
		return backend.VM{}, failure("create native clone", e)
	}
	r := &record{Spec: s, UID: hex.EncodeToString(uid[:]), Phase: "creating", Created: time.Now().UTC(), HostKey: key}
	b.properties[s.Name] = bytes.Clone(b.staged[s.Name])
	delete(b.staged, s.Name)
	b.records[s.Name] = r
	if e := b.save(r); e != nil {
		return backend.VM{}, e
	}
	if e := b.hypervisor().Clone(ctx, s.Image, s.Name); e != nil {
		// A command may fail after creating its destination. Attach the known
		// creation properties while this process still knows the operation, so
		// restart can clean the partial VM without retrying clone or run.
		if _, statErr := os.Lstat(b.path(s.Name)); statErr == nil {
			if bindErr := b.binding(r, true); bindErr != nil {
				return backend.VM{}, errors.Join(e, bindErr)
			}
			if saveErr := b.save(r); saveErr != nil {
				return backend.VM{}, errors.Join(e, saveErr)
			}
		} else if os.IsNotExist(statErr) {
			delete(b.records, s.Name)
		}
		return backend.VM{}, failure("create native clone", e)
	}
	if e := b.binding(r, true); e != nil {
		return backend.VM{}, e
	}
	if e := b.save(r); e != nil {
		return backend.VM{}, e
	}
	if cold {
		if e := b.hypervisor().Configure(ctx, s.Name, MachineOptions{CPU: s.CPU, Memory: s.Memory, RandomMAC: true, RandomSerial: s.OS == "darwin"}); e != nil {
			return backend.VM{}, failure("create native clone", e)
		}
	}
	h, e := b.inspect(ctx, s.Name)
	if e != nil {
		return backend.VM{}, failure("create native clone", e)
	}
	if h.CPU != s.CPU || h.Memory != s.Memory || h.MACAddress == "" {
		return backend.VM{}, failure("native identity, hardware or state mismatch", nil)
	}
	if !cold && h.State != "suspended" {
		return backend.VM{}, failure("restored clone is not suspended", nil)
	}
	inventory, e := b.hypervisor().List(ctx)
	if e != nil {
		return backend.VM{}, failure("create native clone", e)
	}
	for _, vm := range inventory {
		if vm.Name == s.Name || (vm.State != "running" && !vm.Running) {
			continue
		}
		other, err := b.inspect(ctx, vm.Name)
		if err != nil {
			return backend.VM{}, failure("inspect native hardware", err)
		}
		if other.MACAddress == h.MACAddress {
			return backend.VM{}, failure("native MAC collision", nil)
		}
	}
	r.MAC = h.MACAddress
	if e := b.binding(r, true); e != nil {
		return backend.VM{}, e
	}
	r.Phase = "prepared"
	if e := b.save(r); e != nil {
		return backend.VM{}, e
	}
	r.Phase = "starting"
	if e := b.save(r); e != nil {
		return backend.VM{}, e
	}
	pid, e := b.hypervisor().Start(s.Name)
	if e != nil {
		return backend.VM{}, failure("create native clone", e)
	}
	r.PID = pid
	r.Phase = "started"
	r.Delivery = &DeliveryStatus{StartedAt: time.Now().UTC(), Stage: "waiting_for_vm"}
	if e := b.save(r); e != nil {
		return backend.VM{}, e
	}
	b.deliver(r)
	return backend.VM{Spec: s, UID: r.UID, Worker: "local", LocalName: s.Name, Status: "pending", CreatedAt: r.Created}, nil
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
`, "pulse-tart-run", b.cfg.Binary, name, b.artifact(name, "exit"), strconv.FormatBool(suspendable))
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
func (b *Client) Suspend(ctx context.Context, name, uid string) error {
	if e := b.mu.LockContext(ctx); e != nil {
		return e
	}
	defer b.mu.Unlock()
	v, e := b.get(ctx, name)
	if e != nil {
		return failure("suspend native instance", e)
	}
	if v.UID != uid {
		return failure("native identity, hardware or state mismatch", nil)
	}
	if v.PowerState == "suspended" {
		return nil
	}
	if v.PowerState != "running" {
		return failure("native ownership/state condition failed", nil)
	}
	r := b.records[name]
	if r.SuspendRequested {
		return nil // Poll native state; do not replay an in-progress save.
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Keep the request with the VM before dispatch. After interruption the
	// outcome may be uncertain, so a save request must not be replayed.
	r.SuspendRequested = true
	if err := b.save(r); err != nil {
		return failure("save suspend request", err)
	}
	e = b.hypervisor().Suspend(ctx, name)
	return e
}
func (b *Client) Delete(ctx context.Context, name string) error {
	if e := b.mu.LockContext(ctx); e != nil {
		return e
	}
	defer b.mu.Unlock()
	v, e := b.get(ctx, name)
	if e != nil {
		return e
	}
	if !v.Stopped() {
		return failure("delete requires observed stopped VM", nil)
	}
	if b.workers[name] {
		return failure("delete blocked by native delivery worker", nil)
	}
	if e = b.hypervisor().Delete(ctx, name); e != nil {
		return e
	}
	if _, e = os.Lstat(b.path(name)); !os.IsNotExist(e) {
		return failure("delete destination still exists or cannot be observed", e)
	}
	delete(b.records, name)
	delete(b.properties, name)
	return nil
}
func (b *Client) Events(ctx context.Context, name string) ([]backend.Event, error) {
	if e := b.mu.LockContext(ctx); e != nil {
		return nil, e
	}
	defer b.mu.Unlock()
	if e := ctx.Err(); e != nil {
		return nil, failure("read native events canceled", e)
	}
	if b.records[name] == nil {
		return nil, failure("native events require an owned record", nil)
	}
	f, e := os.Open(b.artifact(name, "events"))
	if os.IsNotExist(e) {
		return []backend.Event{}, nil
	}
	if e != nil {
		return nil, failure("read native preparation events", e)
	}
	defer f.Close()
	// Startup output is fixed markers; bound even unexpected SSH output.
	st, e := f.Stat()
	if e != nil {
		return nil, failure("read native preparation events", e)
	}
	if st.Size() > 1<<20 {
		_, _ = f.Seek(-(1 << 20), io.SeekEnd)
	}
	data, e := io.ReadAll(io.LimitReader(f, 1<<20))
	if e != nil {
		return nil, failure("read native preparation events", e)
	}
	return []backend.Event{{Payload: string(data)}}, nil
}

// Recover resumes pre-exec delivery and stops expired, undispatched VMs. It
// never repeats an exec with an uncertain outcome. List/Get stay observational.
func (b *Client) Recover(ctx context.Context) error {
	if e := b.mu.LockContext(ctx); e != nil {
		return e
	}
	defer b.mu.Unlock()
	if b.closed {
		return failure("recover native delivery: client closed", nil)
	}
	if err := b.readProperties(); err != nil {
		return err
	}
	for name, r := range b.records {
		if r.Delivery != nil && r.Delivery.Expired && !r.Delivered && !r.Delivery.ExecutionPossible && !b.workers[name] {
			v, err := b.get(ctx, name)
			if backend.IsStatus(err, 404) {
				continue
			}
			if err != nil {
				return err
			}
			if v.PowerState == "running" {
				if err := b.hypervisor().Stop(ctx, name); err != nil {
					return failure("stop undelivered instance", err)
				}
			}
			continue // Normal reconciliation requires observed termination before deletion.
		}
		if (r.Phase != "started" && r.Phase != "starting") || r.Delivered || b.workers[name] {
			continue
		}
		v, e := b.get(ctx, name)
		if e != nil {
			if backend.IsStatus(e, 404) {
				continue
			}
			return e
		}
		if v.PowerState == "running" || v.Status == "pending" || (v.PowerState == "suspended" && !b.runExited(r)) {
			if r.Phase == "starting" {
				r.Phase = "started"
				if err := b.save(r); err != nil {
					return err
				}
			}
			b.deliver(r)
		}
	}
	return nil
}
func (b *Client) deliver(r *record) {
	if b.workers[r.Spec.Name] || b.closed || r.Delivered {
		return
	}
	b.workers[r.Spec.Name] = true
	if r.Delivery == nil {
		// Old records have no durable pre-exec fence. Do not infer that absence
		// of local events proves that a guest command never ran.
		r.Delivery = &DeliveryStatus{StartedAt: time.Now().UTC(), ExecutionPossible: true, Stage: "uncertain", Cause: "legacy_delivery_outcome_unknown"}
		_ = b.save(r)
	}
	b.wg.Add(1)
	// Copy immutable data before starting; journal/worker flags are under mu.
	spec := r.Spec
	key := bytes.Clone(r.HostKey)
	name := spec.Name
	go func() {
		defer b.wg.Done()
		defer func() { b.mu.Lock(); delete(b.workers, name); b.mu.Unlock() }()
		ctx, cancel := context.WithTimeout(b.ctx, b.timeout)
		defer cancel()
		for ctx.Err() == nil {
			b.mu.Lock()
			if r.Delivered {
				b.mu.Unlock()
				return
			}
			if b.deliveryMarker(name) {
				b.deliverySucceeded(r)
				b.mu.Unlock()
				return
			}
			if r.Delivery.ExecutionPossible {
				r.Delivery.Stage, r.Delivery.Cause = "uncertain", "guest_execution_may_have_started"
				_ = b.save(r)
				b.mu.Unlock()
				return
			}
			if time.Since(r.Delivery.StartedAt) >= b.deliveryBudget() {
				r.Delivery.Expired = true
				r.Delivery.Stage = "expired_before_exec"
				if r.Delivery.Cause == "" {
					r.Delivery.Cause = "delivery_deadline_exceeded"
				}
				_ = b.save(r)
				b.mu.Unlock()
				return
			}
			r.Delivery.Attempts++
			r.Delivery.LastAttemptAt = time.Now().UTC()
			r.Delivery.Stage = "observe_vm"
			setup, cancelSetup := context.WithDeadline(ctx, r.Delivery.StartedAt.Add(b.deliveryBudget()))
			v, err := b.get(setup, name)
			// A fresh clone retains its snapshot's suspended state until the
			// detached Tart process resumes it. Only a finished run proves that
			// suspension is terminal and delivery should stop.
			finished := err == nil && ((v.Status == "failed" && v.Stopped()) || (v.PowerState == "suspended" && b.runExited(r)))
			b.mu.Unlock()
			if backend.IsStatus(err, 404) || finished {
				cancelSetup()
				return
			}
			if err == nil && v.PowerState == "running" {
				b.deliveryStage(r, "discover_ip")
				data, e := b.hypervisor().IP(setup, name)
				ip := strings.TrimSpace(string(data))
				if e == nil && net.ParseIP(ip) != nil {
					e = b.sendAddressHooks(ctx, setup, net.JoinHostPort(ip, "22"), spec, key,
						func(stage string) error { return b.deliveryStage(r, stage) },
						func() { b.mu.Lock(); b.deliverySucceeded(r); b.mu.Unlock() })
					if e == nil || b.deliveryMarker(name) {
						b.mu.Lock()
						b.deliverySucceeded(r)
						b.mu.Unlock()
						cancelSetup()
						return
					}
				} else if e == nil {
					e = errors.New("invalid IP observation")
				}
				if e != nil {
					b.deliveryFailed(r, e)
				}
			} else if err != nil {
				b.deliveryFailed(r, err)
			}
			cancelSetup()
			b.mu.Lock()
			delay := time.Second << min(r.Delivery.Attempts-1, 4)
			remaining := b.deliveryBudget() - time.Since(r.Delivery.StartedAt)
			if !r.Delivery.ExecutionPossible && delay > remaining {
				delay = max(remaining, time.Millisecond)
			}
			b.mu.Unlock()
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}
func (b *Client) send(ctx context.Context, ip string, spec backend.Spec, key []byte) error {
	return b.sendAddress(ctx, net.JoinHostPort(ip, "22"), spec, key)
}
func (b *Client) sendAddress(ctx context.Context, address string, spec backend.Spec, key []byte) error {
	return b.sendAddressHooks(ctx, ctx, address, spec, key, func(string) error { return nil }, func() {})
}
func (b *Client) sendAddressHooks(ctx, setup context.Context, address string, spec backend.Spec, key []byte, stage func(string) error, delivered func()) error {
	callback := ssh.InsecureIgnoreHostKey()
	if len(key) > 0 {
		pub, _, _, _, e := ssh.ParseAuthorizedKey(key)
		if e != nil {
			return failure("delivery SSH", e)
		}
		callback = ssh.FixedHostKey(pub)
	}
	cfg := &ssh.ClientConfig{User: spec.Username, Auth: []ssh.AuthMethod{ssh.Password(spec.Password)}, HostKeyCallback: callback, Timeout: b.cfg.SSHTimeout}
	if e := stage("tcp"); e != nil {
		return e
	}
	conn, e := (&net.Dialer{Timeout: b.cfg.SSHTimeout}).DialContext(setup, "tcp", address)
	if e != nil {
		return failure("delivery SSH", e)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	stopSetup := context.AfterFunc(setup, func() { conn.Close() })
	defer stopSetup()
	_ = conn.SetDeadline(time.Now().Add(b.cfg.SSHTimeout))
	if e := stage("ssh_handshake"); e != nil {
		return e
	}
	cc, ch, req, e := ssh.NewClientConn(conn, address, cfg)
	if e != nil {
		return failure("delivery SSH", e)
	}
	client := ssh.NewClient(cc, ch, req)
	defer client.Close()
	if e := stage("ssh_session"); e != nil {
		return e
	}
	session, e := client.NewSession()
	if e != nil {
		return failure("delivery SSH", e)
	}
	defer session.Close()
	f, e := os.OpenFile(b.artifact(spec.Name, "events"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if e != nil {
		return failure("delivery SSH", e)
	}
	defer f.Close()
	// Disarm only the pre-exec timer. Runtime cancellation still closes SSH,
	// while an executing job can outlive the delivery deadline.
	if !stopSetup() || setup.Err() != nil {
		return setup.Err()
	}
	if e := stage("guest_exec"); e != nil {
		return e
	}
	_ = conn.SetDeadline(time.Time{}) // Only executing guest work may outlive the SSH setup deadline.
	session.Stdin = strings.NewReader(spec.StartupScript.Content)
	writer := &deliveryWriter{destination: f, delivered: delivered}
	session.Stdout = writer
	session.Stderr = writer
	if e := session.Run("/bin/bash -s"); e != nil {
		return failure("delivery SSH", e)
	}
	return nil
}
func (b *Client) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	b.cancel()
	b.mu.Unlock()
	b.wg.Wait()
	for _, f := range b.locks {
		_ = f.Close()
	}
	b.locks = nil
	return nil
}

// String intentionally excludes SSH credentials and private delivery records.
func (b *Client) String() string {
	if b.kind == "vz" {
		return fmt.Sprintf("VZ(%s)", b.cfg.Home)
	}
	return fmt.Sprintf("Tart(%s)", b.cfg.Home)
}

// stderrPrefix drains the command while retaining only the per-command prefix.
type stderrPrefix struct {
	data      []byte
	truncated bool
}

func (b *stderrPrefix) Write(p []byte) (int, error) {
	n := len(p)
	remaining := 16384 - len(b.data)
	if len(p) > remaining {
		b.truncated = true
		p = p[:remaining]
	}
	b.data = append(b.data, p...)
	return n, nil
}
func (b *stderrPrefix) cause() error {
	text := string(b.data)
	if b.truncated {
		text += "[truncated after 16384 bytes]"
	}
	if text == "" {
		return nil
	}
	return errors.New(text)
}
func failure(operation string, cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", Err, operation)
	}
	return fmt.Errorf("%w: %s: %w", Err, operation, cause)
}
