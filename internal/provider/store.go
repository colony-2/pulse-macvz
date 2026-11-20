package provider

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/colony-2/pulse-tart/internal/ctxlock"
	"os"
	"path/filepath"
	"time"

	compute "github.com/colony-2/pulse-provider-api"
	"golang.org/x/sys/unix"
)

var ErrState = errors.New("provider state unavailable")

type Record struct {
	diagnostic  string         // Latest in-memory outcome detail; never persisted.
	LaunchID    string         `json:"launch_id"`
	Fingerprint string         `json:"fingerprint"`
	NativeHash  string         `json:"native_hash"`
	Name        string         `json:"name"`
	UID         string         `json:"uid"`
	Job         compute.JobRef `json:"job"`
	Created     time.Time      `json:"created"`
	Outcome     compute.Status `json:"outcome"`
	Phase       string         `json:"phase"`
	PairID      string         `json:"pair_id,omitempty"`
	SlotID      string         `json:"slot_id,omitempty"`
}
type state struct {
	Version int                     `json:"version"`
	Scope   string                  `json:"scope"`
	Key     []byte                  `json:"key"`
	Records map[string]*Record      `json:"records"`
	Slots   map[string]*SlotRecord  `json:"slots,omitempty"`
	Pairs   map[string]*PairRecord  `json:"pairs,omitempty"`
	Images  map[string]*ImageRecord `json:"images,omitempty"`
}
type Store struct {
	mu               ctxlock.Mutex
	dir              string
	lock             *os.File
	data             state
	broken           bool
	brokenCause      error
	io               *journalIO
	properties       VMPropertyBackend
	propertySnapshot map[string]json.RawMessage
}

func OpenStore(dir, scope string) (*Store, error) {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, stateError("create state directory", e)
	}
	st, e := os.Lstat(dir)
	if e != nil {
		return nil, stateError("inspect state directory", e)
	}
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return nil, stateError("state directory must be private", nil)
	}
	f, e := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, stateError("open journal lock", e)
	}
	if e := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		f.Close()
		return nil, stateError("lock journal", e)
	}
	s := &Store{dir: dir, lock: f}
	st, e = os.Lstat(filepath.Join(dir, "state.json"))
	if e == nil && (!st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0) {
		s.Close()
		return nil, stateError("state file must be private and regular", nil)
	}
	b, e := os.ReadFile(filepath.Join(dir, "state.json"))
	if os.IsNotExist(e) {
		s.data = state{Version: 3, Scope: scope, Key: make([]byte, 32), Records: map[string]*Record{}}
		if _, e = rand.Read(s.data.Key); e == nil {
			e = s.save()
		}
	} else if e == nil {
		e = json.Unmarshal(b, &s.data)
	}
	if e != nil {
		s.Close()
		return nil, stateError("read or decode journal", e)
	}
	if s.data.Version != 3 || s.data.Scope != scope || len(s.data.Key) != 32 || s.data.Records == nil {
		s.Close()
		return nil, stateError("invalid journal version, scope, key or records", nil)
	}
	for id, r := range s.data.Records {
		if r == nil || r.LaunchID != id || r.Name == "" || r.Fingerprint == "" || r.NativeHash == "" || (r.Outcome != compute.Accepted && r.Outcome != compute.Unknown) || !validPhase(r.Phase) {
			s.Close()
			return nil, stateError("record validation failed", nil)
		}
	}
	if s.data.Pairs == nil {
		s.data.Pairs = map[string]*PairRecord{}
	}
	for id, p := range s.data.Pairs {
		if p == nil || p.ID != id || p.Primary.Name == "" || p.PrimaryHash == "" || p.SlotID == "" || !validPairPhase(p.Phase) {
			s.Close()
			return nil, stateError("pair validation failed", nil)
		}
	}
	for _, r := range s.data.Records {
		if r.PairID != "" && s.data.Pairs[r.PairID] == nil {
			s.Close()
			return nil, stateError("record references missing pair", nil)
		}
	}
	if s.data.Slots == nil {
		s.data.Slots = map[string]*SlotRecord{}
	}
	if s.data.Images == nil {
		s.data.Images = map[string]*ImageRecord{}
	}
	for name, image := range s.data.Images {
		if image == nil || name != image.Name || image.NativeHash == "" || image.Worker == "" || !validSlotPhase(image.Phase) {
			s.Close()
			return nil, stateError("image validation failed", nil)
		}
	}
	for id, r := range s.data.Slots {
		if r == nil || id != r.ID || r.Name == "" || r.NativeHash == "" || r.ConfigHash == "" || !validSlotPhase(r.Phase) || !validSlotIdentity(r) {
			s.Close()
			return nil, stateError("slot validation failed", nil)
		}
	}
	return s, nil
}
func (s *Store) Close() error {
	if s.lock != nil {
		return s.lock.Close()
	}
	return nil
}
func (s *Store) hash(v any) string {
	b, _ := json.Marshal(v)
	h := hmac.New(sha256.New, s.data.Key)
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}
func (s *Store) save() error {
	if s.properties != nil {
		return s.saveProperties()
	}
	if s.broken {
		return s.stateError()
	}
	ops := defaultJournalIO()
	if s.io != nil {
		ops = *s.io
	}
	b, e := ops.marshal(s.data)
	if e != nil {
		return stateError("marshal journal", e)
	}
	f, e := ops.create(s.dir, ".state-")
	if e != nil {
		s.broken = true
		s.brokenCause = stateError("create journal temporary file", e)
		return s.brokenCause
	}
	name := f.Name()
	defer os.Remove(name)
	op := "write journal"
	if _, e = f.Write(b); e == nil {
		op = "sync journal file"
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		op = "close journal file"
		e = ce
	}
	if e == nil {
		op = "rename journal"
		e = ops.rename(name, filepath.Join(s.dir, "state.json"))
	}
	if e == nil {
		op = "sync journal directory"
		d, de := ops.open(s.dir)
		if de == nil {
			e = d.Sync()
			d.Close()
		} else {
			e = de
		}
	}
	if e != nil {
		s.broken = true
		s.brokenCause = stateError(op, e)
		return s.brokenCause
	}
	return nil
}

func validPhase(phase string) bool {
	switch phase {
	case "dispatch", "active", "deleting", "quarantine", "done":
		return true
	}
	return false
}

func validPairPhase(s string) bool {
	switch s {
	case "reserved", "engine-dispatch", "primary-dispatch", "configuring", "docker-starting", "ready", "handoff-intent", "running", "reclaiming", "quarantine", "gone":
		return true
	}
	return false
}

func stateError(operation string, cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrState, operation)
	}
	return fmt.Errorf("%w: %s: %w", ErrState, operation, cause)
}
func (s *Store) stateError() error {
	if s.brokenCause != nil {
		return s.brokenCause
	}
	return stateError("journal is broken; original cause unavailable", nil)
}
