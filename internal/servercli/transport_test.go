package servercli

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colony-2/pulse-macvz/internal/nologging"
	api "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse-provider-api/plugin"
)

type fixtureAPI struct{}

func TestSharedKeySources(t *testing.T) {
	for _, tc := range []struct {
		name, value, want string
		valid             bool
	}{
		{"exact", "  secret  ", "  secret  ", true}, {"newline", "secret\n", "secret", true}, {"crlf", "secret\r\n", "secret", true},
		{"empty", "", "", false}, {"multiple", "secret\nignored", "", false}, {"oversized", strings.Repeat("x", plugin.MaxMutualKeyBytes+1), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "key")
			if err := os.WriteFile(file, []byte(tc.value), 0600); err != nil {
				t.Fatal(err)
			}
			key, err := (transportOptions{auth: "mutual-key", sharedKeyFile: file}).mutualKey()
			if (err == nil) != tc.valid || tc.valid && string(key) != tc.want {
				t.Fatalf("key acquisition valid=%v error=%v", tc.valid, err)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("error exposed key")
			}
		})
	}
	t.Setenv("PULSE_TEST_KEY", " exact env value ")
	key, err := (transportOptions{auth: "mutual-key", sharedKeyEnv: "PULSE_TEST_KEY"}).mutualKey()
	if err != nil || string(key) != " exact env value " {
		t.Fatal("environment bytes changed", err)
	}
	t.Setenv("PULSE_TEST_KEY", "secret\n")
	if _, err := (transportOptions{auth: "mutual-key", sharedKeyEnv: "PULSE_TEST_KEY"}).mutualKey(); err == nil {
		t.Fatal("accepted newline in environment key")
	}
}

func TestReverseInvalidOptionsDoNotOpenProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, flags := range [][]string{
		{"--reverse-auth", "unknown"}, {"--reverse-transport", "http"},
		{"--reverse-auth", "mutual-key"},
		{"--reverse-auth", "mutual-key", "--shared-key-env", "KEY", "--shared-key-file", "key"},
		{"--reverse-auth", "mutual-key", "--shared-key-env", "KEY", "--tls-cert", "cert"},
		{"--reverse-auth", "mutual-key", "--shared-key-env", "KEY", "--reverse-transport", "http", "--tls-ca", "ca"},
		{"--reverse-auth", "mutual-key", "--shared-key-env", "KEY", "--reverse-transport", "http", "--tls-server-name", "host"},
		{"--mode", "inbound", "--reverse-auth", "mutual-key", "--shared-key-env", "KEY"},
	} {
		cmd := newCommand(func(context.Context, serverOptions) error { t.Fatal("invalid options reached provider"); return nil })
		cmd.SetErr(io.Discard)
		cmd.SetArgs(append([]string{"server", "--collector", "localhost:8443"}, flags...))
		if cmd.Execute() == nil {
			t.Fatal("accepted", flags)
		}
	}
}

func TestWrongSharedKeyNeverOpensRuntime(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PULSE_TEST_WRONG_KEY", "DO-NOT-PRINT-THIS-KEY")
	for _, mode := range []string{"tls", "http"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			hostTLS, certFlags := fixtureTrust(t)
			hostTLS.NextProtos = []string{plugin.ReverseKeyALPN}
			hostTLS.ClientAuth = tls.NoClientCert
			co := plugin.CollectorOptions{AuthMode: plugin.ReverseAuthMutualKey, MutualKeys: map[string][]byte{"worker": []byte("expected key")}, TLSConfig: hostTLS}
			flags := []string{"--tls-ca", certFlags[1], "--tls-server-name", "collector.test"}
			if mode == "http" {
				co.Transport = plugin.ReverseTransportHTTP
				co.TLSConfig = nil
				flags = nil
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			collector, err := plugin.StartCollector(ctx, listener, co)
			if err != nil {
				listener.Close()
				t.Fatal(err)
			}
			defer collector.Close()
			var opened atomic.Int32
			cmd := newCommand(func(ctx context.Context, o serverOptions) error {
				return runPlugin(ctx, o, func(serverOptions) (runtimeProvider, error) { opened.Add(1); return fixtureRuntime(), nil })
			})
			cmd.SetErr(io.Discard)
			cmd.SetArgs(append([]string{"server", "--collector", listener.Addr().String(), "--name", "worker", "--reverse-auth", "mutual-key", "--reverse-transport", mode, "--shared-key-env", "PULSE_TEST_WRONG_KEY", "--reconnect-attempts", "0"}, flags...))
			err = cmd.ExecuteContext(ctx)
			if err == nil || errors.Is(err, context.DeadlineExceeded) || opened.Load() != 0 {
				t.Fatalf("authentication did not reject before construction: %v opens=%d", err, opened.Load())
			}
			if strings.Contains(err.Error(), "DO-NOT-PRINT") {
				t.Fatal("error exposed key")
			}
		})
	}
}

