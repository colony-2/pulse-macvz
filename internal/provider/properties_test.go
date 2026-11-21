package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colony-2/pulse-macvz/internal/backend"
	api "github.com/colony-2/pulse-provider-api"
)

// Models metadata that only exists while its VM exists. Staged pre-create
// properties disappear on restart, just as in the direct Tart adapter.
type propertyFake struct {
	*fake
	props, staged          map[string]json.RawMessage
	createError, readError error
}

func (f *propertyFake) ReadVMProperties() (map[string]json.RawMessage, error) {
	if f.readError != nil {
		return nil, f.readError
	}
	out := map[string]json.RawMessage{}
	for name, raw := range f.props {
		if _, err := f.Get(context.Background(), name); err == nil {
			out[name] = bytes.Clone(raw)
		}
	}
	return out, nil
}
func (f *propertyFake) WriteVMProperties(props map[string]json.RawMessage) error {
	f.staged = props
	for name, raw := range props {
		if _, err := f.Get(context.Background(), name); err == nil {
			f.props[name] = bytes.Clone(raw)
		}
	}
	return nil
}
func (f *propertyFake) Create(ctx context.Context, spec backend.Spec) (backend.VM, error) {
	if f.createError != nil {
		return backend.VM{}, f.createError
	}
	v, err := f.fake.Create(ctx, spec)
	f.props[spec.Name] = bytes.Clone(f.staged[spec.Name])
	return v, err
}

type propertyDockerFake struct {
	*dockerFake
	metadata         *propertyFake
	endpointsRemoved int
}

func (f *propertyDockerFake) ReadVMProperties() (map[string]json.RawMessage, error) {
	return f.metadata.ReadVMProperties()
}
func (f *propertyDockerFake) WriteVMProperties(p map[string]json.RawMessage) error {
	return f.metadata.WriteVMProperties(p)
}
func (f *propertyDockerFake) Create(ctx context.Context, spec backend.Spec) (backend.VM, error) {
	return f.metadata.Create(ctx, spec)
}
func (f *propertyDockerFake) RemoveEndpoint(context.Context, string, string) error {
	f.endpointsRemoved++
	return nil
}

