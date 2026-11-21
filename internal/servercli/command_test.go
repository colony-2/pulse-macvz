package servercli

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
)

func TestCommandDefaultsAndFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, args := range [][]string{
		{"server"},
		{"server", "--mode", "inbound", "--backend", "vz", "--linux-capacity", "1", "--macos-capacity", "1", "--macos-docker-capacity", "1", "--total-capacity", "0"},
		{"server", "-m", "inbound", "-b", "vz", "-l", "1", "--macos-capacity", "1", "-d", "1", "-c", "0"},
	} {
		called := false
		cmd := newCommand(func(_ context.Context, o serverOptions) error {
			called = true
			if !filepath.IsAbs(o.config.StateDir) {
				t.Fatal("state directory is relative")
			}
			if len(args) == 1 {
				if o.mode != "outbound" || o.backend != "vz" || *o.config.VMCounts.MacOS != 2 || *o.config.VMCounts.Linux != 4 || *o.config.VMCounts.Total != 6 {
					t.Fatalf("wrong defaults: %+v", o.capacity)
				}
			} else {
				if o.mode != "inbound" || o.backend != "vz" || *o.config.VMCounts.Total != 0 {
					t.Fatal("flags not applied")
				}
			}
			return nil
		})
		testArgs := args
		if len(args) == 1 {
			testArgs = append([]string{"server"}, "--collector", "localhost:8443", "--tls-ca", "ca.pem", "--tls-cert", "cert.pem", "--tls-key", "key.pem")
		}
		cmd.SetArgs(testArgs)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if !called {
			t.Fatal("server not started")
		}
	}
}

func TestInvalidFlagsNeverStartProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, args := range [][]string{
		{"--mode", "typo"}, {"-mode", "inbound"}, {"--backend", "unknown"},
		{"--macos-capacity", "3"}, {"--linux-capacity", "-1"}, {"--macos-docker-capacity", "3"},
		{"--owner", "../escape"}, {"--backend", "orchard"},
		{"--macos-docker-capacity", "1", "--docker-worker-address", "not-an-ip"},
		{"--macos-docker-capacity", "2", "--docker-port-min", "45000", "--docker-port-max", "45000"},
		{"extra-positional"}, {"--rpc-concurrency", "0"}, {"--startup-timeout", "-1s"}, {"--max-message-bytes", "128"},
		{"--mode", "inbound", "--tls-key", "key.pem"}, {"--macos-base", "unknown"},
	} {
		cmd := newCommand(func(context.Context, serverOptions) error { t.Fatal("invalid options reached provider"); return nil })
		cmd.SetArgs(append([]string{"server"}, args...))
		if err := cmd.Execute(); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestHelpDoesNotStartProvider(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"server", "-h"}} {
		cmd := newCommand(func(context.Context, serverOptions) error { t.Fatal("help started provider"); return nil })
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if out.Len() == 0 {
			t.Fatal("empty help")
		}
	}
}
