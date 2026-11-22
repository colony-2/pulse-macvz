package servercli

import (
	"bytes"
	"context"
	"errors"
	"github.com/colony-2/pulse-macvz/internal/launchhistory"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colony-2/pulse-macvz/guest/logstore"
	"github.com/colony-2/pulse-macvz/internal/config"
)

func TestLogsReadsRetainedOutputWithoutController(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	id := "launch"
	scope := (config.Settings{Config: config.Config{OwnerID: "server"}, Kind: "vz", Endpoint: home}).Scope()
	payload := []byte{0, 255, 'x'}
	p := logstore.Page{Manifest: logstore.Manifest{Version: 1, LaunchID: id, End: 3, Closed: true}, Frames: []logstore.Frame{{Data: payload, CRC: crc32.ChecksumIEEE(payload)}}, Next: 3}
	if e := logstore.Mirror(filepath.Join(root, scope), id, p, true); e != nil {
		t.Fatal(e)
	}
	cmd := NewLogsCommand()
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--config", filepath.Join(home, "absent.json"), "--vm-home", home, "--log-directory", root, "--launch-id", id})
	// Explicit missing config is an error; use an empty valid file.
	if e := os.WriteFile(filepath.Join(home, "absent.json"), []byte(`{"version":1,"flags":{}}`), 0600); e != nil {
		t.Fatal(e)
	}
	if e := cmd.Execute(); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(out.Bytes(), payload) || stderr.Len() != 0 {
		t.Fatal(out.Bytes(), stderr.String())
	}
}

func TestLogFollowStreamsCompressedBatchesOnceAndStopsAtClosure(t *testing.T) {
	root := t.TempDir()
	id := "follow"
	first := []byte("first\n")
	mirror := func(start uint64, data []byte, closed bool) {
		t.Helper()
		end := start + uint64(len(data))
		p := logstore.Page{Manifest: logstore.Manifest{LaunchID: id, End: end, Closed: closed}, Frames: []logstore.Frame{{Offset: start, Data: data, CRC: crc32.ChecksumIEEE(data)}}, Next: end}
		if e := logstore.Mirror(root, id, p, true); e != nil {
			t.Fatal(e)
		}
	}
	mirror(0, first, false)
	out := &notifyingLogWriter{seen: make(chan struct{}, 1)}
	var diagnostics bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- readLaunchLogs(ctx, root, id, 0, true, out, &diagnostics) }()
	select {
	case <-out.seen:
	case <-ctx.Done():
		t.Fatal("no initial log output")
	}
	mirror(uint64(len(first)), []byte("second\n"), true)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if out.String() != "first\nsecond\n" || diagnostics.Len() != 0 {
		t.Fatal(out.String(), diagnostics.String())
	}
}

type notifyingLogWriter struct {
	mu   sync.Mutex
	data bytes.Buffer
	seen chan struct{}
}

func (w *notifyingLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, e := w.data.Write(p)
	w.mu.Unlock()
	select {
	case w.seen <- struct{}{}:
	default:
	}
	return n, e
}
func (w *notifyingLogWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.data.String()
}
func TestLogFollowCancellationUnavailableAndMissing(t *testing.T) {
	root := t.TempDir()
	if e := launchhistory.Save(root, launchhistory.Record{LaunchID: "pending"}); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- readLaunchLogs(ctx, root, "pending", 0, true, io.Discard, io.Discard) }()
	cancel()
	if e := <-done; !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if e := readLaunchLogs(t.Context(), root, "missing", 0, true, io.Discard, io.Discard); e == nil {
		t.Fatal("waited for nonexistent launch")
	}
	if e := logstore.Save(logstore.Dir(root, "unavailable"), logstore.Manifest{LaunchID: "unavailable", Unavailable: true, Interrupted: true}); e != nil {
		t.Fatal(e)
	}
	if e := readLaunchLogs(t.Context(), root, "unavailable", 0, true, io.Discard, io.Discard); e == nil || !strings.Contains(e.Error(), "helper unavailable") {
		t.Fatal(e)
	}
}
func TestLogsPositionalAndLegacyFlagAgree(t *testing.T) {
	o, conf := historyOptions(t)
	id := "positional"
	if e := logstore.Save(logstore.Dir(logRoot(o), id), logstore.Manifest{LaunchID: id, Closed: true}); e != nil {
		t.Fatal(e)
	}
	for _, args := range [][]string{{id}, {"--launch-id", id}, {id, "--launch-id", id}} {
		cmd := NewLogsCommand()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(append(args, "--config", conf, "--vm-home", o.vmHome, "--log-directory", o.logDirectory))
		if e := cmd.Execute(); e != nil {
			t.Fatal(e)
		}
	}
	cmd := NewLogsCommand()
	cmd.SetArgs([]string{id, "--launch-id", "conflict", "--config", conf})
	if e := cmd.Execute(); e == nil {
		t.Fatal("conflicting identities accepted")
	}
}

func TestLogFollowReportsArchiveRemoval(t *testing.T) {
	root := t.TempDir()
	id := "removed"
	now := time.Now().UTC()
	if e := launchhistory.Save(root, launchhistory.Record{LaunchID: id, ObservedAt: &now, Phase: "active"}); e != nil {
		t.Fatal(e)
	}
	data := []byte("present\n")
	p := logstore.Page{Manifest: logstore.Manifest{LaunchID: id, End: uint64(len(data))}, Frames: []logstore.Frame{{Data: data, CRC: crc32.ChecksumIEEE(data)}}, Next: uint64(len(data))}
	if e := logstore.Mirror(root, id, p, true); e != nil {
		t.Fatal(e)
	}
	out := &notifyingLogWriter{seen: make(chan struct{}, 1)}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- readLaunchLogs(ctx, root, id, 0, true, out, io.Discard) }()
	select {
	case <-out.seen:
	case <-ctx.Done():
		t.Fatal("no output")
	}
	if e := os.RemoveAll(logstore.Dir(root, id)); e != nil {
		t.Fatal(e)
	}
	if e := <-done; e == nil || !strings.Contains(e.Error(), "no longer retained") {
		t.Fatal(e)
	}
}
