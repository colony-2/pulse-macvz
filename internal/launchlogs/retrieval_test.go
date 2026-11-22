package launchlogs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse-macvz/guest/logstore"
	api "github.com/colony-2/pulse-provider-api"
)

func retrievalFixture(t *testing.T, root, id string, start, count int, closed bool) {
	t.Helper()
	var frames []logstore.Frame
	for i := start; i < start+count; i++ {
		data := []byte{byte(i), 0, 255}
		frames = append(frames, logstore.Frame{Offset: uint64(i * 3), Sequence: uint64(i + 1), Timestamp: time.Date(2026, 11, 21, 0, 0, i, 0, time.UTC), Data: data, CRC: crc32.ChecksumIEEE(data)})
	}
	end := uint64((start + count) * 3)
	if err := logstore.Mirror(root, id, logstore.Page{Manifest: logstore.Manifest{Version: 1, LaunchID: id, End: end, Closed: closed}, Frames: frames, Next: end}, true); err != nil {
		t.Fatal(err)
	}
}

func TestRetainedLogPagingRetryRestartAndPageSize(t *testing.T) {
	root := t.TempDir()
	retrievalFixture(t, root, "launch", 0, 120, true)
	a := &Archive{root: root} // No provider/VM; reads must use durable storage only.
	first, err := a.GetLaunchLog(t.Context(), api.LaunchLogRequest{LaunchID: "launch", PageSize: 1})
	if err != nil || len(first.Records) != 1 || first.Records[0].Sequence != 1 || !first.HasMore || !first.Finalized || first.Incomplete || *first.EstimatedTotalSizeBytes != 360 {
		t.Fatal(first, err)
	}
	reopened := &Archive{root: root}
	q := api.LaunchLogRequest{LaunchID: "launch", PageSize: 100, PageToken: first.NextPageToken}
	page, err := reopened.GetLaunchLog(t.Context(), q)
	if err != nil || len(page.Records) != 100 || page.Records[0].Sequence != 2 || page.Records[99].Sequence != 101 || !page.HasMore {
		t.Fatal(page, err)
	}
	again, err := reopened.GetLaunchLog(t.Context(), q)
	if err != nil || !reflect.DeepEqual(page, again) {
		t.Fatal("checkpoint consumed or unstable", err)
	}
	page.Records[0].Data[0] = 99
	if again.Records[0].Data[0] != 1 {
		t.Fatal("response aliases another page")
	}
	tail, err := a.GetLaunchLog(t.Context(), api.LaunchLogRequest{LaunchID: "launch", PageToken: page.NextPageToken})
	if err != nil || len(tail.Records) != 19 || tail.HasMore || !tail.Finalized {
		t.Fatal(tail, err)
	}
	empty, err := a.GetLaunchLog(t.Context(), api.LaunchLogRequest{LaunchID: "launch", PageToken: tail.NextPageToken})
	if err != nil || empty.Records == nil || len(empty.Records) != 0 || empty.HasMore || empty.NextPageToken == "" {
		t.Fatal(empty, err)
	}
}

func TestRetainedLogActiveTailAppendsAndFinalizes(t *testing.T) {
	root := t.TempDir()
	retrievalFixture(t, root, "launch", 0, 0, false)
	q := api.LaunchLogRequest{LaunchID: "launch"}
	p, err := ReadLaunchLog(t.Context(), root, q)
	if err != nil || p.Finalized || p.Records == nil || p.NextPageToken == "" {
		t.Fatal(p, err)
	}
	q.PageToken = p.NextPageToken
	retrievalFixture(t, root, "launch", 0, 2, false)
	p, err = ReadLaunchLog(t.Context(), root, q)
	if err != nil || p.Finalized || len(p.Records) != 2 {
		t.Fatal(p, err)
	}
	q.PageToken = p.NextPageToken
	retrievalFixture(t, root, "launch", 2, 1, true)
	p, err = ReadLaunchLog(t.Context(), root, q)
	if err != nil || !p.Finalized || len(p.Records) != 1 || p.Records[0].Sequence != 3 {
		t.Fatal(p, err)
	}
}

