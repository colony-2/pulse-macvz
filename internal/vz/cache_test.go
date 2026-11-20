package vz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCacheRejectsIncompleteOrMismatchedPublication(t *testing.T) {
	reference := "registry.invalid/base@sha256:" + strings.Repeat("a", 64)
	for _, state := range []string{"incomplete", "mismatched", "malformed", "verified"} {
		t.Run(state, func(t *testing.T) {
			s := &Store{Home: t.TempDir()}
			hash := sha256.Sum256([]byte(reference))
			dir := filepath.Join(s.Home, "cache", hex.EncodeToString(hash[:]))
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(dir, "verified.json")
			switch state {
			case "mismatched", "verified":
				saved := reference
				if state == "mismatched" {
					saved += "-different"
				}
				if err := atomicJSON(marker, saved); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				if err := os.WriteFile(marker, []byte("broken"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			got, err := s.cache(ctx, reference)
			if state == "verified" {
				if err != nil || got != dir {
					t.Fatalf("reuse published cache: path=%q error=%v", got, err)
				}
			} else if err == nil || (!strings.Contains(err.Error(), "incomplete") && !strings.Contains(err.Error(), "provenance")) {
				t.Fatalf("expected publication rejection before any registry request, got %v", err)
			}
		})
	}
}

func TestCacheWaitHonorsCancellation(t *testing.T) {
	s := &Store{Home: t.TempDir()}
	reference := "registry.invalid/base@sha256:" + strings.Repeat("b", 64)
	hash := sha256.Sum256([]byte(reference))
	root := filepath.Join(s.Home, "cache")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, hex.EncodeToString(hash[:])+".lock")
	lock, err := lockFile(context.Background(), path, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := s.cache(ctx, reference); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cache waiter did not respect deadline: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("cancelled waiter created cache data: entries=%v error=%v", entries, err)
	}
	if held, err := lockHeld(path); err != nil || !held {
		t.Fatalf("cancelled waiter disturbed existing writer: held=%t error=%v", held, err)
	}
}
