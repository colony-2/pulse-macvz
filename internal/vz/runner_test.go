package vz

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestColdBootCompatibilityPreservesExistingSnapshots(t *testing.T) {
	legacy := `{"os":"darwin","cpuCount":4,"memorySize":8589934592,"macAddress":"02:00:00:00:00:01","vzVersion":1,"arch":"arm64"}`
	var c diskConfig
	if err := json.Unmarshal([]byte(legacy), &c); err != nil {
		t.Fatal(err)
	}
	oldHash := fmt.Sprintf("%x", sha256.Sum256([]byte(legacy)))
	if configHash(c) != oldHash {
		t.Fatal("legacy snapshot configuration hash changed")
	}
	for _, restore := range []bool{false, true} {
		t.Run(fmt.Sprint("restore=", restore), func(t *testing.T) {
			dir := t.TempDir()
			before := c
			before.RestoreRequired = restore
			if err := atomicJSON(filepath.Join(dir, "config.json"), before); err != nil {
				t.Fatal(err)
			}
			after, err := prepareColdBootConfig(dir, before)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := readConfig(dir)
			if err != nil || stored.TartGuestCompatibility != !restore || after.TartGuestCompatibility != !restore {
				t.Fatal("wrong durable compatibility mode", stored, err)
			}
			if restore && configHash(stored) != oldHash {
				t.Fatal("changed the device graph of an existing snapshot")
			}
			if !restore && configHash(stored) == oldHash {
				t.Fatal("new device missing from snapshot compatibility hash")
			}
		})
	}
}

func TestColdBootCompatibilityRefusesUntrackedSnapshot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.vzvmsave"), []byte("saved state"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareColdBootConfig(dir, diskConfig{OS: "darwin"}); err == nil {
		t.Fatal("changed devices despite an untracked snapshot")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); !os.IsNotExist(err) {
		t.Fatal("wrote configuration before rejecting snapshot")
	}
	if c, err := prepareColdBootConfig(dir, diskConfig{OS: "linux"}); err != nil || c.TartGuestCompatibility {
		t.Fatal("changed Linux device graph", err)
	}
}

func TestControlAuthenticatesAndRestrictsOperations(t *testing.T) {
	for _, tc := range []struct {
		name, token, op, os, want string
		queued                    bool
	}{
		{"stop", "secret", "stop", "linux", "accepted\n", true},
		{"save", "secret", "suspend", "darwin", "accepted\n", true},
		{"wrong token", "incorrect", "stop", "linux", "", false},
		{"no token", "", "stop", "linux", "", false},
		{"unknown operation", "secret", "restart", "darwin", "", false},
		{"Linux save", "secret", "suspend", "linux", "unsupported\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			requests := make(chan string, 1)
			go handleControl(server, "secret", tc.os, requests)
			client.SetDeadline(time.Now().Add(time.Second))
			if err := json.NewEncoder(client).Encode(controlRequest{Token: tc.token, Operation: tc.op}); err != nil {
				t.Fatal(err)
			}
			response, err := io.ReadAll(client)
			if err != nil || string(response) != tc.want {
				t.Fatal(string(response), err)
			}
			if (len(requests) == 1) != tc.queued {
				t.Fatal("unauthorized or missing operation", len(requests))
			}
		})
	}
}
func TestStartDoesNotReplayDispatchedVM(t *testing.T) {
	s, dir := fixture(t)
	// fixture includes the durable start fence, even though no process is alive.
	if _, err := s.Start("source"); err == nil || !strings.Contains(err.Error(), "already dispatched") {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pulse.run.log")); !os.IsNotExist(err) {
		t.Fatal("start spawned before checking fence")
	}
}
func TestRunnerProtocolRejectsMissingHook(t *testing.T) {
	s := &Store{Runner: "/usr/bin/true", Timeout: time.Second}
	if err := s.Info(context.Background()); err == nil || !strings.Contains(err.Error(), "RunnerMain") {
		t.Fatal(err)
	}
}
func TestRunnerOutputBound(t *testing.T) {
	var output runnerOutput
	data := []byte(strings.Repeat("x", 32768))
	n, err := output.Write(data)
	if n != len(data) || err != nil || output.Len() != 16384 {
		t.Fatal(n, err, output.Len())
	}
}
