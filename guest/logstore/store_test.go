package logstore

import (
	"bytes"
	"encoding/json"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

func TestCaptureBinaryAndMirrorRestart(t *testing.T) {
	root := t.TempDir()
	id := "launch"
	payload := bytes.Repeat([]byte{0, 255, 'x', '\n'}, 2000)
	c := New(root, id, 1, 3, true)
	if n, e := c.Write(payload); n != len(payload) || e != nil {
		t.Fatal(n, e)
	}
	c.Close()
	p, e := Read(root, id, 0)
	if e != nil || p.Gap || p.Pending || !p.Manifest.Closed || p.Next != uint64(len(payload)) {
		t.Fatalf("%+v %v", p, e)
	}
	var got []byte
	for _, f := range p.Frames {
		got = append(got, f.Data...)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("binary output changed")
	}
	host := t.TempDir()
	if e = Mirror(host, id, p, true); e != nil {
		t.Fatal(e)
	}
	if e = Mirror(host, id, p, true); e != nil {
		t.Fatal("replay", e)
	}
	saved, e := Read(host, id, 0)
	if e != nil || saved.Gap || saved.Next != p.Next {
		t.Fatal(saved, e)
	}
	m, e := Load(Dir(host, id), id)
	if e != nil || m.End != p.Next || !m.Closed {
		t.Fatal(m, e)
	}
	tail, e := Read(host, id, 123)
	if e != nil || tail.Gap || tail.Frames[0].Offset != 123 {
		t.Fatal(tail, e)
	}
	if _, e = Load(Dir(host, id), "other"); e == nil {
		t.Fatal("identity mismatch accepted")
	}
}
func TestRotationCompressionAndRetentionGap(t *testing.T) {
	root := t.TempDir()
	id := "rotated"
	dir := Dir(root, id)
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	l := &lumberjack.Logger{Filename: filepath.Join(dir, "output.log"), MaxSize: 1, MaxBackups: 1, Compress: true}
	var offset uint64
	write := func(n int) {
		for i := 0; i < n; i++ {
			data := bytes.Repeat([]byte{'z'}, Chunk)
			f := Frame{Offset: offset, Data: data, CRC: crc32.ChecksumIEEE(data)}
			b, _ := json.Marshal(f)
			if _, e := l.Write(append(b, '\n')); e != nil {
				t.Fatal(e)
			}
			offset += Chunk
		}
	}
	write(100)
	if e := l.Rotate(); e != nil {
		t.Fatal(e)
	}
	time.Sleep(5 * time.Millisecond)
	write(100)
	if e := l.Rotate(); e != nil {
		t.Fatal(e)
	}
	write(1)
	_ = l.Close()
	if e := Save(dir, Manifest{LaunchID: id, End: offset, Closed: true}); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		files, _ := filepath.Glob(filepath.Join(dir, "*.gz"))
		raw, _ := filepath.Glob(filepath.Join(dir, "output-*.log"))
		if len(files) == 1 && len(raw) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("rotation did not compress/prune")
		}
		time.Sleep(10 * time.Millisecond)
	}
	p, e := Read(root, id, 0)
	if e != nil || !p.Gap || len(p.Frames) == 0 || p.Frames[0].Offset == 0 {
		t.Fatal(p, e)
	}
}
func TestEOFIsNotClosureAndTornFrameIsGap(t *testing.T) {
	root := t.TempDir()
	id := "active"
	dir := Dir(root, id)
	if e := Save(dir, Manifest{LaunchID: id, End: 10}); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, "output.log"), []byte(`{"offset":0`), 0600); e != nil {
		t.Fatal(e)
	}
	p, e := Read(root, id, 0)
	if e != nil || !p.Gap || p.Manifest.Closed {
		t.Fatal(p, e)
	}
	pending, e := Read(root, "missing", 0)
	if e != nil || !pending.Pending {
		t.Fatal(pending, e)
	}
}
func TestCaptureStorageFailureDoesNotBlockWriter(t *testing.T) {
	root := filepath.Join(t.TempDir(), "file")
	if e := os.WriteFile(root, []byte("x"), 0600); e != nil {
		t.Fatal(e)
	}
	c := New(root, "bad", 1, 1, true)
	start := time.Now()
	b := bytes.Repeat([]byte{'x'}, 2<<20)
	if n, e := c.Write(b); n != len(b) || e != nil {
		t.Fatal(n, e)
	}
	c.Close()
	if time.Since(start) > 3*time.Second {
		t.Fatal("capture blocked execution")
	}
}

func TestMirrorCoalescesWithoutDuplicateBytes(t *testing.T) {
	root := t.TempDir()
	var expected []byte
	for i := 0; i < 100; i++ {
		data := []byte{byte(i), 0, 255}
		start := uint64(len(expected))
		expected = append(expected, data...)
		p := Page{Manifest: Manifest{Version: 1, LaunchID: "quiet", End: uint64(len(expected)), Closed: i == 99}, Frames: []Frame{{Offset: start, Data: data, CRC: crc32.ChecksumIEEE(data)}}, Next: uint64(len(expected))}
		if err := Mirror(root, "quiet", p, true); err != nil {
			t.Fatal(err)
		}
	}
	files, _ := filepath.Glob(filepath.Join(Dir(root, "quiet"), "*.log.gz"))
	if len(files) != 1 {
		t.Fatal("small polls grew file count", len(files))
	}

	var got []byte
	var offset uint64
	for {
		p, err := Read(root, "quiet", offset)
		if err != nil || p.Gap || !p.Manifest.Closed {
			t.Fatal(p, err)
		}
		for _, f := range p.Frames {
			got = append(got, f.Data...)
		}
		offset = p.Next
		if offset == p.Manifest.End {
			break
		}
	}
	if !bytes.Equal(got, expected) {
		t.Fatal("coalescing changed output")
	}
}

func TestReadersDuringCompressionAndRotation(t *testing.T) {
	root := t.TempDir()
	id := "concurrent"
	dir := Dir(root, id)
	if err := PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	l := &lumberjack.Logger{Filename: filepath.Join(dir, "output.log"), MaxSize: 1, MaxBackups: 16, Compress: true}
	finished := make(chan error, 1)
	go func() {
		var offset uint64
		for i := 0; i < 12; i++ {
			data := bytes.Repeat([]byte{byte(i)}, 1000)
			f := Frame{Offset: offset, Data: data, CRC: crc32.ChecksumIEEE(data)}
			b, _ := json.Marshal(f)
			if _, err := l.Write(append(b, '\n')); err != nil {
				finished <- err
				return
			}
			offset += uint64(len(data))
			if err := Save(dir, Manifest{LaunchID: id, End: offset}); err != nil {
				finished <- err
				return
			}
			if err := l.Rotate(); err != nil {
				finished <- err
				return
			}
			time.Sleep(3 * time.Millisecond)
		}
		_ = l.Close()
		finished <- Save(dir, Manifest{LaunchID: id, End: offset, Closed: true})
	}()
	for {
		p, err := Read(root, id, 0)
		if err != nil || p.Gap {
			t.Fatalf("rotation produced a false gap: %+v %v", p, err)
		}
		select {
		case err := <-finished:
			if err != nil {
				t.Fatal(err)
			}
			p, err = Read(root, id, 0)
			if err != nil || p.Gap || p.Next != 12000 || !p.Manifest.Closed {
				t.Fatal(p, err)
			}
			return
		default:
			time.Sleep(time.Millisecond)
		}
	}
}