func (fixtureAPI) GetCapabilities(context.Context) (api.Capabilities, error) {
	return api.Capabilities{BasesComplete: true, Bases: []api.BaseCapabilities{{Name: "fixture-arm64"}}}, nil
}
func (fixtureAPI) List(context.Context, api.ListRequest) (api.ListResponse, error) {
	return api.ListResponse{}, nil
}
func (fixtureAPI) Submit(_ context.Context, launches []api.Launch, options api.SubmitOptions) ([]api.Submission, api.LogSubscription, error) {
	out := make([]api.Submission, len(launches))
	for i, l := range launches {
		out[i] = api.Submission{LaunchID: l.LaunchID, Status: api.Unsupported, Reason: "fixture has no native executor"}
	}
	return out, nologging.Subscription(launches, options), nil
}

func fixtureRuntime() *fakeRuntime {
	return &fakeRuntime{Provider: fixtureAPI{}, run: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}
}

// Generate ephemeral test trust; never use machine credentials or real jobs.
func fixtureTrust(t *testing.T) (*tls.Config, []string) {
	t.Helper()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	write := func(name string, block *pem.Block) string {
		filename := filepath.Join(dir, name)
		if err := os.WriteFile(filename, pem.EncodeToMemory(block), 0600); err != nil {
			t.Fatal(err)
		}
		return filename
	}
	caFile := write("ca.pem", &pem.Block{Type: "CERTIFICATE", Bytes: der})
	pair := func(name string, serial int64, usage x509.ExtKeyUsage) (tls.Certificate, string, string) {
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		pk, err := x509.MarshalPKCS8PrivateKey(leafKey)
		if err != nil {
			t.Fatal(err)
		}
		certFile := write(name+".pem", &pem.Block{Type: "CERTIFICATE", Bytes: der})
		keyFile := write(name+".key", &pem.Block{Type: "PRIVATE KEY", Bytes: pk})
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			t.Fatal(err)
		}
		return cert, certFile, keyFile
	}
	host, _, _ := pair("collector.test", 2, x509.ExtKeyUsageServerAuth)
	_, certFile, keyFile := pair("worker", 3, x509.ExtKeyUsageClientAuth)
	return &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{plugin.ReverseALPN}, Certificates: []tls.Certificate{host}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert}, []string{"--tls-ca", caFile, "--tls-cert", certFile, "--tls-key", keyFile, "--tls-server-name", "collector.test"}
}

type faultListener struct {
	net.Listener
	mu     sync.Mutex
	latest net.Conn
}

func (l *faultListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.latest = c
		l.mu.Unlock()
	}
	return c, err
}
func (l *faultListener) disconnect() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.latest != nil {
		_ = l.latest.Close()
	}
}

