// Package launchhistory stores public launch metadata independently of VM and
// log retention. It is observational and is never used to reconstruct capacity.
package launchhistory

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/colony-2/pulse-macvz/guest/logstore"
	"github.com/colony-2/pulse-macvz/internal/config"
	"github.com/colony-2/pulse-macvz/internal/local"
)

type Record struct {
	Version           int                 `json:"version"`
	LaunchID          string              `json:"launch_id"`
	InstanceID        string              `json:"jobdb_instance,omitempty"`
	Tenant            string              `json:"tenant,omitempty"`
	JobID             string              `json:"job_id,omitempty"`
	VMName            string              `json:"vm_name,omitempty"`
	VMUID             string              `json:"vm_uid,omitempty"`
	Slot              string              `json:"slot,omitempty"`
	Base              string              `json:"base,omitempty"`
	C2J               *local.ImageVersion `json:"c2j,omitempty"`
	SubmittedAt       *time.Time          `json:"submitted_at,omitempty"`
	ObservedAt        *time.Time          `json:"observed_at,omitempty"`
	CleanupObservedAt *time.Time          `json:"cleanup_observed_at,omitempty"`
	Admission         string              `json:"admission,omitempty"`
	Phase             string              `json:"last_recorded_phase,omitempty"`
}

func Root(c config.Settings) string {
	root := c.Logging.Directory
	if root == "" {
		root = c.Endpoint
		if root == "" {
			root = c.StateDir
		}
		root = filepath.Join(root, "logs")
	}
	return filepath.Join(root, c.Scope())
}

var ErrIdentity = errors.New("invalid or conflicting launch history identity")
var writeMu sync.Mutex // One controller per scope; CLI readers never take this lock.

func path(root, id string) string { return filepath.Join(root, "history", logstore.Key(id)+".json") }
func Read(root, id string) (Record, error) {
	var r Record
	data, err := ReadJSON(path(root, id))
	if err != nil {
		return r, err
	}
	if json.Unmarshal(data, &r) != nil || r.Version != 1 || r.LaunchID != id || id == "" {
		return r, ErrIdentity
	}
	return r, nil
}

// ReadJSON bounds metadata reads and refuses symlinks/nonregular files.
func ReadJSON(name string) ([]byte, error) {
	info, e := os.Lstat(name)
	if e != nil {
		return nil, e
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return nil, ErrIdentity
	}
	f, e := os.Open(name)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 64<<10))
}

// Merge never overwrites known identity with a conflicting or missing value.
// Old archives cannot supply submission time or image version; leave these nil.
func Merge(old, next Record) (Record, error) {
	if old.LaunchID != "" && old.LaunchID != next.LaunchID {
		return old, ErrIdentity
	}
	pairs := [][2]string{{old.VMUID, next.VMUID}, {old.VMName, next.VMName}, {old.InstanceID, next.InstanceID}, {old.Tenant, next.Tenant}, {old.JobID, next.JobID}, {old.Base, next.Base}, {old.Slot, next.Slot}}
	for _, p := range pairs {
		if p[0] != "" && p[1] != "" && p[0] != p[1] {
			return old, ErrIdentity
		}
	}
	if next.LaunchID == "" {
		return old, ErrIdentity
	}
	old.Version = 1
	old.LaunchID = next.LaunchID
	for _, p := range []struct {
		dst *string
		src string
	}{{&old.InstanceID, next.InstanceID}, {&old.Tenant, next.Tenant}, {&old.JobID, next.JobID}, {&old.VMName, next.VMName}, {&old.VMUID, next.VMUID}, {&old.Slot, next.Slot}, {&old.Base, next.Base}} {
		if p.src != "" {
			*p.dst = p.src
		}
	}
	if old.SubmittedAt == nil {
		old.SubmittedAt = next.SubmittedAt
	}
	if old.C2J == nil {
		old.C2J = next.C2J
	}
	if next.ObservedAt != nil && (old.ObservedAt == nil || !next.ObservedAt.Before(*old.ObservedAt)) {
		old.ObservedAt = next.ObservedAt
		if next.Phase != "" {
			old.Phase = next.Phase
		}
		if next.Admission != "" {
			old.Admission = next.Admission
		}
	} else if old.ObservedAt == nil {
		if next.Phase != "" {
			old.Phase = next.Phase
		}
		if next.Admission != "" {
			old.Admission = next.Admission
		}
	}
	if old.CleanupObservedAt == nil {
		old.CleanupObservedAt = next.CleanupObservedAt
	}
	return old, nil
}

func Save(root string, r Record) error {
	writeMu.Lock()
	defer writeMu.Unlock()
	old, e := Read(root, r.LaunchID)
	if e != nil && !os.IsNotExist(e) {
		return e
	}
	merged, e := Merge(old, r)
	if e != nil {
		return e
	}
	// Observation time advances only for changed metadata, not every pool tick.
	before, _ := json.Marshal(old)
	same := merged
	same.ObservedAt = old.ObservedAt
	after, _ := json.Marshal(same)
	if bytes.Equal(before, after) {
		return nil
	}
	if e = logstore.PrivateDir(root); e != nil {
		return e
	}
	data, e := json.Marshal(merged)
	if e != nil {
		return e
	}
	return logstore.Atomic(filepath.Join(root, "history"), logstore.Key(r.LaunchID)+".json", data)
}
