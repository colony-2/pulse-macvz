package provider

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	api "github.com/colony-2/pulse-provider-api"
)

// VMPropertyBackend keeps properties with their native VM. Missing VMs have no
// properties. Writes for a not-yet-created VM are staged in memory and attached
// by Create before dispatch; they never become independent durable reservations.
type VMPropertyBackend interface {
	ReadVMProperties() (map[string]json.RawMessage, error)
	WriteVMProperties(map[string]json.RawMessage) error
}

// vmProperties describes one resource, not a pool-wide inventory. Pair setup
// properties travel with both members so either surviving member can be cleaned
// up after a crash. Revision resolves a crash between the two member writes.
type vmProperties struct {
	Version  int          `json:"version"`
	Scope    string       `json:"scope"`
	Key      []byte       `json:"key"`
	Revision int64        `json:"revision"`
	Record   *Record      `json:"job,omitempty"`
	Slot     *SlotRecord  `json:"slot,omitempty"`
	Image    *ImageRecord `json:"image,omitempty"`
	Pair     *PairRecord  `json:"pair,omitempty"`
}

func emptyState(scope string, key []byte) state {
	return state{Version: 3, Scope: scope, Key: key, Records: map[string]*Record{}, Slots: map[string]*SlotRecord{}, Images: map[string]*ImageRecord{}, Pairs: map[string]*PairRecord{}}
}

// OpenVMStore reconstructs an in-memory index. StateDir is only a legacy import
// source; no state.json or terminal launch history is written in this mode.
func OpenVMStore(legacyDir, scope string, b VMPropertyBackend) (*Store, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	s := &Store{properties: b, data: emptyState(scope, key)}
	props, err := b.ReadVMProperties()
	if err != nil {
		return nil, err
	}
	if err = s.loadProperties(props); err != nil {
		return nil, err
	}
	// Only import records for existing, owned VMs without attached properties.
	// A leftover legacy journal cannot resurrect a deleted VM or terminal job.
	missing := false
	for _, raw := range props {
		missing = missing || len(raw) == 0
	}
	if missing && legacyDir != "" {
		path := filepath.Join(legacyDir, "state.json")
		info, e := os.Lstat(path)
		if e == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0) {
			return nil, stateError("legacy journal must be private and regular", nil)
		}
		if e != nil && !os.IsNotExist(e) {
			return nil, e
		}
		if e == nil {
			data, e := os.ReadFile(path)
			if e != nil {
				return nil, e
			}
			var old state
			if e = json.Unmarshal(data, &old); e != nil {
				return nil, stateError("decode legacy journal", e)
			}
			if old.Version != 3 || old.Scope != scope || len(old.Key) != 32 {
				return nil, stateError("legacy journal scope/version mismatch", nil)
			}
			if len(s.data.Records)+len(s.data.Slots)+len(s.data.Images)+len(s.data.Pairs) > 0 && !bytes.Equal(old.Key, s.data.Key) {
				return nil, stateError("legacy properties key mismatch", nil)
			}
			// Convert through the same per-VM representation used for new writes.
			importer := &Store{data: old}
			encoded, e := importer.encodeProperties()
			if e != nil {
				return nil, e
			}
			for name, raw := range props {
				if len(raw) == 0 {
					props[name] = encoded[name]
				}
			}
			if e = s.loadProperties(props); e != nil {
				return nil, e
			}
		}
	}
	if err = s.save(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) refresh() error {
	if s.properties == nil {
		return nil
	}
	props, err := s.properties.ReadVMProperties()
	if err != nil {
		return stateError("read VM properties", err)
	}
	// Unstarted warm-pool planning may span ticks, but is intentionally lost on
	// restart. It does not outlive the process until a member VM actually exists.
	planned := s.data.Pairs
	if err = s.loadProperties(props); err != nil {
		return err
	}
	for id, p := range planned {
		if p.Phase == "reserved" && !p.PrimaryDispatched && !p.EngineDispatched && s.data.Pairs[id] == nil {
			s.data.Pairs[id] = p
		}
	}
	return nil
}

