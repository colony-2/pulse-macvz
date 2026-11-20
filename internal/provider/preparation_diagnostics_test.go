package provider

import (
	"errors"
	api "github.com/colony-2/pulse-provider-api"
	"strings"
	"testing"
)

func TestDiagnosticDockerSourcePreparationCause(t *testing.T) {
	for _, stage := range []string{"base", "template"} {
		t.Run(stage, func(t *testing.T) {
			p, _ := dockerFixture(t, false)
			if stage == "template" {
				for range 8 {
					if e := p.reconcileImages(t.Context()); e != nil {
						t.Fatal(e)
					}
					im := p.Store.data.Images[p.baseImageName(p.Config.Bases[0].Name)]
					if im != nil && im.Phase == "ready" {
						break
					}
				}
				im := p.Store.data.Images[p.baseImageName(p.Config.Bases[0].Name)]
				if im == nil || im.Phase != "ready" {
					t.Fatal("base setup did not finish")
				}
			}
			d := diagnosticWrap(p)
			d.faults["Create"] = errors.New(diagnosticRaw)
			if e := p.Reconcile(t.Context()); e != nil {
				t.Fatal("preparation must remain non-stopping", e)
			}
			t.Logf("retained preparation entries=%d pairs=%d slots=%d", len(p.preparationCauses), len(p.Store.data.Pairs), len(p.Store.data.Slots))
			d.calls = nil
			diagnosticSubmit(t, p, dockerLaunch(), api.NoCapacity, "earlier preparation observation", diagnosticRaw)
			diagnosticCalls(t, d, "List")
		})
	}
}

func TestDiagnosticDockerPrimaryPreparationCauseAfterEngineGet(t *testing.T) {
	p, _ := dockerFixture(t, false)
	var pair *PairRecord
	for range 30 {
		if e := p.Reconcile(t.Context()); e != nil {
			t.Fatal(e)
		}
		for _, candidate := range p.Store.data.Pairs {
			if candidate.Phase == "engine-dispatch" {
				pair = candidate
			}
		}
		if pair != nil {
			break
		}
	}
	if pair == nil {
		t.Fatal("setup did not reach engine-dispatch")
	}
	d := diagnosticWrap(p)
	d.faults["Create"] = errors.New(diagnosticRaw)
	if e := p.Reconcile(t.Context()); e != nil {
		t.Fatal(e)
	}
	if pair.Phase != "primary-dispatch" {
		t.Fatalf("did not reach create: %s", pair.Phase)
	}
	if old, ok := p.preparationCauses["pair:"+pair.ID]; !ok || !strings.Contains(old.cause.Error(), diagnosticRaw) {
		t.Fatal("did not retain initial create failure")
	}
	d.calls = nil
	if e := p.Reconcile(t.Context()); e != nil {
		t.Fatal(e)
	}
	diagnosticCalls(t, d, "List", "Get:base", "Get:base", "Get:template", "Get:primary", "Get:engine")
	t.Logf("after missing primary and successful engine Get: phase=%s retained=%d", pair.Phase, len(p.preparationCauses))
	diagnosticSubmit(t, p, dockerLaunch(), api.NoCapacity, "earlier preparation observation", diagnosticRaw)
}

func TestDiagnosticDockerPreparationRecoveryIsRelevant(t *testing.T) {
	for _, primary := range []bool{true, false} {
		member := "engine"
		if primary {
			member = "primary"
		}
		t.Run(member, func(t *testing.T) {
			p, _ := dockerFixture(t, false)
			pair := readyDocker(t, p)
			d := diagnosticWrap(p)
			key := "pair:" + pair.ID
			p.pairPreparation(pair, primary, errors.New("old cause"))
			p.pairPreparation(pair, primary, errors.New(diagnosticRaw))
			if _, exists, err := p.pairVM(t.Context(), pair, !primary); err != nil || !exists {
				t.Fatal(exists, err)
			}
			if old, ok := p.preparationCauses[key]; !ok || old.cause.Error() != diagnosticRaw {
				t.Fatal("unrelated member cleared latest cause", old, ok)
			}
			if _, exists, err := p.pairVM(t.Context(), pair, primary); err != nil || !exists {
				t.Fatal(exists, err)
			}
			if _, ok := p.preparationCauses[key]; ok {
				t.Fatal("recovered member retained cause")
			}
			// A successful identity read is not successful configuration/readiness.
			p.preparation(key, errors.New(diagnosticRaw))
			if _, _, err := p.pairVM(t.Context(), pair, primary); err != nil {
				t.Fatal(err)
			}
			if _, ok := p.preparationCauses[key]; !ok {
				t.Fatal("identity read cleared preparation failure")
			}
			other := "primary"
			if primary {
				other = "engine"
			}
			diagnosticCalls(t, d, "Get:"+other, "Get:"+member, "Get:"+member)
		})
	}
}

func TestDiagnosticDockerPreparationSkippedGatesStayUnknown(t *testing.T) {
	p, _ := dockerFixture(t, false)
	d := diagnosticWrap(p)
	d.faults["Create"] = errors.New(diagnosticRaw)
	if err := p.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	d.calls = nil
	l := dockerLaunch()
	l.CPUMillis = 5000
	r := diagnosticSubmit(t, p, l, api.Unsupported)
	if strings.Contains(r.Reason, diagnosticRaw) || strings.Contains(r.Reason, "no prepared pair") {
		t.Fatal("request-fit short circuit reached preparation", r)
	}
	diagnosticCalls(t, d)
}
