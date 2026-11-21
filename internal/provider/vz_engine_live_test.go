//go:build darwin && arm64

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse-macvz/internal/backend"
	"github.com/colony-2/pulse-macvz/internal/config"
	"github.com/colony-2/pulse-macvz/internal/vz"
	"golang.org/x/sys/unix"
)

// Isolate companion preparation from macOS snapshots and host launchd. The input
// is a stopped raw Linux base; only APFS clones in a private home are modified.
func TestLiveVZDockerEngine(t *testing.T) {
	base, runner, output := os.Getenv("PULSE_VZ_LINUX_BASE"), os.Getenv("PULSE_VZ_RUNNER"), os.Getenv("PULSE_VZ_RESULTS")
	if base == "" || runner == "" || output == "" {
		t.Skip("set PULSE_VZ_LINUX_BASE, PULSE_VZ_RUNNER, PULSE_VZ_RESULTS")
	}
	home, err := os.MkdirTemp(output, "vz-engine-")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("artifacts:", home)
	source := filepath.Join(home, "vms", "source")
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	disk, err := os.Open(filepath.Join(base, "disk.img"))
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	if err := unix.Flock(int(disk.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(base, "state.vzvmsave")); !os.IsNotExist(err) {
		t.Fatal("base must be cold")
	}
	for _, file := range []string{"config.json", "disk.img", "nvram.bin"} {
		info, err := os.Lstat(filepath.Join(base, file))
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("base %s must be a regular file", file)
		}
		if data, err := exec.Command("/bin/cp", "-c", filepath.Join(base, file), filepath.Join(source, file)).CombinedOutput(); err != nil {
			t.Fatalf("clone: %v %s", err, data)
		}
	}
	raw, err := os.ReadFile(filepath.Join(source, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sourceConfig map[string]any
	if err := json.Unmarshal(raw, &sourceConfig); err != nil {
		t.Fatal(err)
	}
	if sourceConfig["os"] != "linux" || sourceConfig["arch"] != "arm64" {
		t.Fatal("requires cold arm64 Linux base")
	}
	var resources struct {
		CPU    int64 `json:"cpuCount"`
		Memory int64 `json:"memorySize"`
	}
	if err := json.Unmarshal(raw, &resources); err != nil {
		t.Fatal(err)
	}
	if resources.CPU <= 0 || resources.Memory < 1<<20 {
		t.Fatal("base must specify positive CPU and memory resources")
	}
	sourceConfig["vzVersion"] = 1
	raw, _ = json.Marshal(sourceConfig)
	if err := os.WriteFile(filepath.Join(source, "config.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	b, err := vz.New(config.VZ{Home: home, Runner: runner, SSHTimeout: 5 * time.Second}, config.Config{OwnerID: "engine-live", BootstrapTimeout: 6 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	spec := backend.Spec{Name: "source", OS: "linux", Arch: "arm64", CPU: resources.CPU, Memory: resources.Memory / (1 << 20), Username: "admin", Password: "admin"}
	if _, err := b.AdoptSource(ctx, spec, nil); err != nil {
		t.Fatal(err)
	}
	p := &Provider{Assets: os.DirFS("../.."), Config: config.Settings{Config: config.Config{BootstrapTimeout: 5 * time.Minute}}}
	profile := config.EngineProfile{EngineVersion: "29.1.3", DiskGiB: 8}
	owned := map[string]string{}
	stopVM := func(name, uid string) {
		t.Helper()
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if err := b.Stop(cleanup, name, uid); err != nil {
			t.Error(err)
			return
		}
		for cleanup.Err() == nil {
			got, err := b.Get(cleanup, name)
			if err != nil || got.Stopped() {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Error("stop timeout", name)
	}
	defer func() {
		for name, uid := range owned {
			stopVM(name, uid)
		}
	}()
	waitReady := func(vm backend.VM, marker string, stopped bool) {
		t.Helper()
		start := time.Now()
		for ctx.Err() == nil {
			events, err := b.Events(ctx, vm.Name)
			if err != nil {
				t.Fatal(err)
			}
			var output strings.Builder
			for _, e := range events {
				output.WriteString(e.Payload)
			}
			_ = os.WriteFile(filepath.Join(home, vm.Name+".log"), []byte(output.String()), 0600)
			got, err := b.Get(ctx, vm.Name)
			if err != nil {
				t.Fatal(err)
			}
			ready := strings.Contains(output.String(), marker)
			if ready && (!stopped || got.Stopped()) {
				t.Logf("%s ready after %s", vm.Name, time.Since(start).Round(time.Millisecond))
				return
			}
			if got.Stopped() && !ready {
				t.Fatalf("%s stopped before readiness: %s", vm.Name, output.String())
			}
			time.Sleep(time.Second)
		}
		t.Fatal("readiness timeout", vm.Name)
	}
	script, err := p.engineBaseStartup(profile)
	if err != nil {
		t.Fatal(err)
	}
	spec.Name, spec.Image, spec.CPU, spec.Memory = "prepared", "source", 4, 8192
	spec.StartupScript.Content = script
	prepared, err := b.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	owned[prepared.Name] = prepared.UID
	waitReady(prepared, "PULSE_BASE_READY", true)
	if err := b.RetainSource(ctx, prepared.Name, prepared.UID); err != nil {
		t.Fatal(err)
	}
	engineIDs := map[string]bool{}
	for round := 1; round <= 2; round++ {
		host, _, err := keyPair()
		if err != nil {
			t.Fatal(err)
		}
		_, client, err := keyPair()
		if err != nil {
			t.Fatal(err)
		}
		script, err := p.engineStartup(profile, host, client)
		if err != nil {
			t.Fatal(err)
		}
		spec.Name, spec.Image = fmt.Sprintf("engine-%d", round), prepared.Name
		spec.StartupScript.Content = script
		vm, err := b.Create(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		owned[vm.Name] = vm.UID
		waitReady(vm, "PULSE_DOCKER_ENGINE_READY ", false)
		events, err := b.Events(ctx, vm.Name)
		if err != nil {
			t.Fatal(err)
		}
		engineID := ""
		for _, event := range events {
			for _, line := range strings.Split(event.Payload, "\n") {
				if strings.HasPrefix(line, "PULSE_DOCKER_ENGINE_READY ") {
					engineID = strings.TrimPrefix(line, "PULSE_DOCKER_ENGINE_READY ")
				}
			}
		}
		if engineID == "" || engineIDs[engineID] {
			t.Fatal("missing or reused Docker engine identity", engineID)
		}
		engineIDs[engineID] = true
		run := func(name, script string) {
			t.Helper()
			command := "sudo -n /bin/bash -s <<'ENGINE_CHECK' 2>&1\nset -eu\n" + script + "\nENGINE_CHECK\nstatus=$?\nprintf '\\nENGINE_CHECK_EXIT=%s\\n' \"$status\"\n"
			out, err := b.Exec(ctx, vm.Name, vm.UID, []byte(command))
			_ = os.WriteFile(filepath.Join(home, vm.Name+"-"+name+".log"), out, 0600)
			t.Log(name+":", string(out))
			if err != nil || !strings.HasSuffix(string(out), "ENGINE_CHECK_EXIT=0\n") {
				t.Fatalf("%s failed: %v", name, err)
			}
		}
		run("base-reused", "test -x /usr/bin/dockerd\ntest -f /var/lib/pulse-docker-image/prepare.sh\n! systemctl cat pulse-docker-image.service >/dev/null 2>&1")
		if os.Getenv("PULSE_VZ_ENGINE_PRESSURE") == "1" && round == 1 {
			run("daemon-restart", "systemctl kill --kill-whom=main --signal=SIGKILL docker.service\nsystemctl restart docker.service\ndocker info >/dev/null")
			pressure, err := os.ReadFile("../../scripts/docker-qualification/pressure.sh")
			if err != nil {
				t.Fatal(err)
			}
			verify, err := os.ReadFile("../../scripts/docker-qualification/verify-oom.py")
			if err != nil {
				t.Fatal(err)
			}
			run("pressure", string(pressure)+"\npython3 - \"$oom_id\" <<'VERIFY_OOM'\n"+string(verify)+"\nVERIFY_OOM")
			disk, err := os.ReadFile("../../scripts/docker-qualification/disk-pressure.py")
			if err != nil {
				t.Fatal(err)
			}
			run("disk-pressure", "python3 - <<'ENGINE_DISK'\n"+string(disk)+"\nENGINE_DISK")
			run("post-pressure", "docker run --rm python:3.13-alpine python -c 'print(\"ENGINE_PRESSURE_RECOVERED\")'")
		}
		stopVM(vm.Name, vm.UID)
		delete(owned, vm.Name)
	}
	t.Log("VZ_ENGINE_PASS: one prepared base, two fresh companions")
}
