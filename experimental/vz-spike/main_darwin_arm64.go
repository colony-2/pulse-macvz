//go:build darwin && arm64

// Command vz-spike exercises tmc/apple on disposable copies of VM images.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	vz "github.com/tmc/apple/virtualization"
	vmx "github.com/tmc/apple/x/vzkit/vm"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

type imageConfig struct {
	OS       string `json:"os"`
	CPU      uint   `json:"cpuCount"`
	Memory   uint64 `json:"memorySize"`
	MAC      string `json:"macAddress"`
	Hardware []byte `json:"hardwareModel"`
	Identity []byte `json:"ecid"`
}

func main() {
	if err := command(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func command(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: vz-spike probe | clone SOURCE DEST | run VM_DIRECTORY | ssh VM_DIRECTORY")
	}
	switch args[0] {
	case "probe":
		return probeNative()
	case "clone":
		if len(args) != 3 {
			return errors.New("usage: vz-spike clone SOURCE DEST")
		}
		return clone(args[1], args[2])
	case "run":
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		fresh := fs.Bool("fresh", false, "assign new identity before cold boot")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("usage: vz-spike run [-fresh] VM_DIRECTORY")
		}
		return run(fs.Arg(0), *fresh)
	case "ssh":
		fs := flag.NewFlagSet("ssh", flag.ContinueOnError)
		timeout := fs.Duration("timeout", 3*time.Minute, "maximum wait for guest SSH")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 || *timeout <= 0 {
			return errors.New("usage: vz-spike ssh [-timeout DURATION] VM_DIRECTORY")
		}
		return waitSSH(fs.Arg(0), *timeout)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// Clone only VM data, never Pulse ownership, locks, or delivery credentials.
// External sources must be stopped; only spike-owned sources may carry state.
func clone(src, dst string) error {
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		return fmt.Errorf("destination must be absent: %s", dst)
	}
	owned := false
	if _, err := os.Stat(filepath.Join(src, "spike-owned")); err == nil {
		owned = true
	}
	lock, err := os.OpenFile(filepath.Join(src, "disk.img"), os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		return fmt.Errorf("source disk busy: %w", err)
	}
	if owned {
		runLock, err := os.OpenFile(filepath.Join(src, "spike.lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		defer runLock.Close()
		if err = unix.Flock(int(runLock.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
			return fmt.Errorf("source runner active: %w", err)
		}
	}
	files := []string{"config.json", "disk.img", "nvram.bin"}
	if _, err := os.Stat(filepath.Join(src, "state.vzvmsave")); err == nil {
		if !owned {
			return errors.New("external source must be cold; create VZ snapshots with this spike")
		}
		files = append(files, "state.vzvmsave")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err = os.Mkdir(dst, 0700); err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(dst)
		}
	}()
	for _, name := range files {
		if err = unix.Clonefile(filepath.Join(src, name), filepath.Join(dst, name), 0); err != nil {
			return err
		}
	}
	if err = os.WriteFile(filepath.Join(dst, "spike-owned"), []byte("disposable VZ spike\n"), 0600); err != nil {
		return err
	}
	ok = true
	return nil
}

// Apple DHCP leases omit leading zeroes in MAC octets.
func leaseMAC(value string) string {
	parts := strings.Split(value, ":")
	if len(parts) != 6 {
		return ""
	}
	mac := make(net.HardwareAddr, 6)
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 16, 8)
		if err != nil {
			return ""
		}
		mac[i] = byte(n)
	}
	return mac.String()
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
		if leaseMAC(hw) == mac && net.ParseIP(ip) != nil {
			out = append(out, ip)
		}
	}
	return out
}
func waitSSH(dir string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	start := time.Now()
	var last error
	for ctx.Err() == nil {
		// The runner assigns fresh identity atomically before boot. Re-read while waiting.
		data, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err != nil {
			return err
		}
		var c imageConfig
		if err = json.Unmarshal(data, &c); err != nil {
			return err
		}
		leases, err := os.ReadFile("/var/db/dhcpd_leases")
		if err != nil {
			return err
		}
		for _, ip := range leaseIPs(string(leases), c.MAC) {
			output, err := checkSSH(ctx, ip, c)
			if err != nil {
				if last == nil || last.Error() != err.Error() {
					fmt.Fprintf(os.Stderr, "SSH pending at %s: %v\n", ip, err)
				}
				last = err
				continue
			}
			fmt.Printf("SSH ready after %s at %s: %s", time.Since(start), ip, output)
			return nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("SSH timeout: %w", errors.Join(ctx.Err(), last))
}
func checkSSH(ctx context.Context, ip string, c imageConfig) ([]byte, error) {
	address := net.JoinHostPort(ip, "22")
	socket, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	defer socket.Close()
	stop := context.AfterFunc(ctx, func() { socket.Close() })
	defer stop()
	if err = socket.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return nil, err
	}
	cc, ch, req, err := ssh.NewClientConn(socket, address, &ssh.ClientConfig{User: "admin", Auth: []ssh.AuthMethod{ssh.Password("admin")}, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	if err != nil {
		return nil, err
	}
	client := ssh.NewClient(cc, ch, req)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer session.Close()
	cmd := "uname -s; uname -m; ifconfig en0"
	expected := "Darwin"
	if c.OS == "linux" {
		cmd = "uname -s; uname -m; cat /sys/class/net/*/address"
		expected = "Linux"
	}
	output, err := session.CombinedOutput(cmd)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(string(output), expected+"\n") || !strings.Contains(string(output), c.MAC) {
		return nil, errors.New("SSH endpoint does not match guest OS/MAC")
	}
	return output, nil
}
func run(dir string, fresh bool) error {
	if _, err := os.Stat(filepath.Join(dir, "spike-owned")); err != nil {
		return errors.New("run requires a disposable directory made by vz-spike clone")
	}
	lock, err := os.OpenFile(filepath.Join(dir, "spike.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return err
	}
	started := time.Now()
	if err := os.WriteFile(filepath.Join(dir, "runner.pid"), []byte(fmt.Sprint(os.Getpid())), 0600); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return err
	}
	var c imageConfig
	if err = json.Unmarshal(data, &c); err != nil {
		return err
	}
	state := filepath.Join(dir, "state.vzvmsave")
	if fresh {
		if _, err := os.Stat(state); !os.IsNotExist(err) {
			return fmt.Errorf("fresh identity requires no saved state")
		}
		mac := make(net.HardwareAddr, 6)
		if _, err = rand.Read(mac); err != nil {
			return err
		}
		mac[0] = (mac[0] | 2) & 0xfe
		c.MAC = mac.String()
		if c.OS == "darwin" {
			id, e := newIdentity()
			if e != nil {
				return e
			}
			c.Identity = id
		}
		data, err = json.Marshal(c)
		if err != nil {
			return err
		}
		if err = os.WriteFile(filepath.Join(dir, "config.json.tmp"), data, 0600); err != nil {
			return err
		}
		if err = os.Rename(filepath.Join(dir, "config.json.tmp"), filepath.Join(dir, "config.json")); err != nil {
			return err
		}
	}
	cfg, err := nativeConfig(c, dir)
	if err != nil {
		return err
	}
	fmt.Printf("validated os=%s mac=%s (cgo-free)\n", c.OS, c.MAC)
	vm, err := newNativeVM(cfg)
	if err != nil {
		return err
	}

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGUSR1, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sig)
	if _, err := os.Stat(state); err == nil {
		if err = vm.RestoreMachineStateFromURL(state); err != nil {
			return fmt.Errorf("restore: %w", err)
		}
		if err = vm.Resume(); err != nil {
			return fmt.Errorf("resume: %w", err)
		}
		if err = os.Remove(state); err != nil {
			return err
		}
		fmt.Printf("restored in %s\n", time.Since(started))
	} else if os.IsNotExist(err) {
		if err = vm.Start(); err != nil {
			return fmt.Errorf("start: %w", err)
		}
		fmt.Printf("started in %s\n", time.Since(started))
	} else {
		return err
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case s := <-sig:
			if s == syscall.SIGUSR1 {
				if err = vm.Pause(); err != nil {
					return err
				}
				if err = vm.SaveMachineStateToPath(state + ".tmp"); err != nil {
					return err
				}
				if err = os.Rename(state+".tmp", state); err != nil {
					return err
				}
				fmt.Println("saved state")
				return nil
			}
			return vm.Stop()
		case <-ticker.C:
			vmx.RunLoopOnce()
			s := vm.State()
			if s == vz.VZVirtualMachineStateStopped {
				return nil
			}
			if s == vz.VZVirtualMachineStateError {
				return fmt.Errorf("VM entered error state")
			}
		}
	}
}
