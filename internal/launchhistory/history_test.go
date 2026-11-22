package launchhistory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse-macvz/guest/logstore"
	"github.com/colony-2/pulse-macvz/internal/local"
)

func TestHistoryPreservesIdentityAndSurvivesLogDeletion(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	r := Record{LaunchID: "../unsafe/id", Tenant: "tenant", JobID: "job", VMName: "vm", VMUID: "uid", SubmittedAt: &now, ObservedAt: &now, C2J: &local.ImageVersion{Version: "v1", Revision: "1234567"}, Phase: "active"}
	if e := Save(root, r); e != nil {
		t.Fatal(e)
	}
	if e := logstore.Save(logstore.Dir(root, r.LaunchID), logstore.Manifest{LaunchID: r.LaunchID}); e != nil {
		t.Fatal(e)
	}
	if e := os.RemoveAll(logstore.Dir(root, r.LaunchID)); e != nil {
		t.Fatal(e)
	}
	later := now.Add(time.Hour)
	if e := Save(root, Record{LaunchID: r.LaunchID, VMUID: "uid", Phase: "done", ObservedAt: &later, CleanupObservedAt: &later}); e != nil {
		t.Fatal(e)
	}
	got, e := Read(root, r.LaunchID)
	if e != nil || got.C2J.Version != "v1" || got.JobID != "job" || !got.SubmittedAt.Equal(now) || got.Phase != "done" {
		t.Fatal(got, e)
	}
	if e := Save(root, Record{LaunchID: r.LaunchID, VMUID: "replacement"}); e != ErrIdentity {
		t.Fatal("allowed identity replacement", e)
	}
	info, e := os.Stat(path(root, r.LaunchID))
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, e)
	}
	if e := Save(root, Record{LaunchID: r.LaunchID, Phase: "active"}); e != nil {
		t.Fatal(e)
	}
	got, _ = Read(root, r.LaunchID)
	if got.Phase != "done" {
		t.Fatal("legacy metadata replaced current phase", got)
	}
}
func TestHistoryUnchangedObservationDoesNotRewriteAndRejectsUnsafeMetadata(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	r := Record{LaunchID: "id", ObservedAt: &now, Phase: "active"}
	if e := Save(root, r); e != nil {
		t.Fatal(e)
	}
	raw, _ := os.ReadFile(path(root, "id"))
	later := now.Add(time.Second)
	r.ObservedAt = &later
	if e := Save(root, r); e != nil {
		t.Fatal(e)
	}
	next, _ := os.ReadFile(path(root, "id"))
	if string(raw) != string(next) {
		t.Fatal("rewrote unchanged record")
	}
	if e := os.WriteFile(filepath.Join(root, "large"), []byte(strings.Repeat("x", 65537)), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := ReadJSON(filepath.Join(root, "large")); e == nil {
		t.Fatal("read oversized metadata")
	}
	if e := os.Symlink(path(root, "id"), filepath.Join(root, "link")); e != nil {
		t.Fatal(e)
	}
	if _, e := ReadJSON(filepath.Join(root, "link")); e == nil {
		t.Fatal("followed metadata symlink")
	}
}
