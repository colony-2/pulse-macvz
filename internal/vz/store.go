// Package vz implements the cgo-free local Virtualization.framework backend.
package vz

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/colony-2/pulse-tart/internal/backend"
	"github.com/colony-2/pulse-tart/internal/config"
	"github.com/colony-2/pulse-tart/internal/local"
	"github.com/colony-2/pulse-tart/internal/vz/image"
	"golang.org/x/sys/unix"
)

const layoutVersion = 1
const runnerProtocol = "pulse-vz/1"
const deviceLayout = "tmc-4c51c1664813-lean-v1"

var ErrPlatform = errors.New("VZ requires macOS 14 or later on Apple Silicon")

type diskConfig struct {
	OS              string    `json:"os"`
	CPU             uint      `json:"cpuCount"`
	Memory          uint64    `json:"memorySize"`
	MAC             string    `json:"macAddress"`
	Hardware        []byte    `json:"hardwareModel,omitempty"`
	Identity        []byte    `json:"ecid,omitempty"`
	Version         int       `json:"vzVersion,omitempty"`
	Arch            string    `json:"arch,omitempty"`
	RestoreRequired bool      `json:"restoreRequired,omitempty"`
	Snapshot        *snapshot `json:"snapshot,omitempty"`
}
type snapshot struct{ Host, Layout, ConfigHash string }

