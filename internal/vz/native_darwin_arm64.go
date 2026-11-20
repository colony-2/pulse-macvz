//go:build darwin && arm64

package vz

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unsafe"

	cf "github.com/tmc/apple/corefoundation"
	"github.com/tmc/apple/foundation"
	"github.com/tmc/apple/security"
	vz "github.com/tmc/apple/virtualization"
	configx "github.com/tmc/apple/x/vzkit/config"
	storagex "github.com/tmc/apple/x/vzkit/storage"
	vmx "github.com/tmc/apple/x/vzkit/vm"
)

// The main thread pumps CoreFoundation while native work runs on its serial queue.
func init() {
	if len(os.Args) > 1 && os.Args[1] == "__pulse-vm" {
		runtime.LockOSThread()
	}
}

type nativeVM struct {
	machine vz.VZVirtualMachine
	queue   *vmx.Queue
}

func newIdentity() ([]byte, error) {
	id := vz.NewVZMacMachineIdentifier()
	if id.ID == 0 {
		return nil, errors.New("create machine identifier")
	}
	data := storagex.NSDataToBytes(id.DataRepresentation())
	if len(data) == 0 {
		return nil, errors.New("empty machine identifier")
	}
	return data, nil
}

func nativeConfig(c diskConfig, dir string) (vz.VZVirtualMachineConfiguration, error) {
	cfg := vz.NewVZVirtualMachineConfiguration()
	cfg.SetCPUCount(c.CPU)
	cfg.SetMemorySize(c.Memory)
	if c.OS == "darwin" {
		if len(c.Hardware) == 0 || len(c.Identity) == 0 {
			return cfg, errors.New("missing macOS hardware model or machine identifier")
		}
		hw := vz.NewMacHardwareModelWithDataRepresentation(storagex.NSDataFromBytes(c.Hardware))
		id := vz.NewMacMachineIdentifierWithDataRepresentation(storagex.NSDataFromBytes(c.Identity))
		aux := vz.NewMacAuxiliaryStorageWithContentsOfURL(foundation.NewURLFileURLWithPath(filepath.Join(dir, "nvram.bin")))
		if hw.ID == 0 || id.ID == 0 || aux.ID == 0 || !hw.IsSupported() {
			return cfg, errors.New("invalid or unsupported macOS identity")
		}
		platform := vz.NewVZMacPlatformConfiguration()
		platform.SetHardwareModel(hw)
		platform.SetMachineIdentifier(id)
		platform.SetAuxiliaryStorage(aux)
		cfg.SetPlatform(platform)
		cfg.SetBootLoader(vz.NewVZMacOSBootLoader())
		graphics := vz.NewVZMacGraphicsDeviceConfiguration()
		graphics.SetDisplays([]vz.VZMacGraphicsDisplayConfiguration{vz.NewMacGraphicsDisplayConfigurationWithWidthInPixelsHeightInPixelsPixelsPerInch(1024, 768, 80)})
		configx.SetMacGraphicsDevices(cfg, graphics)
		configx.SetKeyboards(cfg, vz.NewVZMacKeyboardConfiguration())
		configx.SetPointingDevices(cfg, []vz.IVZPointingDeviceConfiguration{vz.NewVZMacTrackpadConfiguration()})
		if c.TartGuestCompatibility {
			// Only the image's disk-layout guard is supported, not Tart RPC or clipboard.
			port := vz.NewVZVirtioConsolePortConfiguration()
			port.SetName("tart-version-2.0.0-pulse-vz")
			console := vz.NewVZVirtioConsoleDeviceConfiguration()
			console.Ports().SetObjectAtIndexedSubscript(port, 0)
			cfg.SetConsoleDevices([]vz.VZConsoleDeviceConfiguration{vz.VZConsoleDeviceConfigurationFromID(console.ID)})
		}
	} else if c.OS == "linux" {
		boot := vz.NewVZEFIBootLoader()
		store := vz.NewEFIVariableStoreWithURL(foundation.NewURLFileURLWithPath(filepath.Join(dir, "nvram.bin")))
		if store.ID == 0 {
			return cfg, errors.New("open EFI variable store")
		}
		boot.SetVariableStore(store)
		cfg.SetBootLoader(boot)
		platform := vz.NewVZGenericPlatformConfiguration()
		if nestedVirtualizationSupported() {
			platform.SetNestedVirtualizationEnabled(true)
		}
		cfg.SetPlatform(platform)
		configx.SetEntropyDevices(cfg, vz.NewVZVirtioEntropyDeviceConfiguration())
	} else {
		return cfg, fmt.Errorf("unsupported guest OS %q", c.OS)
	}
	caching, synchronization := vz.VZDiskImageCachingModeUncached, vz.VZDiskImageSynchronizationModeFsync
	if c.OS == "linux" {
		// Match Tart's workaround for Linux virtio disk corruption with uncached I/O.
		// https://github.com/cirruslabs/tart/pull/675
		// macOS retains its existing attachment settings for snapshot compatibility.
		caching, synchronization = vz.VZDiskImageCachingModeCached, vz.VZDiskImageSynchronizationModeFull
	}
	disk, err := vz.NewDiskImageStorageDeviceAttachmentWithURLReadOnlyCachingModeSynchronizationModeError(
		foundation.NewURLFileURLWithPath(filepath.Join(dir, "disk.img")), false, caching, synchronization)
	if err != nil {
		return cfg, err
	}
	configx.SetStorageDevices(cfg, vz.NewVirtioBlockDeviceConfigurationWithAttachment(disk))
	network := vz.NewVZVirtioNetworkDeviceConfiguration()
	network.SetAttachment(vz.NewVZNATNetworkDeviceAttachment())
	mac := vz.NewMACAddressWithString(c.MAC)
	if mac.ID == 0 {
		return cfg, errors.New("invalid MAC address")
	}
	network.SetMACAddress(mac)
	configx.SetNetworkDevices(cfg, network)
	if ok, err := cfg.ValidateWithError(); err != nil || !ok {
		return cfg, fmt.Errorf("validate configuration: %v: %w", ok, err)
	}
	if c.OS == "darwin" {
		if ok, err := cfg.ValidateSaveRestoreSupportWithError(); err != nil || !ok {
			return cfg, fmt.Errorf("validate save/restore: %v: %w", ok, err)
		}
	}
	return cfg, nil
}

