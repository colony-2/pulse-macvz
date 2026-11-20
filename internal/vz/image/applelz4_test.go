// Copyright (c) 2025 Travis Cline. MIT license; see LICENSE.cove.
// Adapted from tmc/cove 8dad6f9abe6a3c5de68b32f09ffb3875a3a6651e.
package image

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"testing"
)

func TestAppleLZ4RoundTripCompressible(t *testing.T) {
	// 1 MiB of repeating text — highly compressible, exercises the
	// compressed-block path (bv41).
	src := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 1<<14)
	enc, err := CompressAppleLZ4(src)
	if err != nil {
		t.Fatalf("CompressAppleLZ4: %v", err)
	}
	if len(enc) >= len(src) {
		t.Errorf("compressed size %d not smaller than raw %d", len(enc), len(src))
	}
	if !bytes.HasPrefix(enc, appleLZ4MagicCompressed[:]) {
		t.Errorf("compressed stream missing bv41 magic: %q", enc[:4])
	}
	dec, err := DecompressAppleLZ4(enc)
	if err != nil {
		t.Fatalf("DecompressAppleLZ4: %v", err)
	}
	if !bytes.Equal(dec, src) {
		t.Errorf("round-trip mismatch: got %d bytes, want %d", len(dec), len(src))
	}
}

func TestAppleLZ4RoundTripIncompressible(t *testing.T) {
	// 4 KiB of random bytes — usually inflates under LZ4 so the encoder
	// falls back to the uncompressed-block path (bv4-).
	src := make([]byte, 4096)
	r := rand.New(rand.NewSource(0xC07E_7A47))
	r.Read(src)
	enc, err := CompressAppleLZ4(src)
	if err != nil {
		t.Fatalf("CompressAppleLZ4: %v", err)
	}
	dec, err := DecompressAppleLZ4(enc)
	if err != nil {
		t.Fatalf("DecompressAppleLZ4: %v", err)
	}
	if !bytes.Equal(dec, src) {
		t.Errorf("round-trip mismatch: got %d bytes, want %d", len(dec), len(src))
	}
}

func TestAppleLZ4RoundTripEmpty(t *testing.T) {
	enc, err := CompressAppleLZ4(nil)
	if err != nil {
		t.Fatalf("CompressAppleLZ4(nil): %v", err)
	}
	if !bytes.Equal(enc, appleLZ4MagicEndOfStream[:]) {
		t.Errorf("empty input should encode as just bv4$, got %x", enc)
	}
	dec, err := DecompressAppleLZ4(enc)
	if err != nil {
		t.Fatalf("DecompressAppleLZ4: %v", err)
	}
	if len(dec) != 0 {
		t.Errorf("empty round-trip produced %d bytes, want 0", len(dec))
	}
}

func TestAppleLZ4MultipleBlocks(t *testing.T) {
	// Two appleLZ4MaxBlockSize chunks force the encoder to emit two blocks
	// followed by the EOS marker. We don't actually allocate 128 MiB —
	// instead, hand-craft a stream and confirm the decoder iterates blocks.
	var stream bytes.Buffer
	for i := 0; i < 3; i++ {
		raw := []byte{byte(i)}
		var hdr [8]byte
		copy(hdr[:4], appleLZ4MagicUncompressed[:])
		binary.LittleEndian.PutUint32(hdr[4:8], 1)
		stream.Write(hdr[:])
		stream.Write(raw)
	}
	stream.Write(appleLZ4MagicEndOfStream[:])
	dec, err := DecompressAppleLZ4(stream.Bytes())
	if err != nil {
		t.Fatalf("DecompressAppleLZ4: %v", err)
	}
	if !bytes.Equal(dec, []byte{0, 1, 2}) {
		t.Errorf("multi-block decode = %v, want [0 1 2]", dec)
	}
}

func TestAppleLZ4UnknownMagic(t *testing.T) {
	// Garbage prefix should fail with an "unknown block magic" error rather
	// than crash or silently misinterpret.
	bad := []byte{'b', 'v', '4', 'X', 0, 0, 0, 0}
	_, err := DecompressAppleLZ4(bad)
	if err == nil {
		t.Fatal("expected error on unknown magic, got nil")
	}
}

func TestAppleLZ4Truncated(t *testing.T) {
	src := []byte("hello tart")
	enc, err := CompressAppleLZ4(src)
	if err != nil {
		t.Fatal(err)
	}
	// Drop the EOS marker; the decoder should report ErrUnexpectedEOF.
	truncated := enc[:len(enc)-len(appleLZ4MagicEndOfStream)]
	_, err = DecompressAppleLZ4(truncated)
	if err == nil {
		t.Fatal("expected error on truncated stream, got nil")
	}
}

func TestAppleLZ4UncompressedFallback(t *testing.T) {
	// A 1-byte input is typically smaller than its LZ4 representation;
	// confirm the encoder picked the bv4- (uncompressed) path.
	src := []byte{0x42}
	enc, err := CompressAppleLZ4(src)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(enc, appleLZ4MagicUncompressed[:]) {
		t.Errorf("expected bv4- prefix for 1-byte input, got %q", enc[:4])
	}
	dec, err := DecompressAppleLZ4(enc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dec, src) {
		t.Errorf("round-trip mismatch: got %x, want %x", dec, src)
	}
}
