// Package logstore defines the private, versioned worker-output journal shared
// by the guest capture helper and the host collector. Offsets count raw bytes.
package logstore

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

const Chunk = 4096
const Batch = 64 << 10

var ErrJournal = errors.New("log journal unavailable or corrupt")

type Manifest struct {
	Version     int       `json:"version"`
	LaunchID    string    `json:"launch_id"`
	End         uint64    `json:"end"`
	Closed      bool      `json:"closed"`
	Unavailable bool      `json:"unavailable,omitempty"`
	Interrupted bool      `json:"interrupted,omitempty"`
	Lost        bool      `json:"lost,omitempty"`
	Updated     time.Time `json:"updated"`
}
type Frame struct {
	Offset uint64 `json:"offset"`
	Data   []byte `json:"data"`
	CRC    uint32 `json:"crc"`
}
type Page struct {
	Manifest Manifest `json:"manifest"`
	Frames   []Frame  `json:"frames"`
	Next     uint64   `json:"next"`
	Gap      bool     `json:"gap,omitempty"`
	Pending  bool     `json:"pending,omitempty"`
}
type Request struct {
	LaunchID string `json:"launch_id"`
	Offset   uint64 `json:"offset"`
}

func Key(id string) string       { h := sha256.Sum256([]byte(id)); return hex.EncodeToString(h[:]) }
func Dir(root, id string) string { return filepath.Join(root, Key(id)) }
func Atomic(dir, name string, b []byte) error {
	if e := PrivateDir(dir); e != nil {
		return e
	}
	f, e := os.CreateTemp(dir, ".log-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	c := f.Close()
	if e == nil {
		e = c
	}
	if e == nil {
		e = os.Rename(f.Name(), filepath.Join(dir, name))
	}
	if e != nil {
		return e
	}
	d, e := os.Open(dir)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func Save(dir string, m Manifest) error {
	m.Version = 1
	m.Updated = time.Now().UTC()
	b, _ := json.Marshal(m)
	return Atomic(dir, "manifest.json", b)
}
func Load(dir, id string) (Manifest, error) {
	var m Manifest
	b, e := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if e != nil {
		return m, e
	}
	if json.Unmarshal(b, &m) != nil || m.Version != 1 || m.LaunchID != id {
		return m, ErrJournal
	}
	return m, nil
}

// Read never treats temporary EOF as closure. Concurrent rotation can produce
// duplicate segments; frame offsets and checksums provide stable deduplication.
func Read(root, id string, offset uint64) (Page, error) {
	var p Page
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		p, err = readOnce(root, id, offset)
		if err != nil || !p.Gap {
			return p, err
		}
		// Directory snapshots race both guest rotation and host batch coalescing.
		// Rediscover before declaring loss when a path vanished or moved mid-read.
		if attempt < 2 {
			time.Sleep(2 * time.Millisecond)
		}
	}
	return p, err
}

func readOnce(root, id string, offset uint64) (Page, error) {
	dir := Dir(root, id)
	m, e := Load(dir, id)
	if os.IsNotExist(e) {
		return Page{Pending: true, Next: offset}, nil
	}
	if e != nil {
		return Page{}, e
	}
	out := Page{Manifest: m, Next: offset}
	if offset == m.End {
		return out, nil
	}
	if offset > m.End {
		return out, ErrJournal
	}
	files, e := os.ReadDir(dir)
	if e != nil {
		return out, e
	}
	// Rotated timestamps sort before the current file; mirror ranges sort by offset.
	sort.Slice(files, func(i, j int) bool {
		a, b := files[i].Name(), files[j].Name()
		if a == "output.log" {
			return false
		}
		if b == "output.log" {
			return true
		}
		return a < b
	})
	used := 0
	for _, entry := range files {
		name := entry.Name()
		if !(strings.HasSuffix(name, ".log") || strings.HasSuffix(name, ".log.gz")) {
			continue
		}
		// Lumberjack publishes gzip before removing its uncompressed source.
		// Prefer the source while both exist, and follow a completed rename if it
		// disappeared after ReadDir. Never consume an in-progress gzip stream.
		if strings.HasSuffix(name, ".gz") {
			if _, err := os.Stat(filepath.Join(dir, strings.TrimSuffix(name, ".gz"))); err == nil {
				continue
			}
		}
		f, err := os.Open(filepath.Join(dir, name))
		if os.IsNotExist(err) && strings.HasSuffix(name, ".log") && name != "output.log" {
			name += ".gz"
			f, err = os.Open(filepath.Join(dir, name))
		}
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return out, err
		}
		var r io.Reader = f
		var z *gzip.Reader
		if strings.HasSuffix(name, ".gz") {
			z, err = gzip.NewReader(f)
			if err != nil {
				f.Close()
				return out, ErrJournal
			}
			r = z
		}
		s := bufio.NewScanner(io.LimitReader(r, 129<<20))
		s.Buffer(make([]byte, 8192), 8192)
		for s.Scan() {
			var frame Frame
			if json.Unmarshal(s.Bytes(), &frame) != nil || len(frame.Data) > Chunk || len(frame.Data) == 0 || frame.CRC != crc32.ChecksumIEEE(frame.Data) {
				if out.Next < m.End {
					out.Gap = true
				}
				continue
			}
			end := frame.Offset + uint64(len(frame.Data))
			if end < frame.Offset || end > m.End {
				continue
			}
			if end <= out.Next {
				continue
			}
			if frame.Offset > out.Next {
				out.Gap = true
				out.Next = frame.Offset
			}
			if frame.Offset < out.Next {
				frame.Data = frame.Data[out.Next-frame.Offset:]
				frame.Offset = out.Next
				frame.CRC = crc32.ChecksumIEEE(frame.Data)
			}
			out.Frames = append(out.Frames, frame)
			used += len(frame.Data)
			out.Next = end
			if used >= Batch || len(out.Frames) >= 16 {
				break
			}
		}
		scanErr := s.Err()
		if z != nil {
			z.Close()
		}
		f.Close()
		if scanErr != nil {
			return out, ErrJournal
		}
		if used >= Batch || len(out.Frames) >= 16 {
			break
		}
	}
	if out.Next < m.End && used < Batch && len(out.Frames) < 16 {
		out.Gap = true
		out.Next = m.End
	}
	return out, nil
}

