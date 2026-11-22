package macvz

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

type vzQualificationCache struct {
	dir      string
	restored bool
}

func openVZQualificationCache(t *testing.T, home string, cfg Config) *vzQualificationCache {
	t.Helper()
	root := os.Getenv("PULSE_VZ_TEST_CACHE")
	if root == "" {
		return nil
	}
	if !filepath.IsAbs(root) {
		t.Fatal("PULSE_VZ_TEST_CACHE must be absolute")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	// Keep cached snapshot identities exclusive across complete test runs.
	lock, err := os.OpenFile(filepath.Join(root, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		t.Fatal("another VZ qualification is using this cache:", err)
	}
	t.Cleanup(func() { lock.Close() })
	hash := sha256.New()
	manifest, err := os.ReadFile(filepath.Join(os.Getenv("PULSE_TEST_OUTPUT_DIR"), "source-sha256.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sources map[string]string
	if err := json.Unmarshal(manifest, &sources); err != nil {
		t.Fatal(err)
	}
	inputs := map[string]string{}
	for name, digest := range sources {
		if vzPreparationInput(name) {
			inputs[name] = digest
		}
	}
	if len(inputs) == 0 {
		t.Fatal("source manifest lacks preparation inputs")
	}
	data, err := json.Marshal(inputs)
	if err != nil {
		t.Fatal(err)
	}
	hash.Write(data)
	// Test state/owner change on each run; image contents do not depend on them.
	cfg.StateDir, cfg.OwnerID = "", ""
	data, err = json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	hash.Write(data)
	host, err := exec.Command("/usr/bin/sw_vers", "-buildVersion").Output()
	if err != nil {
		t.Fatal(err)
	}
	hash.Write(host)
	fmt.Fprintln(hash, "vz-qualification-cache-v1", os.Getenv("PULSE_TEST_C2J_VERSION"))
	if source := os.Getenv("PULSE_VZ_PREPARED_BASE"); source != "" {
		for _, name := range []string{"config.json", "disk.img", "nvram.bin"} {
			info, err := os.Stat(filepath.Join(source, name))
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(hash, source, name, info.Size(), info.ModTime().UnixNano())
		}
	}
	c := &vzQualificationCache{dir: filepath.Join(root, hex.EncodeToString(hash.Sum(nil)))}
	data, err = os.ReadFile(filepath.Join(c.dir, "sources.json"))
	if os.IsNotExist(err) {
		t.Log("VZ prepared-source cache miss:", c.dir)
		return c
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil || len(names) == 0 {
		t.Fatal("invalid prepared-source cache manifest", err)
	}
	for _, name := range names {
		if filepath.Base(name) != name || !strings.HasPrefix(name, "pulse-") {
			t.Fatal("invalid cached source name")
		}
		if err := cloneVZQualificationSource(filepath.Join(c.dir, name), filepath.Join(home, "vms", name)); err != nil {
			t.Fatal(err)
		}
	}
	c.restored = true
	t.Logf("VZ prepared-source cache hit: %d bases/templates", len(names))
	return c
}

func vzPreparationInput(name string) bool {
	// The host TCP forwarder does not participate in guest image preparation
	// or the saved VM configuration. Re-signing/fixing it must not rebuild VMs.
	if strings.HasPrefix(name, "internal/vz/bridge/") {
		return false
	}
	if name == "vz_live_test.go" {
		return true
	}
	if strings.HasSuffix(name, "_test.go") {
		return false
	}
	for _, prefix := range []string{"guest/", "internal/vz/", "internal/config/"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	switch name {
	case "assets.go", "runtime.go", "bases.go", "bases.json", "go.mod", "go.sum", "flake.nix", "flake.lock", "vz_live_test.go",
		"internal/provider/images.go", "internal/provider/slots.go", "internal/provider/engine_images.go",
		"scripts/setup-linux-kvm.sh", "scripts/linux-kvm-network.py", "scripts/setup-linux-nix.sh", "scripts/bootstrap-guest.sh", "scripts/install-guest.sh", "scripts/prepare-image.sh", "scripts/prepare-base.sh", "scripts/prepare-base-linux.sh", "scripts/prepare-docker-engine.sh", "scripts/prepare-template.sh", "scripts/select-c2j.sh", "scripts/resolve-c2j-vendor.sh", "scripts/record-c2j-version.sh",
		"tests/integration/c2j-fixture.go.txt", "tests/integration/docker-fixture.go.txt":
		return true
	}
	return false
}

// Copy raw VM data only. Never carry ownership, delivery records, or leases
// from another run. Saved template memory is copied only after a suspended
// observation; callers publish only reusable, unused sources.
func cloneVZQualificationSource(source, destination string) error {
	disk, err := os.Open(filepath.Join(source, "disk.img"))
	if err != nil {
		return err
	}
	defer disk.Close()
	if err := unix.Flock(int(disk.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		return fmt.Errorf("source disk busy: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return err
	}
	files := []string{"config.json", "disk.img", "nvram.bin"}
	if _, err := os.Lstat(filepath.Join(source, "pulse.c2j.json")); err == nil {
		files = append(files, "pulse.c2j.json")
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Lstat(filepath.Join(source, "state.vzvmsave")); err == nil {
		files = append(files, "state.vzvmsave")
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, name := range files {
		from, to := filepath.Join(source, name), filepath.Join(destination, name)
		info, err := os.Lstat(from)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("source %s is not a regular file", name)
		}
		if out, err := exec.Command("/bin/cp", "-c", from, to).CombinedOutput(); err != nil {
			return fmt.Errorf("clone %s: %w: %s", name, err, out)
		}
		if err := os.Chmod(to, 0600); err != nil {
			return err
		}
	}
	return nil
}

func (q *qualification) cacheSources() {
	if q.cache == nil || q.cache.restored {
		return
	}
	images, err := q.r.Images()
	if err != nil {
		q.t.Fatal(err)
	}
	slots, err := q.r.Slots()
	if err != nil {
		q.t.Fatal(err)
	}
	names, err := vzQualificationSourceNames(images, slots)
	if err != nil {
		q.t.Fatal(err)
	}
	q.publishCacheSources(names)
}

func vzQualificationSourceNames(images []ImageStatus, slots []SlotStatus) ([]string, error) {
	var names []string
	for _, im := range images {
		if im.Phase != "ready" {
			return nil, fmt.Errorf("cache source not ready: %s", im.Name)
		}
		names = append(names, im.Name)
	}
	for _, s := range slots {
		if s.Phase != "ready" {
			return nil, fmt.Errorf("cache template not ready: %s", s.Template)
		}
		names = append(names, s.Template)
	}
	sort.Strings(names)
	names = slices.Compact(names) // Images also includes retained slot templates.
	return names, nil
}

func (q *qualification) publishCacheSources(names []string) {
	temp, err := os.MkdirTemp(filepath.Dir(q.cache.dir), ".publish-")
	if err != nil {
		q.t.Fatal(err)
	}
	defer os.RemoveAll(temp)
	for _, name := range names {
		v, err := q.r.p.Backend.Get(q.ctx, name)
		if err != nil || !v.Stopped() {
			q.t.Fatal("cache source must be stopped/suspended", name, err)
		}
		if err := cloneVZQualificationSource(filepath.Join(q.vzOptions.Home, "vms", name), filepath.Join(temp, name)); err != nil {
			q.t.Fatal(err)
		}
	}
	data, err := json.Marshal(names)
	if err != nil {
		q.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(temp, "sources.json"), data, 0600); err != nil {
		q.t.Fatal(err)
	}
	if err := os.Rename(temp, q.cache.dir); err != nil {
		q.t.Fatal(err)
	}
	q.cache.restored = true
	q.t.Logf("Cached %d prepared VZ bases/templates for subsequent runs", len(names))
}

func TestVZQualificationCacheSourceInventory(t *testing.T) {
	// Production Images includes slot templates, which Slots returns again.
	images := []ImageStatus{
		{Name: "pulse-slot-2", Kind: "slot", Phase: "ready"},
		{Name: "pulse-macos-base", Kind: "base", Phase: "ready"},
		{Name: "pulse-slot-1", Kind: "slot", Phase: "ready"},
		{Name: "pulse-linux-base", Kind: "base", Phase: "ready"},
	}
	slots := []SlotStatus{
		{Template: "pulse-slot-1", Phase: "ready"},
		{Template: "pulse-slot-2", Phase: "ready"},
	}
	names, err := vzQualificationSourceNames(images, slots)
	want := []string{"pulse-linux-base", "pulse-macos-base", "pulse-slot-1", "pulse-slot-2"}
	if err != nil || !slices.Equal(names, want) {
		t.Fatalf("sources = %v, %v; want %v", names, err, want)
	}
	images[0].Phase = "preparing"
	if _, err := vzQualificationSourceNames(images, slots); err == nil {
		t.Fatal("accepted an unprepared image")
	}
	images[0].Phase = "ready"
	slots[0].Phase = "preparing"
	if _, err := vzQualificationSourceNames(images, slots); err == nil {
		t.Fatal("accepted an unprepared template")
	}
}

func TestVZQualificationCacheRawSources(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("APFS clone")
	}
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config.json", "disk.img", "nvram.bin", "state.vzvmsave", "pulse.json", "client-key"} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	destination := filepath.Join(t.TempDir(), "clone")
	if err := cloneVZQualificationSource(source, destination); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config.json", "disk.img", "nvram.bin", "state.vzvmsave"} {
		data, err := os.ReadFile(filepath.Join(destination, name))
		if err != nil || string(data) != name {
			t.Fatal(name, err)
		}
	}
	for _, name := range []string{"pulse.json", "client-key"} {
		if _, err := os.Stat(filepath.Join(destination, name)); !os.IsNotExist(err) {
			t.Fatal("copied ownership/credentials", name)
		}
	}
	if err := os.WriteFile(filepath.Join(destination, "disk.img"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(source, "disk.img"))
	if err != nil || string(data) != "disk.img" {
		t.Fatal("clone modified source", err)
	}
	disk, err := os.Open(filepath.Join(source, "disk.img"))
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	if err := unix.Flock(int(disk.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := cloneVZQualificationSource(source, filepath.Join(t.TempDir(), "busy")); err == nil {
		t.Fatal("cloned active source")
	}
}

func TestVZQualificationCacheReuseAndInvalidation(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS cache identity")
	}
	root, output := t.TempDir(), t.TempDir()
	t.Setenv("PULSE_VZ_TEST_CACHE", root)
	t.Setenv("PULSE_TEST_OUTPUT_DIR", output)
	t.Setenv("PULSE_VZ_PREPARED_BASE", "")
	manifest := filepath.Join(output, "source-sha256.json")
	if err := os.WriteFile(manifest, []byte(`{"flake.nix":"one"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var entry string
	t.Run("cold", func(t *testing.T) {
		c := openVZQualificationCache(t, t.TempDir(), Config{})
		if c.restored {
			t.Fatal("unexpected cache hit")
		}
		entry = c.dir
		vm := filepath.Join(entry, "pulse-test-base")
		if err := os.MkdirAll(vm, 0700); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"config.json", "disk.img", "nvram.bin"} {
			if err := os.WriteFile(filepath.Join(vm, name), []byte(name), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(entry, "sources.json"), []byte(`["pulse-test-base"]`), 0600); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("warm", func(t *testing.T) {
		home := t.TempDir()
		c := openVZQualificationCache(t, home, Config{})
		if !c.restored || c.dir != entry {
			t.Fatal("did not reuse prepared source")
		}
		if _, err := os.Stat(filepath.Join(home, "vms", "pulse-test-base", "disk.img")); err != nil {
			t.Fatal(err)
		}
	})
	if err := os.WriteFile(manifest, []byte(`{"flake.nix":"one","docker_qualification_live_test.go":"new diagnostics"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Run("reporting change", func(t *testing.T) {
		c := openVZQualificationCache(t, t.TempDir(), Config{})
		if !c.restored || c.dir != entry {
			t.Fatal("test diagnostics invalidated prepared images")
		}
	})
	if err := os.WriteFile(manifest, []byte(`{"flake.nix":"one","internal/vz/bridge/bridge.go":"new host forwarder"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Run("host bridge change", func(t *testing.T) {
		c := openVZQualificationCache(t, t.TempDir(), Config{})
		if !c.restored || c.dir != entry {
			t.Fatal("host-only bridge invalidated guest sources")
		}
	})
	if err := os.WriteFile(manifest, []byte(`{"flake.nix":"two"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Run("changed inputs", func(t *testing.T) {
		c := openVZQualificationCache(t, t.TempDir(), Config{})
		if c.restored || c.dir == entry {
			t.Fatal("reused incompatible source")
		}
	})
}
