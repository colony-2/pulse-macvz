//go:build darwin && arm64

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/tmc/apple/foundation"
	vz "github.com/tmc/apple/virtualization"
	configx "github.com/tmc/apple/x/vzkit/config"
	storagex "github.com/tmc/apple/x/vzkit/storage"
	vmx "github.com/tmc/apple/x/vzkit/vm"
)

// The main thread pumps CoreFoundation while native work runs on its serial queue.
func init() { runtime.LockOSThread() }

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

func nativeConfig(c imageConfig, dir string) (vz.VZVirtualMachineConfiguration, error) {
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
	} else if c.OS == "linux" {
		boot := vz.NewVZEFIBootLoader()
		store := vz.NewEFIVariableStoreWithURL(foundation.NewURLFileURLWithPath(filepath.Join(dir, "nvram.bin")))
		if store.ID == 0 {
			return cfg, errors.New("open EFI variable store")
		}
		boot.SetVariableStore(store)
		cfg.SetBootLoader(boot)
		cfg.SetPlatform(vz.NewVZGenericPlatformConfiguration())
		configx.SetEntropyDevices(cfg, vz.NewVZVirtioEntropyDeviceConfiguration())
	} else {
		return cfg, fmt.Errorf("unsupported guest OS %q", c.OS)
	}
	disk, err := vz.NewDiskImageStorageDeviceAttachmentWithURLReadOnlyCachingModeSynchronizationModeError(
		foundation.NewURLFileURLWithPath(filepath.Join(dir, "disk.img")), false, vz.VZDiskImageCachingModeUncached, vz.VZDiskImageSynchronizationModeFsync)
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
	q := vmx.NewQueue("com.colony.pulse-vz.spike")
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
func probeNative() error {
	dir, err := os.MkdirTemp("", "pulse-vz-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	store, err := vz.NewEFIVariableStoreCreatingVariableStoreAtURLOptionsError(foundation.NewURLFileURLWithPath(filepath.Join(dir, "nvram.bin")), 0)
	if err != nil {
		return err
	}
	boot := vz.NewVZEFIBootLoader()
	boot.SetVariableStore(store)
	cfg := vz.NewVZVirtualMachineConfiguration()
	cfg.SetCPUCount(2)
	cfg.SetMemorySize(512 << 20)
	cfg.SetBootLoader(boot)
	if ok, err := cfg.ValidateWithError(); err != nil || !ok {
		return fmt.Errorf("validate: %v: %w", ok, err)
	}
	fmt.Println("configuration valid (cgo-free)")
	machine, err := newNativeVM(cfg)
	if err != nil {
		return err
	}
	if err = machine.Start(); err != nil {
		return err
	}
	fmt.Println("firmware VM started")
	for until := time.Now().Add(time.Second); time.Now().Before(until); {
		vmx.RunLoopOnce()
	}
	if err = machine.Stop(); err != nil {
		return err
	}
	fmt.Println("firmware VM stopped")
	return nil
}
