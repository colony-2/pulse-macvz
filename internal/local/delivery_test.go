package local

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func waitDelivery(t *testing.T, b *Client, name string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		active := b.workers[name]
		b.mu.Unlock()
		if !active {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("delivery worker did not finish")
}

func TestUndispatchedDeliveryExpiresAcrossRestartAndIsStoppedBeforeDeletion(t *testing.T) {
	b, m, spec := fixture(t)
	b.cfg.DeliveryTimeout = 30 * time.Millisecond
	b.command = func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] == "ip" {
			return nil, syscall.EHOSTUNREACH
		}
		return m.run(ctx, args...)
	}
	if _, err := b.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	waitDelivery(t, b, spec.Name)
	obs, _ := b.DeliveryObservations(t.Context())
	if len(obs) != 1 || !obs[0].Expired || obs[0].ExecutionPossible || obs[0].Cause != "EHOSTUNREACH" {
		t.Fatal(obs)
	}
	if err := b.DeliveryHealth(t.Context()); !errors.Is(err, ErrDeliveryUnavailable) {
		t.Fatal(err)
	}
	b = reopenProperties(t, b, m, "")
	if err := b.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	v, err := b.Get(t.Context(), spec.Name)
	if err != nil || !v.Stopped() || v.Status != "failed" {
		t.Fatal(v, err)
	}
	if err = b.Delete(t.Context(), spec.Name); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryDeadlineBoundsBlockedIPDiscovery(t *testing.T) {
	b, m, spec := fixture(t)
	b.cfg.DeliveryTimeout = 30 * time.Millisecond
	b.command = func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] == "ip" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return m.run(ctx, args...)
	}
	if _, err := b.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	waitDelivery(t, b, spec.Name)
	obs, err := b.DeliveryObservations(t.Context())
	if err != nil || len(obs) != 1 || !obs[0].Expired || obs[0].ExecutionPossible {
		t.Fatal(obs, err)
	}
}

func TestDeliverySetupDeadlineDoesNotLimitExecutingScript(t *testing.T) {
	b, _, spec := fixture(t)
	if err := os.MkdirAll(b.path(spec.Name), 0700); err != nil {
		t.Fatal(err)
	}
	_, private, _ := ed25519.GenerateKey(nil)
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	server := &ssh.ServerConfig{NoClientAuth: true}
	server.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	setup, cancelSetup := context.WithCancel(ctx)
	defer cancelSetup()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		defer stop()
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
				if req.Type != "exec" {
					_ = req.Reply(false, nil)
					continue
				}
				_ = req.Reply(true, nil)
				_, _ = io.Copy(io.Discard, ch)
				// Simulate setup deadline expiry after the exec request.
				cancelSetup()
				_, _ = io.WriteString(ch, "PULSE_HANDOFF_OK\n")
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				return
			}
		}
	}()
	if err := b.sendAddressHooks(ctx, setup, listener.Addr().String(), spec, ssh.MarshalAuthorizedKey(signer.PublicKey()),
		func(string) error { return nil }, func() {}); err != nil {
		t.Fatal(err)
	}
	<-done
}

func TestPossibleGuestExecutionIsNeverStoppedOrReplayedOnRestart(t *testing.T) {
	b, m, spec := fixture(t)
	if _, err := b.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	b.Close()
	r := b.records[spec.Name]
	r.Delivery.ExecutionPossible = true
	r.Delivery.StartedAt = time.Now().Add(-time.Hour)
	r.Delivery.Stage = "guest_exec"
	if err := b.save(r); err != nil {
		t.Fatal(err)
	}
	b = reopenProperties(t, b, m, "")
	if err := b.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitDelivery(t, b, spec.Name)
	if err := b.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitDelivery(t, b, spec.Name)
	if err := b.AdmissionHealth(t.Context()); !errors.Is(err, ErrDeliveryUnavailable) {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, call := range m.calls {
		if call[0] == "stop" || call[0] == "delete" {
			t.Fatal("unsafe recovery", call)
		}
	}
}

func TestSuccessfulSetupClearsPreviousAttemptFailure(t *testing.T) {
	b, m, spec := fixture(t)
	if _, err := b.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	b.Close()
	b = reopenProperties(t, b, m, "")
	r := b.records[spec.Name]
	r.Delivery.StartedAt = time.Now()
	r.Delivery.Attempts = 2
	r.Delivery.Cause = "timeout"
	r.Delivery.ExecutionPossible = false
	if err := b.deliveryStage(r, "guest_exec"); err != nil {
		t.Fatal(err)
	}
	if err := b.DeliveryHealth(t.Context()); err != nil {
		t.Fatalf("successful retry inherited the earlier setup failure: %v", err)
	}
	data, err := os.ReadFile(b.artifact(spec.Name, "json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved vmFile
	if err := json.Unmarshal(data, &saved); err != nil || !saved.Native.Delivery.ExecutionPossible || saved.Native.Delivery.Cause != "" {
		t.Fatal("successful setup did not persist its execution fence and cleared failure", err)
	}
	b.deliveryFailed(r, io.EOF)
	if err := b.DeliveryHealth(t.Context()); !errors.Is(err, ErrDeliveryUnavailable) {
		t.Fatal("a new failure after dispatch must still be uncertain", err)
	}
}

func TestHandoffMarkerUpdatesDurableStateBeforeSSHCommandExits(t *testing.T) {
	b, _, spec := fixture(t)
	if _, err := b.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	b.Close()
	r := b.records[spec.Name]
	var log bytes.Buffer
	w := deliveryWriter{destination: &log, delivered: func() { b.mu.Lock(); defer b.mu.Unlock(); b.deliverySucceeded(r) }}
	w.Write([]byte("PULSE_HAND"))
	w.Write([]byte("OFF_OK\n"))
	data, err := os.ReadFile(b.artifact(spec.Name, "json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved vmFile
	if err = json.Unmarshal(data, &saved); err != nil || !saved.Native.Delivered || saved.Native.Delivery.Stage != "delivered" {
		t.Fatal(err, string(data))
	}
}

func TestDeliveryDiagnosticsDoNotExposePeerTextOrCredentials(t *testing.T) {
	b, _, spec := fixture(t)
	if _, err := b.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	r := b.records[spec.Name]
	r.Delivery.Attempts = 3
	b.mu.Unlock()
	b.deliveryFailed(r, errors.New("SECRET_PASSWORD unable to authenticate SECRET_LEASE"))
	if err := b.AdmissionHealth(t.Context()); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal(err)
	}
	obs, _ := b.DeliveryObservations(t.Context())
	data, _ := json.Marshal(obs)
	if strings.Contains(string(data), "SECRET") {
		t.Fatal(string(data))
	}
}