func TestOutboundSDKReconnectAndCalls(t *testing.T) {
	for _, mode := range []string{"mtls", "mutual-key-tls", "mutual-key-http"} {
		t.Run(mode, func(t *testing.T) { testOutboundSDKReconnectAndCalls(t, mode) })
	}
}
func testOutboundSDKReconnectAndCalls(t *testing.T, mode string) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	hostTLS, flags := fixtureTrust(t)
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &faultListener{Listener: raw}
	collectorOptions := plugin.CollectorOptions{TLSConfig: hostTLS, Authorize: func(_ context.Context, state tls.ConnectionState) (string, error) {
		if len(state.VerifiedChains) == 0 || state.PeerCertificates[0].Subject.CommonName != "worker" {
			return "", errors.New("not authorized")
		}
		return "worker", nil
	}}
	if mode != "mtls" {
		collectorOptions.AuthMode = plugin.ReverseAuthMutualKey
		collectorOptions.Authorize = nil
		collectorOptions.MutualKeys = map[string][]byte{"worker": []byte(" fixture shared key ")}
		hostTLS.NextProtos = []string{plugin.ReverseKeyALPN}
		hostTLS.ClientAuth = tls.NoClientCert
		flags = []string{"--tls-ca", flags[1], "--tls-server-name", "collector.test", "--reverse-auth", "mutual-key"}
		if mode == "mutual-key-http" {
			collectorOptions.Transport = plugin.ReverseTransportHTTP
			collectorOptions.TLSConfig = nil
			keyFile := filepath.Join(t.TempDir(), "shared.key")
			if err := os.WriteFile(keyFile, []byte(" fixture shared key \r\n"), 0600); err != nil {
				t.Fatal(err)
			}
			flags = []string{"--reverse-auth", "mutual-key", "--reverse-transport", "http", "--shared-key-file", keyFile}
		} else {
			t.Setenv("PULSE_FIXTURE_SHARED_KEY", " fixture shared key ")
			flags = append(flags, "--shared-key-env", "PULSE_FIXTURE_SHARED_KEY")
		}
	}
	collector, err := plugin.StartCollector(ctx, listener, collectorOptions)
	if err != nil {
		raw.Close()
		t.Fatal(err)
	}
	defer collector.Close()
	var opened atomic.Int32
	var previous *fakeRuntime // factory calls are serialized by the SDK
	command := newCommand(func(ctx context.Context, o serverOptions) error {
		return runPlugin(ctx, o, func(serverOptions) (runtimeProvider, error) {
			if previous != nil && previous.closed.Load() != 1 {
				return nil, errors.New("reopened before closing previous runtime")
			}
			previous = fixtureRuntime()
			opened.Add(1)
			return previous, nil
		})
	})
	command.SetErr(io.Discard)
	command.SetArgs(append([]string{"server", "--collector", listener.Addr().String(), "--name", "worker", "--reconnect-initial-delay", "10ms", "--reconnect-max-delay", "10ms"}, flags...))
	done := make(chan error, 1)
	go func() { done <- command.ExecuteContext(ctx) }()
	waitProvider := func(after uint64) plugin.AvailableProvider {
		t.Helper()
		for {
			snap, err := collector.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range snap.Providers {
				if p.Generation > after {
					return p
				}
			}
			if _, err := collector.WaitForChange(ctx, snap.Revision); err != nil {
				t.Fatal(err)
			}
		}
	}
	first := waitProvider(0)
	caps, err := first.Provider.GetCapabilities(ctx)
	if err != nil || len(caps.Bases) != 1 || caps.Bases[0].Name != "fixture-arm64" {
		t.Fatalf("discovery: %v", err)
	}
	if _, err := first.Provider.List(ctx, api.ListRequest{}); err != nil {
		t.Fatal(err)
	}
	result, _, err := first.Provider.Submit(ctx, []api.Launch{{LaunchID: "fixture-launch", Allocation: api.Allocation{Base: "fixture-arm64", CPUMillis: 1000}, Job: api.JobRef{JobDB: "https://jobdb.invalid/test", InstanceID: "fixture", TenantID: "test", JobID: "job"}, Lease: api.Lease("fixture-only-not-a-credential")}}, api.SubmitOptions{})
	if err != nil || len(result) != 1 || result[0].Status != api.Unsupported {
		t.Fatalf("submit: %v %#v", err, result)
	}
	_, sub, err := first.Provider.Submit(ctx, []api.Launch{{LaunchID: "logs-unavailable", Allocation: api.Allocation{Base: "fixture-arm64"}, Job: api.JobRef{JobDB: "https://jobdb.invalid/test", InstanceID: "fixture", TenantID: "test", JobID: "job"}, Lease: api.Lease("fixture-only")}}, api.SubmitOptions{Listener: &api.ListenOptions{Context: ctx, OnLog: func(context.Context, api.LaunchLog) error { t.Error("unexpected log callback"); return nil }}})
	if err != nil || sub == nil {
		t.Fatalf("logging altered admission: %v", err)
	}
	logResult, logErr := sub.Wait(ctx)
	if logResult.Reason != api.ListenerSourceUnavailable || !errors.Is(logErr, api.ErrLogSource) || len(logResult.Coverage) != 4 {
		t.Fatalf("unavailable logging result: %+v %v", logResult, logErr)
	}
	listener.disconnect()
	second := waitProvider(first.Generation)
	if second.ID != first.ID || opened.Load() != 2 {
		t.Fatal("did not reconstruct provider on reconnect")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server shutdown timed out")
	}
	if previous.closed.Load() != 1 {
		t.Fatal("last runtime was not closed")
	}
}

func TestInboundChild(t *testing.T) {
	if os.Getenv("PULSE_CLI_TEST_CHILD") != "1" {
		return
	}
	command := newCommand(func(ctx context.Context, o serverOptions) error {
		return runPlugin(ctx, o, func(serverOptions) (runtimeProvider, error) { return fixtureRuntime(), nil })
	})
	command.SetArgs([]string{"server", "--mode", "inbound", "--backend", "vz"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestInboundSDKProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("upstream inbound SDK process safeguards are currently Linux-only")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	process, err := plugin.Start(t.Context(), plugin.StartOptions{Executable: executable, Args: []string{"-test.run=^TestInboundChild$"}, Env: map[string]string{"PULSE_CLI_TEST_CHILD": "1", "HOME": t.TempDir()}, Init: plugin.Initialization{Name: "worker", BaseDir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	defer process.Close()
	caps, err := process.GetCapabilities(t.Context())
	if err != nil || len(caps.Bases) != 1 {
		t.Fatalf("discovery: %v", err)
	}
	if err := process.Close(); err != nil {
		t.Fatal(err)
	}
}