func newNativeVM(cfg vz.VZVirtualMachineConfiguration) (*nativeVM, error) {
	q := vmx.NewQueue("com.colony.pulse-vz.runner")
	machine := vmx.Create(cfg, q)
	if machine.ID == 0 {
		return nil, errors.New("create virtual machine")
	}
	return &nativeVM{machine: machine, queue: q}, nil
}

func (m *nativeVM) operation(name string, timeout time.Duration, action func(vz.ErrorHandler)) error {
	fmt.Printf("native %s requested\n", name)
	done := make(chan error, 1)
	m.queue.Async(func() {
		action(func(err error) {
			// Materialize native error text on the callback's thread/lifetime.
			if err != nil {
				err = errors.New(err.Error())
			}
			done <- err
		})
	})
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case err := <-done:
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			fmt.Printf("native %s completed\n", name)
			return nil
		case <-deadline.C:
			return fmt.Errorf("%s timed out after %s", name, timeout)
		default:
			vmx.RunLoopOnce()
		}
	}
}
func (m *nativeVM) Start() error {
	return m.operation("start", time.Minute, m.machine.StartWithCompletionHandler)
}
func (m *nativeVM) Pause() error {
	return m.operation("pause", 30*time.Second, m.machine.PauseWithCompletionHandler)
}
func (m *nativeVM) Resume() error {
	return m.operation("resume", 30*time.Second, m.machine.ResumeWithCompletionHandler)
}
func (m *nativeVM) Stop() error {
	if m.State() == vz.VZVirtualMachineStateStopped {
		return nil
	}
	err := m.operation("stop", 30*time.Second, m.machine.StopWithCompletionHandler)
	if m.State() == vz.VZVirtualMachineStateStopped {
		return nil
	}
	return err
}
func (m *nativeVM) State() vz.VZVirtualMachineState { return vmx.State(m.queue, m.machine) }
func (m *nativeVM) SaveMachineStateToPath(path string) error {
	url := foundation.NewURLFileURLWithPath(path)
	url.Retain()
	return m.operation("save", 2*time.Minute, func(cb vz.ErrorHandler) { m.machine.SaveMachineStateToURLCompletionHandler(url, cb) })
}
func (m *nativeVM) RestoreMachineStateFromURL(path string) error {
	url := foundation.NewURLFileURLWithPath(path)
	url.Retain()
	return m.operation("restore", time.Minute, func(cb vz.ErrorHandler) { m.machine.RestoreMachineStateFromURLCompletionHandler(url, cb) })
}

