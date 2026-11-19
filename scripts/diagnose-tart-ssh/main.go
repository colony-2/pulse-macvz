// Read-only SSH transport probe. Private connection settings arrive on stdin;
// only fixed stage/error labels leave on stdout. Never accepts a job script.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

type request struct {
	Address  string `json:"address"`
	Username string `json:"username"`
	Password string `json:"password"`
	HostKey  []byte `json:"host_key"`
}

type result struct {
	Stage         string `json:"stage"`
	Outcome       string `json:"outcome"`
	Cause         string `json:"cause,omitempty"`
	HostKeyPinned bool   `json:"host_key_pinned"`
}

const probeScript = "printf 'PULSE_DELIVERY_PROBE_OK\\n'\nsudo -n /usr/bin/true\n"

func category(err error) string {
	var network net.Error
	var exit *ssh.ExitError
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &network) && network.Timeout():
		return "timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "network_unreachable"
	case errors.Is(err, syscall.EPERM), errors.Is(err, syscall.EACCES):
		return "permission_denied"
	case errors.As(err, &exit):
		return "remote_exit_nonzero"
	}
	// Library messages may include arbitrary peer-supplied text. Export labels only.
	for _, item := range []struct{ needle, label string }{
		{"unable to authenticate", "authentication_failed"},
		{"no common algorithm", "algorithm_negotiation_failed"},
		{"host key mismatch", "host_key_mismatch"},
		{"unexpected packet", "unexpected_ssh_packet"},
		{"EOF", "connection_closed"},
	} {
		if strings.Contains(err.Error(), item.needle) {
			return item.label
		}
	}
	return "other_error"
}

func probe(ctx context.Context, req request) (out result) {
	out = result{Stage: "settings", Outcome: "failed", HostKeyPinned: len(req.HostKey) > 0}
	host, _, err := net.SplitHostPort(req.Address)
	if err != nil || net.ParseIP(host) == nil || req.Username == "" {
		out.Cause = "invalid_settings"
		return
	}
	fail := func(err error) result {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		out.Cause = category(err)
		return out
	}
	callback := ssh.InsecureIgnoreHostKey()
	if out.HostKeyPinned {
		key, _, _, _, err := ssh.ParseAuthorizedKey(req.HostKey)
		if err != nil {
			out.Cause = "invalid_host_key"
			return
		}
		callback = ssh.FixedHostKey(key)
	}
	// Match internal/tart.Client.sendAddress: password-only auth, key pinning,
	// TCP and handshake deadlines, NewSession, then /bin/bash -s over stdin.
	cfg := &ssh.ClientConfig{User: req.Username, Auth: []ssh.AuthMethod{ssh.Password(req.Password)},
		HostKeyCallback: callback, Timeout: 15 * time.Second}
	out.Stage = "tcp"
	conn, err := (&net.Dialer{Timeout: cfg.Timeout}).DialContext(ctx, "tcp", req.Address)
	if err != nil {
		return fail(err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(cfg.Timeout))
	out.Stage = "ssh_handshake"
	cc, ch, reqs, err := ssh.NewClientConn(conn, req.Address, cfg)
	if err != nil {
		return fail(err)
	}
	_ = conn.SetDeadline(time.Time{})
	client := ssh.NewClient(cc, ch, reqs)
	defer client.Close()
	out.Stage = "ssh_session"
	session, err := client.NewSession()
	if err != nil {
		return fail(err)
	}
	defer session.Close()
	out.Stage = "shell_and_sudo"
	session.Stdin = strings.NewReader(probeScript)
	var stdout markerWriter
	session.Stdout, session.Stderr = &stdout, io.Discard
	if err = session.Run("/bin/bash -s"); err != nil {
		return fail(err)
	}
	if !stdout.found {
		out.Cause = "probe_marker_missing"
		return
	}
	out.Outcome = "succeeded"
	return
}

// Keep memory bounded even if the peer sends unrelated output.
type markerWriter struct {
	tail  []byte
	found bool
}

func (w *markerWriter) Write(p []byte) (int, error) {
	const marker = "PULSE_DELIVERY_PROBE_OK"
	for _, b := range p {
		w.tail = append(w.tail, b)
		if len(w.tail) > len(marker) {
			w.tail = w.tail[1:]
		}
		if bytes.Equal(w.tail, []byte(marker)) {
			w.found = true
		}
	}
	return len(p), nil
}

func main() {
	var req request
	out := result{Stage: "settings", Outcome: "failed", Cause: "invalid_settings"}
	if json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&req) == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out = probe(ctx, req)
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
}