func TestVMPropertiesDockerRecoveryAndMissingMember(t *testing.T) {
	for _, missing := range []string{"", "primary", "engine"} {
		t.Run(missing, func(t *testing.T) {
			p, old := dockerFixture(t, false)
			b := &propertyDockerFake{dockerFake: old, metadata: &propertyFake{fake: old.fake, props: map[string]json.RawMessage{}}}
			p.Store.Close()
			var err error
			p.Store, err = OpenVMStore("", p.Config.Scope(), b)
			if err != nil {
				t.Fatal(err)
			}
			p.Backend = b
			pair := readyDocker(t, p)
			if missing == "" {
				staleEngine := bytes.Clone(b.metadata.props[pair.Engine.Name])
				old.loseDelivery = true
				out, err := p.Submit(t.Context(), []api.Launch{dockerLaunch()})
				if err != nil || out[0].Status != api.Unknown {
					t.Fatal(out, err)
				}
				// Model a crash between the primary and engine property writes.
				b.metadata.props[pair.Engine.Name] = staleEngine
			} else if missing == "primary" {
				delete(b.vms, pair.Primary.Name)
			} else {
				delete(b.vms, pair.Engine.Name)
			}
			creates := b.creates
			p.Store, err = OpenVMStore("", p.Config.Scope(), b)
			if err != nil {
				t.Fatal(err)
			}
			if missing == "" {
				if err = p.Reconcile(t.Context()); err != nil {
					t.Fatal(err)
				}
				if b.deliveries != 1 || b.creates != creates || p.Store.data.Pairs[pair.ID].Phase != "running" {
					t.Fatal("restart replayed or lost handoff")
				}
				out, err := p.Submit(t.Context(), []api.Launch{dockerLaunch()})
				if err != nil || out[0].Status != api.Rejected {
					t.Fatal("duplicate launch admitted", out, err)
				}
				if err = b.Stop(t.Context(), pair.Primary.Name, pair.PrimaryUID); err != nil {
					t.Fatal(err)
				}
			}
			p.Config.Slots[0].Count = 0
			for range 8 {
				if err = p.Reconcile(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if b.vms[pair.Primary.Name].UID != "" || b.vms[pair.Engine.Name].UID != "" {
				t.Fatal("pair survived teardown")
			}
			if b.endpointsRemoved == 0 {
				t.Fatal("endpoint not removed before metadata disappeared")
			}
			if err = p.Store.refresh(); err != nil {
				t.Fatal(err)
			}
			if len(p.Store.data.Pairs) != 0 || len(p.Store.data.Records) != 0 {
				t.Fatal("deleted allocation retained state")
			}
		})
	}
}

func TestVMPropertiesUnchangedReconciliationPreservesRevision(t *testing.T) {
	p, b := propertiesFixture(t)
	before, err := b.ReadVMProperties()
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := b.ReadVMProperties()
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range before {
		if !bytes.Equal(raw, after[name]) {
			t.Fatal("unchanged VM properties rewritten", name)
		}
	}
}

func TestVMPropertiesAdmissionRecoversReplacedPublishedSources(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%t", restart), func(t *testing.T) {
			p, b := propertiesFixture(t)
			out, err := p.Submit(t.Context(), []api.Launch{launch("old-job")})
			if err != nil || out[0].Status != api.Accepted {
				t.Fatal(out, err)
			}
			old := *p.Store.data.Records["old-job"]
			// `tart clone --overwrite` replaces canonical sources, dropping
			// their pulse.json files while the old job continues independently.
			for name, raw := range b.props {
				var props vmProperties
				if err := json.Unmarshal(raw, &props); err != nil {
					t.Fatal(err)
				}
				if props.Slot == nil && props.Image == nil {
					continue
				}
				delete(b.props, name)
				if b.baseVM != nil && b.baseVM.Name == name {
					b.baseVM.UID += "-updated"
				} else {
					v := b.vms[name]
					v.UID += "-updated"
					b.vms[name] = v
				}
			}
			if restart {
				p.Store, err = OpenVMStore("", p.Config.Scope(), b)
				if err != nil {
					t.Fatal(err)
				}
			}
			p.Setup = func(context.Context) ([]byte, error) { t.Fatal("admission rebuilt images"); return nil, nil }
			creates := b.creates
			// No intervening reconciliation: the next admission must use an
			// existing replacement, without rebuilding or disturbing old work.
			out, err = p.Submit(t.Context(), []api.Launch{launch("updated-job")})
			if err != nil || out[0].Status != api.Accepted {
				t.Fatal(out, err)
			}
			if b.creates != creates+1 {
				t.Fatal("admission created preparation VMs")
			}
			if got := p.Store.data.Records["old-job"]; got.UID != old.UID || got.Name != old.Name || got.SlotID != old.SlotID {
				t.Fatal("replacement changed running job identity")
			}
			for _, slot := range p.Store.data.Slots {
				if !strings.HasSuffix(slot.UID, "-updated") {
					t.Fatal("slot did not recover replacement identity")
				}
			}
			p.Store, err = OpenVMStore("", p.Config.Scope(), b)
			if err != nil || len(p.Store.data.Slots) != 2 || len(p.Store.data.Records) != 2 {
				t.Fatal("recovered source properties did not survive restart", err)
			}
		})
	}
}

func TestVMPropertiesAdmissionDoesNotRebuildMissingSources(t *testing.T) {
	p, b := propertiesFixture(t)
	b.props = map[string]json.RawMessage{}
	b.vms = map[string]backend.VM{}
	b.baseVM = nil
	creates := b.creates
	p.Setup = func(context.Context) ([]byte, error) { t.Fatal("admission invoked installer"); return nil, nil }
	out, err := p.Submit(t.Context(), []api.Launch{launch("no-source")})
	if err != nil || out[0].Status != api.NoCapacity || b.creates != creates {
		t.Fatal(out, err)
	}
}

func propertiesFixture(t *testing.T) (*Provider, *propertyFake) {
	t.Helper()
	p, f := slotsFixture(t, 2)
	warmSlots(t, p)
	b := &propertyFake{fake: f, props: map[string]json.RawMessage{}}
	encoded, err := p.Store.encodeProperties()
	if err != nil {
		t.Fatal(err)
	}
	if err = b.WriteVMProperties(encoded); err != nil {
		t.Fatal(err)
	}
	if err = p.Store.Close(); err != nil {
		t.Fatal(err)
	}
	// The new runtime needs none of the old private state directory.
	if err = os.RemoveAll(p.Config.StateDir); err != nil {
		t.Fatal(err)
	}
	p.Store, err = OpenVMStore("", p.Config.Scope(), b)
	if err != nil {
		t.Fatal(err)
	}
	p.Backend = b
	return p, b
}