func (s *Store) loadProperties(all map[string]json.RawMessage) error {
	next := emptyState(s.data.Scope, s.data.Key)
	var key []byte
	revisions := map[string]int64{}
	for name, raw := range all {
		if len(raw) == 0 {
			continue
		}
		var p vmProperties
		if err := json.Unmarshal(raw, &p); err != nil {
			return stateError("decode VM properties", err)
		}
		if p.Version != 1 || p.Scope != next.Scope || len(p.Key) != 32 {
			return stateError("invalid VM properties scope/version/key", nil)
		}
		if key != nil && !bytes.Equal(key, p.Key) {
			return stateError("VM properties keys disagree", nil)
		}
		key, next.Key = p.Key, p.Key
		if p.Slot != nil {
			r := p.Slot
			if r.Name != name || r.ID == "" || r.NativeHash == "" || r.ConfigHash == "" || !validSlotPhase(r.Phase) || !validSlotIdentity(r) {
				return stateError("invalid VM slot properties", nil)
			}
			if next.Slots[r.ID] != nil {
				return stateError("duplicate VM slot properties", nil)
			}
			next.Slots[r.ID] = r
		}
		if p.Image != nil {
			r := p.Image
			if r.Name != name || r.NativeHash == "" || r.Worker == "" || !validSlotPhase(r.Phase) {
				return stateError("invalid VM image properties", nil)
			}
			next.Images[name] = r
		}
		if p.Pair != nil {
			r := p.Pair
			if r.ID == "" || r.SlotID == "" || r.Primary.Name == "" || r.PrimaryHash == "" || (name != r.Primary.Name && name != r.Engine.Name) || !validPairPhase(r.Phase) {
				return stateError("invalid VM pair properties", nil)
			}
			if prev, ok := revisions[r.ID]; ok && prev >= p.Revision {
				continue
			}
			revisions[r.ID] = p.Revision
			next.Pairs[r.ID] = r
			// Any earlier member copy may predate handoff or teardown.
			for id, job := range next.Records {
				if job.PairID == r.ID {
					delete(next.Records, id)
				}
			}
		}
		if p.Record != nil {
			r := p.Record
			if r.LaunchID == "" || r.NativeHash == "" || r.Fingerprint == "" || (r.Name != name && (p.Pair == nil || r.PairID != p.Pair.ID || r.Name != p.Pair.Primary.Name)) {
				return stateError("invalid VM job properties", nil)
			}
			// Existence, not a saved active/busy flag, reserves the slot. Fresh
			// backend observation in reconciliation determines completion/failure.
			r.Phase, r.Outcome = "active", api.Accepted
			if p.Pair != nil && p.Pair.Phase == "handoff-intent" {
				r.Outcome = api.Unknown
			}
			if old := next.Records[r.LaunchID]; old != nil && old.Name != r.Name {
				return stateError("duplicate VM launch identity", nil)
			}
			next.Records[r.LaunchID] = r
		}
	}
	s.data = next
	s.propertySnapshot = all
	return nil
}

func (s *Store) encodeProperties() (map[string]json.RawMessage, error) {
	objects := map[string]*vmProperties{}
	revision := time.Now().UnixNano()
	for _, raw := range s.propertySnapshot {
		var previous vmProperties
		if json.Unmarshal(raw, &previous) == nil && revision <= previous.Revision {
			revision = previous.Revision + 1
		}
	}
	object := func(name string) *vmProperties {
		if objects[name] == nil {
			objects[name] = &vmProperties{Version: 1, Scope: s.data.Scope, Key: s.data.Key, Revision: revision}
		}
		return objects[name]
	}
	for _, r := range s.data.Records {
		if r.Phase == "done" {
			continue
		}
		copy := *r
		copy.Phase, copy.Outcome = "", ""
		object(r.Name).Record = &copy
	}
	for _, r := range s.data.Slots {
		object(r.Name).Slot = r
	}
	for _, r := range s.data.Images {
		object(r.Name).Image = r
	}
	for _, r := range s.data.Pairs {
		if r.Phase == "gone" {
			continue
		}
		object(r.Primary.Name).Pair = r
		if r.Engine.Name != "" {
			object(r.Engine.Name).Pair = r
			object(r.Engine.Name).Record = object(r.Primary.Name).Record
		}
	}
	out := map[string]json.RawMessage{}
	for name, p := range objects {
		var previous vmProperties
		if raw := s.propertySnapshot[name]; json.Unmarshal(raw, &previous) == nil {
			p.Revision = previous.Revision
			candidate, err := json.Marshal(p)
			if err == nil && bytes.Equal(candidate, raw) {
				out[name] = bytes.Clone(raw)
				continue
			}
			p.Revision = revision
		}
		b, err := json.Marshal(p)
		if err != nil {
			return nil, fmt.Errorf("encode VM properties: %w", err)
		}
		out[name] = b
	}
	return out, nil
}

func (s *Store) saveProperties() error {
	props, err := s.encodeProperties()
	if err != nil {
		return err
	}
	if err = s.properties.WriteVMProperties(props); err != nil {
		return stateError("write VM properties", err)
	}
	s.propertySnapshot = props
	return nil
}
