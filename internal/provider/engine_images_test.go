package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/colony-2/pulse-macvz/internal/backend"
)

type engineImageTracker struct {
	*dockerFake
	created []backend.Spec
}

func (f *engineImageTracker) Create(ctx context.Context, s backend.Spec) (backend.VM, error) {
	f.created = append(f.created, s)
	return f.dockerFake.Create(ctx, s)
}

func TestDockerReplacementReusesPreparedEngineBase(t *testing.T) {
	p, f := dockerFixture(t, false)
	tracked := &engineImageTracker{dockerFake: f}
	p.Backend = tracked
	first := readyDocker(t, p)
	baseName := p.engineBaseName(p.Config.Engine("engine"))
	image := p.Store.data.Images[baseName]
	if image == nil || image.Phase != "ready" || first.Engine.Image != image.LocalName {
		t.Fatal("companion did not clone prepared base")
	}
	first.Phase = "reclaiming"
	for i := 0; i < 30; i++ {
		requestTestDockerPool(t, p)
		if err := p.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		for _, pair := range p.Store.data.Pairs {
			if pair.ID != first.ID && pair.Phase == "ready" {
				if pair.Engine.Image != image.LocalName || pair.EngineUID == first.EngineUID {
					t.Fatal("replacement did not reuse software with fresh ownership")
				}
				builds := 0
				for _, spec := range tracked.created {
					if spec.Name == baseName {
						builds++
					}
				}
				if builds != 1 {
					t.Fatalf("engine base rebuilt %d times", builds)
				}
				return
			}
		}
	}
	t.Fatal("replacement never ready")
}

func TestLinuxBaseInstallsSelectedEngineBeforeAllocation(t *testing.T) {
	p, f := dockerFixture(t, true)
	tracked := &engineImageTracker{dockerFake: f}
	p.Backend = tracked
	readyDocker(t, p)
	found := false
	for _, spec := range tracked.created {
		if strings.HasSuffix(spec.Name, "-base") {
			found = true
			if !strings.Contains(spec.StartupScript.Content, "'24.04' '29.1.3'\n") {
				t.Fatal("Linux base omitted engine version", spec.StartupScript.Content)
			}
		}
	}
	if !found {
		t.Fatal("no Linux base prepared")
	}
	old := p.baseImageName(p.Config.Bases[0].Name)
	p.Config.Docker.Profiles[0].EngineVersion = "29.2.0"
	if p.baseImageName(p.Config.Bases[0].Name) == old {
		t.Fatal("different engine version reused old Linux source")
	}
}
