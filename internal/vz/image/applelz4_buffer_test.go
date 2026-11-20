// Copyright (c) 2025 Travis Cline. MIT license; see LICENSE.cove.
// Adapted from tmc/cove 8dad6f9abe6a3c5de68b32f09ffb3875a3a6651e.
package image

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestBufferedByteReaderPassesThroughBufferedTypes(t *testing.T) {
	br := bytes.NewReader([]byte("abc"))
	if got := bufferedByteReader(br); got != br {
		t.Errorf("bytes.Reader: got %T, want passthrough", got)
	}
	bb := bytes.NewBuffer([]byte("abc"))
	if got := bufferedByteReader(bb); got != bb {
		t.Errorf("bytes.Buffer: got %T, want passthrough", got)
	}
	bufR := bufio.NewReader(strings.NewReader("abc"))
	if got := bufferedByteReader(bufR); got != bufR {
		t.Errorf("bufio.Reader: got %T, want passthrough", got)
	}
}

func TestBufferedByteReaderWrapsUnbuffered(t *testing.T) {
	sr := strings.NewReader("hello world")
	got := bufferedByteReader(sr)
	if _, ok := got.(*smallReadBuffer); !ok {
		t.Fatalf("strings.Reader: got %T, want *smallReadBuffer", got)
	}
}

func TestSmallReadBufferReadPaths(t *testing.T) {
	// Large dst (>= 16): bypasses internal buffer, reads directly.
	rb := bufferedByteReader(&plainReader{r: strings.NewReader("0123456789ABCDEFGHIJ")}).(*smallReadBuffer)
	big := make([]byte, 32)
	n, err := rb.Read(big)
	if err != nil || n != 20 || string(big[:n]) != "0123456789ABCDEFGHIJ" {
		t.Fatalf("large read: n=%d err=%v data=%q", n, err, big[:n])
	}

	// Small dst (< 16): fills internal 16-byte buffer, then drains it.
	rb2 := bufferedByteReader(&plainReader{r: strings.NewReader("0123456789ABCDEFxyz")}).(*smallReadBuffer)
	small := make([]byte, 4)
	n, err = rb2.Read(small)
	if err != nil || n != 4 || string(small[:n]) != "0123" {
		t.Fatalf("first small read: n=%d err=%v data=%q", n, err, small[:n])
	}
	// Subsequent small read drains buffered bytes (no underlying Read call).
	n, err = rb2.Read(small)
	if err != nil || n != 4 || string(small[:n]) != "4567" {
		t.Fatalf("buffered drain: n=%d err=%v data=%q", n, err, small[:n])
	}
	// Drain the rest and confirm EOF eventually.
	rest, err := io.ReadAll(rb2)
	if err != nil {
		t.Fatalf("ReadAll rest: %v", err)
	}
	if got := string(rest); got != "89ABCDEFxyz" {
		t.Fatalf("rest: got %q, want %q", got, "89ABCDEFxyz")
	}
}

// plainReader hides any methods that would let bufferedByteReader pass through.
type plainReader struct{ r io.Reader }

func (p *plainReader) Read(b []byte) (int, error) { return p.r.Read(b) }

func TestSmallReadBufferPropagatesEOF(t *testing.T) {
	rb := bufferedByteReader(&plainReader{r: strings.NewReader("")}).(*smallReadBuffer)
	buf := make([]byte, 4)
	n, err := rb.Read(buf)
	if n != 0 || err != io.EOF {
		t.Fatalf("empty read: n=%d err=%v, want 0,EOF", n, err)
	}
}

// Readers may return bytes and an error together. Keep the error until all
// buffered bytes have been delivered, even if the next underlying read succeeds.
func TestSmallReadBufferPreservesErrorWithData(t *testing.T) {
	failure := io.ErrUnexpectedEOF
	r := &dataAndErrorReader{err: failure}
	buffered := bufferedByteReader(r)
	p := make([]byte, 2)
	n, err := buffered.Read(p)
	if n != 2 || err != nil || string(p) != "ab" {
		t.Fatalf("first read: %d, %v, %q", n, err, p)
	}
	n, err = buffered.Read(p)
	if n != 1 || err != nil || p[0] != 'c' {
		t.Fatalf("second read: %d, %v", n, err)
	}
	n, err = buffered.Read(p)
	if n != 0 || err != failure {
		t.Fatalf("lost error: %d, %v", n, err)
	}
}

type dataAndErrorReader struct{ err error }

func (r *dataAndErrorReader) Read(p []byte) (int, error) {
	if r.err == nil {
		return 0, io.EOF
	}
	err := r.err
	r.err = nil
	return copy(p, "abc"), err
}
