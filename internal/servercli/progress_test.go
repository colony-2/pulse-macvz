package servercli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	tart "github.com/colony-2/pulse-tart"
	"github.com/colony-2/pulse-tart/internal/backend"
)

type progressRuntime struct {
	maintenanceFake
	images   []tart.ImageStatus
	slots    []tart.SlotStatus
	pairs    []tart.DockerStatus
	progress tart.PreparationProgress
}

func TestGuestObservationProgressShowsActivityAndReboots(t *testing.T) {
	got := vmStage("preparing", tart.VMProgress{Stage: "base_preparing", LogBytes: 2048, Reboots: 2, ObservedAt: time.Now()})
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
	f := &progressRuntime{images: []tart.ImageStatus{{Name: "base15", Phase: "preparing"}},
		progress: tart.PreparationProgress{VMs: []tart.VMProgress{{Name: "base15", Stage: "base_failed", Cause: "guest_reboot_loop", Reboots: 3}}}}
	if _, err := capacityReady(c, f); err == nil || !strings.Contains(err.Error(), "guest_reboot_loop") {
		t.Fatal(err)
	}
	f.images[0].Obsolete = true
	if _, err := capacityReady(c, f); err != nil {
		t.Fatal("obsolete observation blocked setup", err)
	}
}

func (f *progressRuntime) Images() ([]tart.ImageStatus, error)             { return f.images, nil }
func (f *progressRuntime) Slots() ([]tart.SlotStatus, error)               { return f.slots, nil }
func (f *progressRuntime) DockerAllocations() ([]tart.DockerStatus, error) { return f.pairs, nil }
func (f *progressRuntime) PreparationProgress() (tart.PreparationProgress, error) {
	return f.progress, nil
}

func TestSetupWaitsForEveryMacVersionTemplate(t *testing.T) {
	c, err := (capacityOptions{macOS: 2, linux: 4}).config("test", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := &progressRuntime{images: []tart.ImageStatus{{Phase: "ready"}}, slots: []tart.SlotStatus{
		{ID: "standard-0001", Phase: "ready"}, {ID: "standard-0002", Phase: "ready"},
	}}
	for _, id := range []string{"linux-0001", "linux-0002", "linux-0003", "linux-0004"} {
		f.pairs = append(f.pairs, tart.DockerStatus{Slot: id, Phase: "ready"})
	}
	if ready, err := capacityReady(c, f); err != nil || ready {
		t.Fatal("one OS version incorrectly completed setup", ready, err)
	}
	for _, id := range []string{"mac-macos-15-arm64-0001", "mac-macos-15-arm64-0002"} {
		f.slots = append(f.slots, tart.SlotStatus{ID: id, Phase: "ready"})
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
	f := &progressRuntime{images: []tart.ImageStatus{{Base: c.Bases[1].Name, Name: "mac15", Phase: "preparing", Kind: "base"}},
		progress: tart.PreparationProgress{
			Sources: []tart.SourceProgress{{Source: c.Bases[0].Image, Started: now.Add(-10 * time.Second), Updated: now,
				TransferProgress: backend.TransferProgress{Stage: "downloading", Current: 10 << 20, Total: 100 << 20}}},
			VMs: []tart.VMProgress{{Name: "mac15", Stage: "guest_exec"}},
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
	p := tart.SourceProgress{Started: now.Add(-10 * time.Second), Updated: now,
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
