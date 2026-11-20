//go:build darwin && arm64

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLeaseIPs(t *testing.T) {
	leases := "{\n ip_address=192.168.64.2\n hw_address=1,a:0:b:1:2:c\n}\n{\n ip_address=invalid\n hw_address=1,0a:00:0b:01:02:0c\n}\n{\n ip_address=192.168.64.3\n hw_address=1,aa:bb:cc:dd:ee:ff\n}"
	got := leaseIPs(leases, "0a:00:0b:01:02:0c")
	if len(got) != 1 || got[0] != "192.168.64.2" {
		t.Fatalf("IPs = %v", got)
	}
	for _, mac := range []string{"", ":0:b:1:2:c", "100:0:b:1:2:c", "1:2:3"} {
		if leaseMAC(mac) != "" {
			t.Fatalf("accepted invalid MAC %q", mac)
		}
	}
}
func fixture(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	for _, name := range []string{"config.json", "disk.img", "nvram.bin", "pulse.json", "pulse.run.lock"} {
		if err := os.WriteFile(filepath.Join(src, name), []byte("source-"+name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return src
}
func TestCloneCopiesOnlyVMData(t *testing.T) {
	src := fixture(t)
	dst := filepath.Join(t.TempDir(), "clone")
	if err := clone(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "pulse.json")); !os.IsNotExist(err) {
		t.Fatal("copied ownership credentials")
	}
	if _, err := os.Stat(filepath.Join(dst, "spike-owned")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "disk.img"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(src, "disk.img"))
	if string(got) != "source-disk.img" {
		t.Fatal("clone modified source")
	}
	if err := clone(src, dst); err == nil {
		t.Fatal("overwrote existing destination")
	}
}
func TestCloneSuspendedIdentityAndExclusion(t *testing.T) {
	src := fixture(t)
	dst := filepath.Join(t.TempDir(), "clone")
	if err := os.WriteFile(filepath.Join(src, "state.vzvmsave"), []byte("snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := clone(src, dst); err == nil {
		t.Fatal("accepted external suspended source")
	}
	if err := os.WriteFile(filepath.Join(src, "spike-owned"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(src, "spike.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err = clone(src, dst); err == nil {
		t.Fatal("cloned running source")
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err = clone(src, dst); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config.json", "nvram.bin", "state.vzvmsave"} {
		a, _ := os.ReadFile(filepath.Join(src, name))
		b, _ := os.ReadFile(filepath.Join(dst, name))
		if string(a) != string(b) {
			t.Fatalf("changed %s", name)
		}
	}
}
func TestRunRefusesExternalDirectory(t *testing.T) {
	src := fixture(t)
	if err := run(src, true); err == nil || !strings.Contains(err.Error(), "disposable") {
		t.Fatalf("run = %v", err)
	}
	if _, err := os.Stat(filepath.Join(src, "spike.lock")); !os.IsNotExist(err) {
		t.Fatal("modified external source")
	}
}
func TestCloneRollsBackIncompleteCopy(t *testing.T) {
	src := fixture(t)
	dst := filepath.Join(t.TempDir(), "clone")
	if err := os.Remove(filepath.Join(src, "nvram.bin")); err != nil {
		t.Fatal(err)
	}
	if err := clone(src, dst); err == nil {
		t.Fatal("accepted missing NVRAM")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("left partial destination")
	}
}