func TestVMPropertiesRecoverJobsWithoutStateDirectory(t *testing.T) {
	p, b := propertiesFixture(t)
	out, err := p.Submit(t.Context(), []api.Launch{launch("a"), launch("b")})
	if err != nil || out[0].Status != api.Accepted || out[1].Status != api.Accepted {
		t.Fatal(out, err)
	}
	before, err := p.List(t.Context(), api.ListRequest{})
	if err != nil || len(before.Items) != 2 {
		t.Fatal(before, err)
	}
	b.staged = nil // Model process restart, retaining only VM properties.
	p.Store, err = OpenVMStore("", p.Config.Scope(), b)
	if err != nil {
		t.Fatal(err)
	}
	after, err := p.List(t.Context(), api.ListRequest{})
	if err != nil || len(after.Items) != 2 || after.Items[0].LaunchID != before.Items[0].LaunchID || after.Items[0].Job != before.Items[0].Job {
		t.Fatal(after, err)
	}
	if _, err := os.Stat(p.Config.StateDir); !os.IsNotExist(err) {
		t.Fatal("recreated state directory", err)
	}
	for _, r := range p.Store.data.Records {
		v := b.vms[r.Name]
		v.Status, v.PowerState = "failed", "suspended"
		v.Conditions = []backend.Condition{{Type: "running", State: "false"}}
		b.vms[r.Name] = v
	}
	// Submit observes and cleans terminal clones even before the next Run tick.
	out, err = p.Submit(t.Context(), []api.Launch{launch("replacement-a"), launch("replacement-b")})
	if err != nil || out[0].Status != api.Accepted || out[1].Status != api.Accepted {
		t.Fatal(out, err)
	}
	if b.deletes != 2 {
		t.Fatal("failed suspended clones not removed", b.deletes)
	}
	p.Store, err = OpenVMStore("", p.Config.Scope(), b)
	if err != nil {
		t.Fatal(err)
	}
	if p.Store.data.Records["a"] != nil || p.Store.data.Records["b"] != nil {
		t.Fatal("deleted jobs retained as durable history")
	}
}

func TestVMPropertiesMissingUnknownLaunchDoesNotReserveSlot(t *testing.T) {
	p, b := propertiesFixture(t)
	b.createError = errors.New("clone command failed before a VM existed")
	out, err := p.Submit(t.Context(), []api.Launch{launch("failed")})
	if err != nil || out[0].Status != api.Unknown {
		t.Fatal(out, err)
	}
	b.createError = nil
	out, err = p.Submit(t.Context(), []api.Launch{launch("one"), launch("two")})
	if err != nil || out[0].Status != api.Accepted || out[1].Status != api.Accepted {
		t.Fatal("absent failed dispatch consumed capacity", out, err)
	}
	delete(b.vms, p.Store.data.Records["one"].Name)
	out, err = p.Submit(t.Context(), []api.Launch{launch("three")})
	if err != nil || out[0].Status != api.Accepted {
		t.Fatal("external deletion retained reservation", out, err)
	}
}

func TestVMPropertiesReadFailureDoesNotAdmitOrClearOccupancy(t *testing.T) {
	p, b := propertiesFixture(t)
	_, err := p.Submit(t.Context(), []api.Launch{launch("one")})
	if err != nil {
		t.Fatal(err)
	}
	creates := b.creates
	b.readError = errors.New("properties unreadable")
	if _, err = p.Submit(t.Context(), []api.Launch{launch("two")}); !errors.Is(err, b.readError) {
		t.Fatal(err)
	}
	if b.creates != creates || p.Store.data.Records["one"] == nil {
		t.Fatal("failed observation mutated allocation")
	}
}

func TestVMPropertiesMigrationOnlyImportsExistingResources(t *testing.T) {
	p, f := slotsFixture(t, 2)
	warmSlots(t, p)
	_, err := p.Submit(t.Context(), []api.Launch{launch("existing"), launch("gone")})
	if err != nil {
		t.Fatal(err)
	}
	delete(f.vms, p.Store.data.Records["gone"].Name)
	legacy, err := os.ReadFile(filepath.Join(p.Config.StateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	b := &propertyFake{fake: f, props: map[string]json.RawMessage{}}
	for name := range f.vms {
		b.props[name] = nil
	}
	b.props[f.baseVM.Name] = nil
	p.Store.Close()
	p.Store, err = OpenVMStore(p.Config.StateDir, p.Config.Scope(), b)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Store.data.Records) != 1 || p.Store.data.Records["existing"] == nil || len(p.Store.data.Slots) != 2 {
		t.Fatal("migration changed ownership", p.Store.data)
	}
	after, _ := os.ReadFile(filepath.Join(p.Config.StateDir, "state.json"))
	if !bytes.Equal(after, legacy) {
		t.Fatal("migration changed legacy journal")
	}
	if err = os.RemoveAll(p.Config.StateDir); err != nil {
		t.Fatal(err)
	}
	p.Store, err = OpenVMStore("", p.Config.Scope(), b)
	if err != nil || len(p.Store.data.Records) != 1 {
		t.Fatal("migration did not persist attached properties", err)
	}
}