// Capture drains command output through a bounded queue. A full queue or failed
// disk loses logging, never execution. Absolute offsets make that loss visible.
type Capture struct {
	mu      sync.Mutex
	q       chan Frame
	done    chan struct{}
	m       Manifest
	dir     string
	logger  *lumberjack.Logger
	closed  bool
	initErr error
}

func New(root, id string, size, backups int, compress bool) *Capture {
	c := &Capture{q: make(chan Frame, 64), done: make(chan struct{}), m: Manifest{Version: 1, LaunchID: id}, dir: Dir(root, id)}
	c.initErr = PrivateDir(c.dir)
	c.logger = &lumberjack.Logger{Filename: filepath.Join(c.dir, "output.log"), MaxSize: size, MaxBackups: backups, Compress: compress, LocalTime: false}
	_ = Save(c.dir, c.m)
	go c.run()
	return c
}
func (c *Capture) Write(b []byte) (int, error) {
	n := len(b)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return n, nil
	}
	for len(b) > 0 {
		l := min(len(b), Chunk)
		f := Frame{Offset: c.m.End, Data: append([]byte(nil), b[:l]...)}
		f.CRC = crc32.ChecksumIEEE(f.Data)
		c.m.End += uint64(l)
		select {
		case c.q <- f:
		default:
			c.m.Lost = true
		}
		b = b[l:]
	}
	return n, nil
}
func (c *Capture) run() {
	defer close(c.done)
	for f := range c.q {
		b, _ := json.Marshal(f)
		e := c.initErr
		if e == nil {
			_, e = c.logger.Write(append(b, '\n'))
		}
		if e == nil {
			e = os.Chmod(c.logger.Filename, 0600)
		}
		// Publish only a durable prefix; final manifest also records dropped bytes.
		if e == nil {
			var file *os.File
			file, e = os.OpenFile(c.logger.Filename, os.O_RDWR, 0600)
			if e == nil {
				e = file.Sync()
				file.Close()
			}
		}
		c.mu.Lock()
		m := c.m
		m.End = f.Offset + uint64(len(f.Data))
		if e != nil {
			c.m.Lost = true
			m.Lost = true
		}
		c.mu.Unlock()
		_ = Save(c.dir, m)
	}
	_ = c.logger.Close()
	c.mu.Lock()
	m := c.m
	m.Closed = true
	c.mu.Unlock()
	_ = Save(c.dir, m)
}
func (c *Capture) Close() {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.q)
	}
	c.mu.Unlock()
	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
	}
}

