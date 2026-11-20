package bridge

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBridgeHalfCloseAndCancellation(t *testing.T) {
	upstream, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	targetDone := make(chan string, 1)
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			targetDone <- err.Error()
			return
		}
		defer conn.Close()
		data, err := io.ReadAll(conn)
		if err != nil {
			targetDone <- err.Error()
			return
		}
		if string(data) != "encrypted SSH bytes" {
			targetDone <- "wrong payload"
			return
		}
		_, err = conn.Write([]byte("reply after EOF"))
		if err != nil {
			targetDone <- err.Error()
			return
		}
		targetDone <- ""
	}()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := filepath.Join(t.TempDir(), "ready.json")
	done := make(chan error, 1)
	go func() { done <- serve(ctx, listener, Config{Target: upstream.Addr().String(), Ready: ready}) }()
	var metadata struct {
		Port   int
		Target string
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if data, err := os.ReadFile(ready); err == nil {
			if err = json.Unmarshal(data, &metadata); err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("readiness not published")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if metadata.Target != upstream.Addr().String() || metadata.Port != listener.Addr().(*net.TCPAddr).Port {
		t.Fatalf("wrong readiness: %+v", metadata)
	}
	info, err := os.Stat(ready)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("readiness permissions: %v %v", info, err)
	}
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err = conn.Write([]byte("encrypted SSH bytes")); err != nil {
		t.Fatal(err)
	}
	if err = conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(conn)
	if err != nil || string(response) != "reply after EOF" {
		t.Fatalf("response=%q error=%v", response, err)
	}
	if err := <-targetDone; err != "" {
		t.Fatal(err)
	}
	// Leave another stream open and verify cancellation terminates serving.
	idle, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bridge did not stop")
	}
}