func readConfig(dir string) (diskConfig, error) {
	var c diskConfig
	data, err := readRegular(filepath.Join(dir, "config.json"), 1<<20)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(data, &c); err != nil {
		return c, err
	}
	if c.OS != "darwin" && c.OS != "linux" {
		return c, errors.New("unsupported VM OS")
	}
	if c.CPU < 1 || c.Memory < 512<<20 {
		return c, errors.New("invalid VM CPU/memory")
	}
	if _, err = net.ParseMAC(c.MAC); err != nil {
		return c, err
	}
	return c, nil
}
func readRegular(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("expected bounded regular file: %s", path)
	}
	return os.ReadFile(path)
}
func atomicJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".vz-write-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
func syncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func configHash(c diskConfig) string {
	c.Snapshot = nil
	c.RestoreRequired = false
	data, _ := json.Marshal(c)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func validateOwned(c diskConfig) error {
	if c.Version != layoutVersion || c.Arch != "arm64" {
		return errors.New("unsupported VZ layout or architecture")
	}
	return nil
}
func lockFile(ctx context.Context, path string, shared, wait bool) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	mode := unix.LOCK_EX
	if shared {
		mode = unix.LOCK_SH
	}
	for {
		if err = ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		err = unix.Flock(int(f.Fd()), mode|unix.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !wait || (!errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN)) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
func lockHeld(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, unix.Flock(int(f.Fd()), unix.LOCK_UN)
}

type Store struct {
	Home, Runner         string
	Timeout, PullTimeout time.Duration
}

func (s *Store) path(name string) (string, error) {
	if !config.Name(name) {
		return "", config.Err
	}
	path := filepath.Join(s.Home, "vms", name)
	if info, err := os.Lstat(path); err == nil && !info.IsDir() {
		return "", errors.New("VM directory must not be a symlink or file")
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return path, nil
}
func (s *Store) Inspect(ctx context.Context, name string) (local.Hardware, error) {
	var h local.Hardware
	if err := ctx.Err(); err != nil {
		return h, err
	}
	dir, err := s.path(name)
	if err != nil {
		return h, err
	}
	c, err := readConfig(dir)
	if err != nil {
		return h, err
	}
	if err = validateOwned(c); err != nil {
		return h, err
	}
	running, err := lockHeld(filepath.Join(dir, "pulse.run.lock"))
	if err != nil {
		return h, err
	}
	native, err := lockHeld(filepath.Join(dir, "vz.lock"))
	if err != nil {
		return h, err
	}
	running = running || native
	state := "stopped"
	if _, err = os.Lstat(filepath.Join(dir, "state.vzvmsave")); err == nil {
		state = "suspended"
	} else if !os.IsNotExist(err) {
		return h, err
	}
	if running {
		state = "running"
	}
	return local.Hardware{OS: c.OS, Name: name, CPU: int64(c.CPU), Memory: int64(c.Memory >> 20), MACAddress: c.MAC, State: state, Running: running}, nil
}
func (s *Store) List(ctx context.Context) ([]local.Hardware, error) {
	result := []local.Hardware{}
	entries, err := os.ReadDir(filepath.Join(s.Home, "vms"))
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if !e.IsDir() {
			return nil, errors.New("unexpected entry in VZ inventory")
		}
		h, err := s.Inspect(ctx, e.Name())
		if err != nil {
			return nil, err
		}
		result = append(result, h)
	}
	return result, nil
}

// PrepareSource imports a registry image without creating or starting a VM.
// The local client runs it outside its lifecycle lock.
func (s *Store) PrepareSource(ctx context.Context, reference string) error {
	ctx, cancel := context.WithTimeout(ctx, s.PullTimeout)
	defer cancel()
	_, err := s.cache(ctx, reference)
	return err
}

func (s *Store) cache(ctx context.Context, reference string) (string, error) {
	backend.ReportProgress(ctx, backend.TransferProgress{Stage: "waiting_cache"})
	sum := sha256.Sum256([]byte(reference))
	key := hex.EncodeToString(sum[:])
	root := filepath.Join(s.Home, "cache")
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	lock, err := lockFile(ctx, filepath.Join(root, key+".lock"), false, true)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	dst := filepath.Join(root, key)
	if data, err := readRegular(filepath.Join(dst, "verified.json"), 4096); err == nil {
		var saved string
		if json.Unmarshal(data, &saved) != nil || saved != reference {
			return "", errors.New("cache provenance mismatch")
		}
		backend.ReportProgress(ctx, backend.TransferProgress{Stage: "cached"})
		return dst, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if _, err = os.Lstat(dst); !os.IsNotExist(err) {
		return "", errors.New("incomplete cache entry; refusing to reuse")
	}
	temp, err := os.MkdirTemp(root, ".pull-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temp)
	stage := filepath.Join(temp, "image")
	if err = image.Pull(ctx, reference, stage); err != nil {
		return "", err
	}
	c, err := readConfig(stage)
	if err != nil {
		return "", err
	}
	if c.Arch != "arm64" {
		return "", errors.New("OCI VM image must declare arm64 architecture")
	}
	c.Version = layoutVersion
	c.RestoreRequired = false
	c.Snapshot = nil
	if err = atomicJSON(filepath.Join(stage, "config.json"), c); err != nil {
		return "", err
	}
	if err = atomicJSON(filepath.Join(stage, "verified.json"), reference); err != nil {
		return "", err
	}
	if err = os.Rename(stage, dst); err != nil {
		return "", err
	}
	return dst, syncDir(root)
}
func (s *Store) Clone(ctx context.Context, source, destination string) error {
	ctx, cancel := context.WithTimeout(ctx, s.PullTimeout)
	defer cancel()
	dst, err := s.path(destination)
	if err != nil {
		return err
	}
	var src string
	if strings.Contains(source, "@sha256:") {
		src, err = s.cache(ctx, source)
	} else {
		src, err = s.path(source)
	}
	if err != nil {
		return err
	}
	if _, err = os.Lstat(dst); !os.IsNotExist(err) {
		return errors.New("clone destination must be absent")
	}
	lock, err := lockFile(ctx, filepath.Join(src, "vz.lock"), true, false)
	if err != nil {
		return fmt.Errorf("clone source busy: %w", err)
	}
	defer lock.Close()
	if held, err := lockHeld(filepath.Join(src, "pulse.run.lock")); err != nil || held {
		return fmt.Errorf("source runner active or unavailable: %v", err)
	}
	c, err := readConfig(src)
	if err != nil {
		return err
	}
	if err = validateOwned(c); err != nil {
		return err
	}
	files := []string{"config.json", "disk.img", "nvram.bin"}
	if c.RestoreRequired {
		if err = validateSnapshot(src, c); err != nil {
			return err
		}
		files = append(files, "state.vzvmsave")
	} else if _, err = os.Lstat(filepath.Join(src, "state.vzvmsave")); !os.IsNotExist(err) {
		return errors.New("unexpected saved state")
	}
	root := filepath.Dir(dst)
	if err = os.MkdirAll(root, 0700); err != nil {
		return err
	}
	temp, err := os.MkdirTemp(root, ".clone-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	for _, file := range files {
		if err = ctx.Err(); err != nil {
			return err
		}
		info, err := os.Lstat(filepath.Join(src, file))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("clone source file must be regular")
		}
		if err = cloneFile(filepath.Join(src, file), filepath.Join(temp, file)); err != nil {
			return err
		}
		if err = os.Chmod(filepath.Join(temp, file), 0600); err != nil {
			return err
		}
	}
	if err = syncDir(temp); err != nil {
		return err
	}
	// Exclusive rename prevents replacing a concurrently published destination.
	if err = renameExclusive(temp, dst); err != nil {
		return err
	}
	return syncDir(root)
}
func (s *Store) Configure(ctx context.Context, name string, o local.MachineOptions) error {
	dir, err := s.path(name)
	if err != nil {
		return err
	}
	lock, err := lockFile(ctx, filepath.Join(dir, "vz.lock"), false, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	if held, err := lockHeld(filepath.Join(dir, "pulse.run.lock")); err != nil || held {
		return errors.New("configure requires stopped VM")
	}
	c, err := readConfig(dir)
	if err != nil {
		return err
	}
	if err = validateOwned(c); err != nil {
		return err
	}
	if c.RestoreRequired {
		return errors.New("cannot reconfigure a snapshot")
	}
	if _, err = os.Lstat(filepath.Join(dir, "state.vzvmsave")); !os.IsNotExist(err) {
		return errors.New("cannot reconfigure saved state")
	}
	if o.CPU < 1 || o.CPU > 1024 || o.Memory < 512 || o.Memory > 1<<30 {
		return config.Err
	}
	c.CPU = uint(o.CPU)
	c.Memory = uint64(o.Memory) << 20
	if o.RandomMAC {
		mac := make(net.HardwareAddr, 6)
		if _, err = rand.Read(mac); err != nil {
			return err
		}
		mac[0] = (mac[0] | 2) & 0xfe
		c.MAC = mac.String()
	}
	if o.RandomSerial {
		data, err := s.command(ctx, "identity")
		if err != nil {
			return err
		}
		if err = json.Unmarshal(data, &c.Identity); err != nil {
			return err
		}
		if len(c.Identity) == 0 {
			return errors.New("empty machine identity")
		}
	}
	return atomicJSON(filepath.Join(dir, "config.json"), c)
}
func (s *Store) Delete(ctx context.Context, name string) error {
	dir, err := s.path(name)
	if err != nil {
		return err
	}
	lock, err := lockFile(ctx, filepath.Join(dir, "vz.lock"), false, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	if held, err := lockHeld(filepath.Join(dir, "pulse.run.lock")); err != nil || held {
		return errors.New("delete requires stopped runner")
	}
	if _, err = readConfig(dir); err != nil {
		return err
	}
	if err = os.RemoveAll(dir); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dir))
}
func (s *Store) IP(ctx context.Context, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	dir, err := s.path(name)
	if err != nil {
		return "", err
	}
	c, err := readConfig(dir)
	if err != nil {
		return "", err
	}
	for {
		data, err := os.ReadFile("/var/db/dhcpd_leases")
		if err != nil {
			return "", err
		}
		ips := leaseIPs(string(data), c.MAC)
		if len(ips) > 0 {
			return ips[len(ips)-1], nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func leaseIPs(data, mac string) []string {
	var out []string
	for _, block := range strings.Split(data, "}") {
		var ip, hw string
		for _, line := range strings.Split(block, "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
			if !ok {
				continue
			}
			if k == "ip_address" {
				ip = v
			}
			if k == "hw_address" {
				_, hw, _ = strings.Cut(v, ",")
			}
		}
		parts := strings.Split(hw, ":")
		if len(parts) != 6 {
			continue
		}
		bytes := make(net.HardwareAddr, 6)
		valid := true
		for i, p := range parts {
			n, e := strconv.ParseUint(p, 16, 8)
			if e != nil {
				valid = false
				break
			}
			bytes[i] = byte(n)
		}
		if valid && bytes.String() == strings.ToLower(mac) && net.ParseIP(ip) != nil {
			out = append(out, ip)
		}
	}
	return out
}
