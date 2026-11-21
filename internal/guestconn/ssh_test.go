package guestconn

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestCancellationClosesBlockedSSHHandshake(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	local, remote := net.Pipe()
	defer remote.Close()
	dialed := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := Exec(ctx, func(context.Context) (net.Conn, error) { close(dialed); return local, nil }, "user", "password", nil, []byte("private lease"))
		done <- err
	}()
	<-dialed
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, Err) || !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "private lease") {
			t.Fatalf("missing cause or leaked script: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SSH handshake ignored cancellation")
	}
}

func TestDiagnosticDialAndHostKeyCauses(t *testing.T) {
	raw := errors.New("dial fixture\n\"original cause\"")
	_, e := Exec(t.Context(), func(context.Context) (net.Conn, error) { return nil, raw }, "fixture", "fixture", nil, []byte("PRIVATE-SCRIPT"))
	if !errors.Is(e, raw) || !errors.Is(e, Err) || !strings.Contains(e.Error(), raw.Error()) {
		t.Fatal(e)
	}
	local, remote := net.Pipe()
	defer remote.Close()
	_, e = Exec(t.Context(), func(context.Context) (net.Conn, error) { return local, nil }, "fixture", "fixture", []byte("not-an-authorized-key"), []byte("PRIVATE-SCRIPT"))
	if e == nil || !strings.Contains(e.Error(), "parse host key") || strings.Contains(e.Error(), "PRIVATE-SCRIPT") {
		t.Fatal(e)
	}
}

func TestDiagnosticHostVerificationCause(t *testing.T) {
	_, private, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	signer, e := ssh.NewSignerFromKey(private)
	if e != nil {
		t.Fatal(e)
	}
	_, other, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	wrong, e := ssh.NewSignerFromKey(other)
	if e != nil {
		t.Fatal(e)
	}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, e := listener.Accept()
		if e != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		ssh.NewServerConn(conn, config)
	}()
	_, e = Exec(t.Context(), func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	}, "fixture", "fixture", ssh.MarshalAuthorizedKey(wrong.PublicKey()), []byte("PRIVATE-SCRIPT"))
	if !errors.Is(e, Err) || !strings.Contains(e.Error(), "host key mismatch") || strings.Contains(e.Error(), "PRIVATE-SCRIPT") {
		t.Fatal(e)
	}
	<-done
}