func TestRetainedLogCursorBindingTrimmingAndNotFoundPrecedence(t *testing.T) {
	root := t.TempDir()
	retrievalFixture(t, root, "one", 0, 3, true)
	retrievalFixture(t, root, "two", 0, 1, true)
	p, _ := ReadLaunchLog(t.Context(), root, api.LaunchLogRequest{LaunchID: "one", PageSize: 1})
	for _, q := range []api.LaunchLogRequest{{LaunchID: "one", PageToken: "invalid!"}, {LaunchID: "two", PageToken: p.NextPageToken}} {
		v, e := ReadLaunchLog(t.Context(), root, q)
		if !errors.Is(e, api.ErrLaunchLogCursor) || !reflect.DeepEqual(v, api.LaunchLogResponse{}) {
			t.Fatal(v, e)
		}
	}
	other := t.TempDir()
	retrievalFixture(t, other, "one", 0, 3, true)
	if _, e := ReadLaunchLog(t.Context(), other, api.LaunchLogRequest{LaunchID: "one", PageToken: p.NextPageToken}); !errors.Is(e, api.ErrLaunchLogCursor) {
		t.Fatal("scope mismatch", e)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(p.NextPageToken)
	var cp logCheckpoint
	json.Unmarshal(raw, &cp)
	cp.Offset = 1
	raw, _ = json.Marshal(cp)
	if _, e := ReadLaunchLog(t.Context(), root, api.LaunchLogRequest{LaunchID: "one", PageToken: base64.RawURLEncoding.EncodeToString(raw)}); !errors.Is(e, api.ErrLaunchLogCursor) {
		t.Fatal("mid-record cursor accepted", e)
	}
	// Simulate trimming while preserving the later immutable sequence/offset.
	entries, _ := os.ReadDir(logstore.Dir(root, "one"))
	for _, f := range entries {
		if strings.HasSuffix(f.Name(), ".gz") {
			os.Remove(filepath.Join(logstore.Dir(root, "one"), f.Name()))
		}
	}
	retrievalFixture(t, root, "one", 2, 1, true)
	if _, e := ReadLaunchLog(t.Context(), root, api.LaunchLogRequest{LaunchID: "one", PageToken: p.NextPageToken}); !errors.Is(e, api.ErrLaunchLogCursor) {
		t.Fatal("expired checkpoint reset", e)
	}
	fresh, e := ReadLaunchLog(t.Context(), root, api.LaunchLogRequest{LaunchID: "one"})
	if e != nil || !fresh.Incomplete || fresh.Records[0].Sequence != 3 || fresh.EstimatedTotalSizeBytes != nil {
		t.Fatal(fresh, e)
	}
	os.RemoveAll(logstore.Dir(root, "one"))
	if _, e := ReadLaunchLog(t.Context(), root, api.LaunchLogRequest{LaunchID: "one", PageToken: "bad"}); !errors.Is(e, api.ErrLaunchLogNotFound) {
		t.Fatal(e)
	}
}

func TestRetainedLogLegacyInterruptedAndRequestErrors(t *testing.T) {
	root := t.TempDir()
	id := "legacy"
	data := []byte{255, 0, 1}
	f := logstore.Frame{Data: data, CRC: crc32.ChecksumIEEE(data)}
	if e := logstore.Mirror(root, id, logstore.Page{Manifest: logstore.Manifest{Version: 1, LaunchID: id, End: 3, Interrupted: true}, Frames: []logstore.Frame{f}, Next: 3}, false); e != nil {
		t.Fatal(e)
	}
	p, e := ReadLaunchLog(t.Context(), root, api.LaunchLogRequest{LaunchID: id})
	if e != nil || !p.Finalized || !p.Incomplete || !p.Records[0].Timestamp.IsZero() || !reflect.DeepEqual(p.Records[0].Data, data) {
		t.Fatal(p, e)
	}
	// Even without a capture failure, the metadata limitation remains visible
	// when polling an empty checkpoint in a legacy log.
	if e := logstore.Save(logstore.Dir(root, id), logstore.Manifest{LaunchID: id, End: 3, Closed: true}); e != nil {
		t.Fatal(e)
	}
	tail, e := ReadLaunchLog(t.Context(), root, api.LaunchLogRequest{LaunchID: id, PageToken: p.NextPageToken})
	if e != nil || !tail.Incomplete || len(tail.Records) != 0 || tail.EstimatedTotalSizeBytes != nil {
		t.Fatal(tail, e)
	}
	for _, q := range []api.LaunchLogRequest{{LaunchID: "../bad"}, {LaunchID: id, PageSize: -1}, {LaunchID: id, PageSize: 101}, {LaunchID: id, PageToken: strings.Repeat("x", 16385)}} {
		if _, e := ReadLaunchLog(t.Context(), root, q); !errors.Is(e, api.ErrLaunchLogInvalidRequest) {
			t.Fatal(e)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, e := ReadLaunchLog(ctx, root, api.LaunchLogRequest{LaunchID: id}); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(logstore.Dir(root, id), "manifest.json"), []byte("corrupt"), 0600)
	if _, e := ReadLaunchLog(t.Context(), root, api.LaunchLogRequest{LaunchID: id}); !errors.Is(e, logstore.ErrJournal) {
		t.Fatal("backend error became empty success", e)
	}
}
