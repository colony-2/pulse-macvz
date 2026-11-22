package servercli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	macvz "github.com/colony-2/pulse-macvz"
	"github.com/colony-2/pulse-macvz/internal/backend"
)

type progressRuntime struct {
	maintenanceFake
	images   []macvz.ImageStatus
	slots    []macvz.SlotStatus
	pairs    []macvz.DockerStatus
	progress macvz.PreparationProgress
}

func TestGuestObservationProgressShowsActivityAndReboots(t *testing.T) {
	got := vmStage("preparing", macvz.VMProgress{Stage: "base_preparing", LogBytes: 2048, Reboots: 2, ObservedAt: time.Now()})
	for _, want := range []string{"preparation in progress", "2.0 KiB", "observed", "restarts observed: 2", "ETA unavailable"} {
		if !strings.Contains(got, want) {
			t.Fatal(got)
		}
	}
}

func TestSetupReportsObservedFailureOnlyForRequiredImages(t *testing.T) {
	c, err := (capacityOptions{macOS: 1, linux: 1}).config("test", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := &progressRuntime{images: []macvz.ImageStatus{{Name: "base15", Phase: "preparing"}},
		progress: macvz.PreparationProgress{VMs: []macvz.VMProgress{{Name: "base15", Stage: "base_failed", Cause: "guest_reboot_loop", Reboots: 3}}}}
	if _, err := capacityReady(c, f); err == nil || !strings.Contains(err.Error(), "guest_reboot_loop") {
		t.Fatal(err)
	}
	f.images[0].Obsolete = true
	if _, err := capacityReady(c, f); err != nil {
		t.Fatal("obsolete observation blocked setup", err)
	}
}

func TestSetupReportsFailedRequiredTemplateButNotBusyOrObsoleteTemplates(t *testing.T) {
	c, err := (capacityOptions{macOS: 1, macBase: "macos-26-arm64"}).config("test", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := &progressRuntime{
		images:   []macvz.ImageStatus{{Name: "base", Phase: "ready"}},
		slots:    []macvz.SlotStatus{{ID: "standard-0001", Template: "template", Phase: "preparing"}},
		progress: macvz.PreparationProgress{VMs: []macvz.VMProgress{{Name: "template", Stage: "base_failed", Cause: "preparation_timeout"}}},
	}
	if _, err := capacityReady(c, f); err == nil || !strings.Contains(err.Error(), "template") {
		t.Fatal("template failure hidden", err)
	}
	f.slots[0].Busy = true
	if _, err := capacityReady(c, f); err != nil {
		t.Fatal("accepted work treated as failed preparation", err)
	}
	f.slots[0].Busy = false
	f.slots[0].ID = "obsolete-0001"
	if _, err := capacityReady(c, f); err != nil {
		t.Fatal("obsolete template blocked setup", err)
	}
}

func (f *progressRuntime) Images() ([]macvz.ImageStatus, error)             { return f.images, nil }
func (f *progressRuntime) Slots() ([]macvz.SlotStatus, error)               { return f.slots, nil }
func (f *progressRuntime) DockerAllocations() ([]macvz.DockerStatus, error) { return f.pairs, nil }
func (f *progressRuntime) PreparationProgress() (macvz.PreparationProgress, error) {
	return f.progress, nil
}

func TestSetupWaitsForEveryMacVersionTemplate(t *testing.T) {
	c, err := (capacityOptions{macOS: 2, linux: 4}).config("test", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := &progressRuntime{images: []macvz.ImageStatus{{Phase: "ready"}}, slots: []macvz.SlotStatus{
		{ID: "standard-0001", Phase: "ready"}, {ID: "standard-0002", Phase: "ready"},
	}}
	for _, id := range []string{"linux-0001", "linux-0002", "linux-0003", "linux-0004"} {
		f.pairs = append(f.pairs, macvz.DockerStatus{Slot: id, Phase: "ready"})
	}
	if ready, err := capacityReady(c, f); err != nil || ready {
		t.Fatal("one OS version incorrectly completed setup", ready, err)
	}
	for _, id := range []string{"mac-macos-15-arm64-0001", "mac-macos-15-arm64-0002"} {
		f.slots = append(f.slots, macvz.SlotStatus{ID: id, Phase: "ready"})
	}
	if ready, err := capacityReady(c, f); err != nil || !ready {
		t.Fatal("all versions not recognized ready", ready, err)
	}
	f.pairs = f.pairs[:3]
	if ready, _ := capacityReady(c, f); ready {
		t.Fatal("extra suspended templates concealed missing Linux capacity")
	}
}

func TestPreparationProgressShowsSlotsDownloadAndGuestStages(t *testing.T) {
	c, err := (capacityOptions{macOS: 1, linux: 1}).config("test", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	f := &progressRuntime{images: []macvz.ImageStatus{{Base: c.Bases[1].Name, Name: "mac15", Phase: "preparing", Kind: "base"}},
		progress: macvz.PreparationProgress{
			Sources: []macvz.SourceProgress{{Source: c.Bases[0].Image, Started: now.Add(-10 * time.Second), Updated: now,
				TransferProgress: backend.TransferProgress{Stage: "downloading", Current: 10 << 20, Total: 100 << 20}}},
			VMs: []macvz.VMProgress{{Name: "mac15", Stage: "guest_exec"}},
		}}
	var output bytes.Buffer
	r := &preparationReporter{}
	if err := r.print(&output, c, f, true); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"standard-0001", "mac-macos-15-arm64-0001", "linux-0001", "10.0 MiB / 100.0 MiB", "10.0%", "/s avg", "ETA ~", "running guest preparation command"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing %q in %s", want, output.String())
		}
	}
	before := output.Len()
	if err := r.print(&output, c, f, false); err != nil {
		t.Fatal(err)
	}
	if output.Len() != before {
		t.Fatal("unchanged progress was not throttled")
	}
	f.progress.VMs[0].Stage = "delivered"
	if err := r.print(&output, c, f, false); err != nil {
		t.Fatal(err)
	}
	if output.Len() == before {
		t.Fatal("stage transition did not print immediately")
	}
}

func TestDownloadEstimatesHandleUnknownTotalAndStalls(t *testing.T) {
	now := time.Now()
	p := macvz.SourceProgress{Started: now.Add(-10 * time.Second), Updated: now,
		TransferProgress: backend.TransferProgress{Stage: "downloading", Current: 10 << 20, Total: 100 << 20}}
	if got := transferDetail(p, now); !strings.Contains(got, "ETA ~1m30s") {
		t.Fatal(got)
	}
	p.Total = 0
	if got := transferDetail(p, now); !strings.Contains(got, "unknown total") || !strings.Contains(got, "ETA unavailable") {
		t.Fatal(got)
	}
	p.Updated = now.Add(-20 * time.Second)
	if got := transferDetail(p, now); !strings.Contains(got, "no new transfer data for 20s") || strings.Contains(got, "ETA ~") {
		t.Fatal(got)
	}
	p.Stage = "cached"
	if got := transferDetail(p, now); !strings.Contains(got, "cache hit") {
		t.Fatal(got)
	}
}