// CF APIs expose some opaque references as uintptr and others as unsafe.Pointer.
// Reinterpret the reference bits; no Go allocation or native memory is accessed.
func cfReference(ref uintptr) cf.CFTypeRef { return *(*cf.CFTypeRef)(unsafe.Pointer(&ref)) }

func nestedVirtualizationSupported() bool {
	// The class property was introduced in macOS 15. Do not send the selector
	// on macOS 14, which remains supported for ordinary VMs.
	release, err := unix.Sysctl("kern.osrelease")
	if err != nil {
		return false
	}
	major, err := strconv.Atoi(strings.Split(release, ".")[0])
	return err == nil && major >= 24 && vz.GetVZGenericPlatformConfigurationClass().IsNestedVirtualizationSupported()
}

func checkNative() error {
	release, err := unix.Sysctl("kern.osrelease")
	if err != nil {
		return err
	}
	major, err := strconv.Atoi(strings.Split(release, ".")[0])
	if err != nil || major < 23 {
		return ErrPlatform
	}
	if !vz.GetVZVirtualMachineClass().IsSupported() {
		return ErrPlatform
	}
	task := security.SecTaskCreateFromSelf(0)
	if task == 0 {
		return errors.New("cannot inspect runner entitlement")
	}
	defer cf.CFRelease(cfReference(uintptr(task)))
	key := cf.CFStringCreateWithCString(0, "com.apple.security.virtualization", 0x08000100)
	defer cf.CFRelease(cfReference(uintptr(key)))
	value := security.SecTaskCopyValueForEntitlement(task, key, nil)
	if value == nil {
		return errors.New("runner requires com.apple.security.virtualization entitlement")
	}
	defer cf.CFRelease(value)
	if cf.CFGetTypeID(value) != cf.CFBooleanGetTypeID() || !cf.CFBooleanGetValue(cf.CFBooleanRef(value)) {
		return errors.New("runner virtualization entitlement is not true")
	}
	return nil
}
func runNative(dir string, c diskConfig, requests <-chan string) error {
	if err := checkNative(); err != nil {
		return err
	}
	cfg, err := nativeConfig(c, dir)
	if err != nil {
		return err
	}
	machine, err := newNativeVM(cfg)
	if err != nil {
		return err
	}
	state := filepath.Join(dir, "state.vzvmsave")
	if c.RestoreRequired {
		if err = validateSnapshot(dir, c); err != nil {
			return err
		}
		if err = machine.RestoreMachineStateFromURL(state); err != nil {
			return err
		}
		if err = machine.Resume(); err != nil {
			return err
		}
		if err = os.Remove(state); err != nil {
			return err
		}
		if err = syncDir(dir); err != nil {
			return err
		}
	} else {
		if _, err = os.Lstat(state); !os.IsNotExist(err) {
			return errors.New("untracked snapshot; refusing cold boot")
		}
		if err = machine.Start(); err != nil {
			return err
		}
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case op := <-requests:
			if op == "suspend" {
				if c.OS != "darwin" {
					return errors.New("Linux snapshots are unsupported")
				}
				host, err := hostBuild()
				if err != nil {
					return err
				}
				c.RestoreRequired = true
				c.Snapshot = &snapshot{Host: host, Layout: deviceLayout, ConfigHash: configHash(c)}
				// Persist restore intent before saving: interrupted saves can never cold boot.
				if err = atomicJSON(filepath.Join(dir, "config.json"), c); err != nil {
					return err
				}
				if err = machine.Pause(); err != nil {
					return err
				}
				if err = machine.SaveMachineStateToPath(state + ".tmp"); err != nil {
					return err
				}
				f, err := os.Open(state + ".tmp")
				if err != nil {
					return err
				}
				err = f.Sync()
				f.Close()
				if err != nil {
					return err
				}
				if err = os.Rename(state+".tmp", state); err != nil {
					return err
				}
				return syncDir(dir)
			}
			return machine.Stop()
		case <-ticker.C:
			vmx.RunLoopOnce()
			switch machine.State() {
			case vz.VZVirtualMachineStateStopped:
				return nil
			case vz.VZVirtualMachineStateError:
				return errors.New("VM entered error state")
			}
		}
	}
}
