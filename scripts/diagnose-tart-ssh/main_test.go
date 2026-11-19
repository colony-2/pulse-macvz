package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestReadOnlyProbeAndFailureStages(t *testing.T) {
	for _, mode := range []string{"success", "password", "key", "sudo"} {
		t.Run(mode, func(t *testing.T) {
			_, private, err := ed25519.GenerateKey(nil)
			if err != nil {
				t.Fatal(err)
			}
			signer, err := ssh.NewSignerFromKey(private)
			if err != nil {
				t.Fatal(err)
			}
			server := &ssh.ServerConfig{PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
				if meta.User() != "admin" || string(password) != "SECRET_PASSWORD" || mode == "password" {
					return nil, errors.New("SECRET_AUTH_ERROR")
				}
				return nil, nil
			}}
			server.AddHostKey(signer)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			received := make(chan string, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				cc, channels, requests, err := ssh.NewServerConn(conn, server)
				if err != nil {
					return
				}
				defer cc.Close()
				go ssh.DiscardRequests(requests)
				for next := range channels {
					ch, requests, err := next.Accept()
					if err != nil {
						return
					}
					defer ch.Close()
					for req := range requests {
						var command struct{ Command string }
						if req.Type != "exec" || ssh.Unmarshal(req.Payload, &command) != nil || command.Command != "/bin/bash -s" {
							_ = req.Reply(false, nil)
							continue
						}
						_ = req.Reply(true, nil)
						data, _ := io.ReadAll(ch)
						received <- string(data)
						_, _ = io.WriteString(ch, "SECRET_STDOUT\nPULSE_DELIVERY_PROBE_OK\n")
						_, _ = io.WriteString(ch.Stderr(), "SECRET_STDERR\n")
						code := uint32(0)
						if mode == "sudo" {
							code = 1
						}
						_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
						return
					}
				}
			}()
			key := ssh.MarshalAuthorizedKey(signer.PublicKey())
			if mode == "key" {
				_, other, _ := ed25519.GenerateKey(nil)
				otherSigner, _ := ssh.NewSignerFromKey(other)
				key = ssh.MarshalAuthorizedKey(otherSigner.PublicKey())
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			out := probe(ctx, request{Address: listener.Addr().String(), Username: "admin", Password: "SECRET_PASSWORD", HostKey: key})
			want := map[string]string{"success": "", "password": "authentication_failed", "key": "host_key_mismatch", "sudo": "remote_exit_nonzero"}[mode]
			if out.Cause != want {
				t.Fatalf("got %+v, want %s", out, want)
			}
			if mode == "password" || mode == "key" {
				if out.Stage != "ssh_handshake" {
					t.Fatal(out)
				}
			} else {
				if out.Stage != "shell_and_sudo" {
					t.Fatal(out)
				}
				select {
				case script := <-received:
					if script != probeScript || strings.Contains(script, "c2j") {
						t.Fatal("unexpected remote script")
					}
				case <-ctx.Done():
					t.Fatal("no probe received")
				}
			}
			data, _ := json.Marshal(out)
			if strings.Contains(string(data), "SECRET_") {
				t.Fatal("private output escaped")
			}
		})
	}
}

func TestHandshakeCancellationAndBoundedOutput(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			<-done
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	out := probe(ctx, request{Address: listener.Addr().String(), Username: "admin"})
	if out.Outcome != "failed" || out.Stage != "ssh_handshake" || time.Since(start) > time.Second {
		t.Fatal(out)
	}
	var w markerWriter
	_, _ = w.Write([]byte(strings.Repeat("x", 1<<20) + "PULSE_DELIVERY_"))
	_, _ = w.Write([]byte("PROBE_OK\n"))
	if !w.found || len(w.tail) > 64 {
		t.Fatal("unbounded or missing marker")
	}
}
