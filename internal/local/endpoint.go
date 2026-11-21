package local

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (b *Client) endpointLabel(uid string) string {
	kind := b.kind
	if kind == "" {
		kind = "vz"
	}
	return "dev.pulse-" + kind + ".engine." + uid
}

// Production endpoints live with their engine VM. The fallback supports the
// VM-free launchd preflight only; it contains no pool or allocation state.
func (b *Client) endpointDir(uid string) string {
	for name, r := range b.records {
		if r.UID == uid {
			return filepath.Join(b.path(name), "pulse-endpoint")
		}
	}
	return filepath.Join(b.dir, "endpoints", uid)
}
func service(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/launchctl", args...)
	var output boundedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	return output.Bytes(), err
}
func serviceAbsent(err error) bool {
	var exited *exec.ExitError
	return errors.As(err, &exited) && exited.ExitCode() == 113
}

// bootout can finish asynchronously: print may briefly still find the service
// after an earlier removal, while another bootout reports ESRCH. Do not release
// its files or reuse its label until print confirms that it is actually absent.
func (b *Client) stopEndpointService(ctx context.Context, dir, target string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := b.service(ctx, "bootout", target)
	if err != nil {
		var exited *exec.ExitError
		if !serviceAbsent(err) && !(errors.As(err, &exited) && exited.ExitCode() == 3) {
			return endpointFailure(dir, "remove launchd service", output, err)
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return endpointFailure(dir, "wait for launchd removal", nil, err)
		}
		output, err := b.service(ctx, "print", target)
		if serviceAbsent(err) {
			return nil
		}
		if err != nil {
			return endpointFailure(dir, "confirm launchd removal", output, err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func endpointFailure(dir, stage string, output []byte, err error) error {
	if len(output) > 8192 {
		output = output[len(output)-8192:]
	}
	detail := fmt.Sprintf("%s: %v\n%s", stage, err, strings.TrimSpace(string(output)))
	_ = os.WriteFile(filepath.Join(dir, "last-error.log"), []byte(detail+"\n"), 0600)
	return fmt.Errorf("Docker endpoint %s: %w", detail, errors.Join(Err, err))
}
func (b *Client) Endpoint(ctx context.Context, name, uid string, port int) (bool, error) {
	if e := b.mu.LockContext(ctx); e != nil {
		return false, e
	}
	defer b.mu.Unlock()
	v, e := b.get(ctx, name)
	if e != nil || v.UID != uid || v.Stopped() {
		return false, failure("Docker endpoint engine identity/state unavailable", e)
	}
	data, e := b.hypervisor().IP(ctx, name)
	if e != nil {
		return false, fmt.Errorf("Docker endpoint engine IP unavailable: %w", e)
	}
	ip := strings.TrimSpace(string(data))
	if net.ParseIP(ip) == nil {
		return false, failure("Docker endpoint IP is invalid", nil)
	}
	ready, err := b.ensureEndpoint(ctx, uid, net.JoinHostPort(ip, "2222"), port)
	if err != nil || !ready || b.kind != "vz" {
		return ready, err
	}
	// A listening launchd agent can still be denied Local Network access to
	// the VM. Check the forwarded SSH banner before restoring the primary.
	if err := probeEngineEndpoint(ctx, port); err != nil {
		return false, fmt.Errorf("Docker endpoint cannot reach engine through launchd bridge: %w; inspect service.log and the runner's Local Network permission", err)
	}
	return true, nil
}

func probeEngineEndpoint(ctx context.Context, port int) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	conn.SetDeadline(deadline)
	banner, err := bufio.NewReader(io.LimitReader(conn, 256)).ReadString('\n')
	if err != nil {
		return fmt.Errorf("read engine SSH banner: %w", err)
	}
	if !strings.HasPrefix(banner, "SSH-2.0-") {
		return errors.New("forwarded endpoint did not return an SSH 2.0 banner")
	}
	return nil
}

// The caller has verified VM ownership. This supervisor also has a VM-free Mac
// preflight so launchd failures are caught before expensive image preparation.
func (b *Client) ensureEndpoint(ctx context.Context, uid, target string, port int) (bool, error) {
	dir := b.endpointDir(uid)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return false, e
	}
	ready := filepath.Join(dir, "ready.json")
	plist := filepath.Join(dir, "service.plist")
	var state struct {
		Port   int
		Target string
	}
	domain := "user/" + strconv.Itoa(os.Getuid())
	label := b.endpointLabel(uid)
	output, e := b.service(ctx, "print", domain+"/"+label)
	if e != nil && !serviceAbsent(e) {
		return false, endpointFailure(dir, "inspect launchd service", output, e)
	}
	if e == nil {
		bytes, err := os.ReadFile(ready)
		if err == nil && json.Unmarshal(bytes, &state) == nil && state.Port == port && state.Target == target {
			return true, nil
		}
		// Restart the owned helper when DHCP changes its target. Missing readiness
		// alone can mean launchd is still starting it; do not restart that case.
		// An existing helper with no plist at this location belongs to the old
		// state-directory layout (or lost its files). Recreate it beside the VM.
		_, plistErr := os.Stat(plist)
		if !os.IsNotExist(plistErr) && (err != nil || state.Target == "" || state.Target == target) {
			return false, nil
		}
		if e = b.stopEndpointService(ctx, dir, domain+"/"+label); e != nil {
			return false, e
		}
	}
	_ = os.Remove(ready)
	if b.endpointArgs == nil {
		return false, failure("VZ endpoint runner is not configured", nil)
	}
	args := b.endpointArgs(target, ready, port)
	var arr strings.Builder
	for _, a := range args {
		arr.WriteString("<string>" + xmlEscape(a) + "</string>")
	}
	association := ""
	if b.kind == "vz" && len(args) > 0 {
		id, err := endpointBundleIdentifier(ctx, args[0])
		if err != nil {
			return false, err
		}
		if id != "" {
			association = "<key>AssociatedBundleIdentifiers</key><array><string>" + xmlEscape(id) + "</string></array>"
		}
	}
	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>Label</key><string>%s</string><key>ProgramArguments</key><array>%s</array>%s<key>LimitLoadToSessionType</key><string>Background</string><key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>ThrottleInterval</key><integer>2</integer><key>StandardOutPath</key><string>%s</string><key>StandardErrorPath</key><string>%s</string></dict></plist>`, label, arr.String(), association, xmlEscape(filepath.Join(dir, "service.log")), xmlEscape(filepath.Join(dir, "service.log")))
	if e = os.WriteFile(plist, []byte(content), 0600); e != nil {
		return false, e
	}
	if output, e = b.service(ctx, "bootstrap", domain, plist); e != nil {
		return false, endpointFailure(dir, "bootstrap launchd service", output, e)
	}
	return false, nil
}
func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;").Replace(s)
}
func (b *Client) RemoveEndpoint(ctx context.Context, name, uid string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	domain := "user/" + strconv.Itoa(os.Getuid())
	label := b.endpointLabel(uid)
	dir := b.endpointDir(uid)
	output, err := b.service(ctx, "print", domain+"/"+label)
	if err == nil {
		if err = b.stopEndpointService(ctx, dir, domain+"/"+label); err != nil {
			return err
		}
	} else if !serviceAbsent(err) {
		return endpointFailure(dir, "inspect before removal", output, err)
	}
	return os.RemoveAll(dir)
}

// EndpointDiagnostics contains only endpoint configuration and bounded helper
// logs. Never include allocation journals, SSH keys or inherited environments.
func (b *Client) EndpointDiagnostics(ctx context.Context) (map[string]map[string]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	result := map[string]map[string]string{}
	paths := map[string]string{}
	for _, r := range b.records {
		dir := b.endpointDir(r.UID)
		if _, err := os.Stat(dir); err == nil {
			paths[r.UID] = dir
		}
	}
	entries, err := os.ReadDir(filepath.Join(b.dir, "endpoints"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			paths[entry.Name()] = filepath.Join(b.dir, "endpoints", entry.Name())
		}
	}
	for uid, dir := range paths {
		files := map[string]string{}
		for _, name := range []string{"service.plist", "ready.json", "service.log", "last-error.log"} {
			f, err := os.Open(filepath.Join(dir, name))
			if err != nil {
				continue
			}
			if info, err := f.Stat(); err == nil && info.Size() > 16384 {
				_, _ = f.Seek(-16384, io.SeekEnd)
			}
			content, _ := io.ReadAll(io.LimitReader(f, 16384))
			f.Close()
			files[name] = string(content)
		}
		output, err := b.service(ctx, "print", "user/"+strconv.Itoa(os.Getuid())+"/"+b.endpointLabel(uid))
		if err != nil {
			files["launchctl-status"] = fmt.Sprint(err)
		}
		// launchctl print can include inherited environment variables: retain only
		// process/lifecycle facts, never the full environment dump.
		for _, line := range strings.Split(string(output), "\n") {
			if !strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "\t\t") {
				continue
			}
			line = strings.TrimSpace(line)
			for _, prefix := range []string{"state = ", "pid = ", "runs = ", "last exit code = ", "last terminating signal = "} {
				if strings.HasPrefix(line, prefix) {
					files["launchctl-status"] += "\n" + line
					break
				}
			}
		}
		result[uid] = files
	}
	return result, nil
}