// Mirror commits a bounded batch before advancing the durable cursor. Range
// filenames make crash replay idempotent. Compression is synchronous on the host.
func Mirror(root, id string, p Page, compress bool) error {
	dir := Dir(root, id)
	if p.Pending {
		return nil
	}
	if p.Manifest.LaunchID != id {
		return ErrJournal
	}

	oldName := ""
	if len(p.Frames) > 0 {
		var raw strings.Builder
		start := p.Frames[0].Offset
		// Coalesce small consecutive batches so quiet, long-running jobs do not
		// create a file every poll. At most 1 MiB is rewritten at a time.
		entries, _ := os.ReadDir(dir)
		for i := len(entries) - 1; i >= 0; i-- {
			name := entries[i].Name()
			first, last, ok := batchRange(name)
			if !ok {
				continue
			}
			if last == start {
				file, err := os.Open(filepath.Join(dir, name))
				if err != nil {
					break
				}
				var reader io.Reader = file
				var z *gzip.Reader
				if strings.HasSuffix(name, ".gz") {
					z, err = gzip.NewReader(file)
					if err == nil {
						reader = z
					}
				}
				var data []byte
				if err == nil {
					data, err = io.ReadAll(io.LimitReader(reader, 1<<20+1))
				}
				if z != nil {
					z.Close()
				}
				file.Close()
				if err == nil && len(data) < (1<<20)-(128<<10) {
					raw.Write(data)
					start = first
					oldName = name
				}
			}
			break
		}
		enc := json.NewEncoder(&raw)
		for _, f := range p.Frames {
			if e := enc.Encode(f); e != nil {
				return e
			}
		}
		var encoded strings.Builder
		var writer io.Writer = &encoded
		var z *gzip.Writer
		suffix := ".log"
		if compress {
			z = gzip.NewWriter(&encoded)
			writer = z
			suffix += ".gz"
		}
		if _, e := io.WriteString(writer, raw.String()); e != nil {
			return e
		}
		if z != nil {
			if e := z.Close(); e != nil {
				return e
			}
		}
		name := fmt.Sprintf("%020d-%020d%s", start, p.Next, suffix)
		if e := Atomic(dir, name, []byte(encoded.String())); e != nil {
			return e
		}
	}

	m := p.Manifest
	m.End = p.Next
	m.Closed = m.Closed && p.Next == p.Manifest.End
	m.Lost = m.Lost || p.Gap
	if e := Save(dir, m); e != nil {
		return e
	}
	if oldName != "" {
		_ = os.Remove(filepath.Join(dir, oldName))
	}
	return nil
}

// PrivateDir rejects symlinks and permissive pre-existing stream directories.
func PrivateDir(dir string) error {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	st, e := os.Lstat(dir)
	if e != nil {
		return e
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0077 != 0 {
		return ErrJournal
	}
	return nil
}

func batchRange(name string) (uint64, uint64, bool) {
	s := strings.TrimSuffix(strings.TrimSuffix(name, ".gz"), ".log")
	if len(s) != 41 || s[20] != '-' {
		return 0, 0, false
	}
	start, e1 := strconv.ParseUint(s[:20], 10, 64)
	end, e2 := strconv.ParseUint(s[21:], 10, 64)
	return start, end, e1 == nil && e2 == nil && start <= end
}
