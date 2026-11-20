package local

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// A legacy launch agent must identify the app it belongs to. Only derive this
// from the runner's own bundle, never from an unrelated installed application.
func endpointBundleIdentifier(ctx context.Context, executable string) (string, error) {
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	macos := filepath.Dir(resolved)
	contents := filepath.Dir(macos)
	app := filepath.Dir(contents)
	if filepath.Base(macos) != "MacOS" || filepath.Base(contents) != "Contents" || !strings.HasSuffix(app, ".app") {
		return "", nil
	}
	// plutil handles both XML and binary Info.plist files from embedding hosts.
	data, err := exec.CommandContext(ctx, "/usr/bin/plutil", "-extract", "CFBundleIdentifier", "raw", "-o", "-", filepath.Join(contents, "Info.plist")).Output()
	if err != nil {
		return "", fmt.Errorf("read VZ runner bundle identity: %w", err)
	}
	id := strings.TrimSpace(string(data))
	if id == "" || strings.ContainsAny(id, "\r\n\t ") {
		return "", fmt.Errorf("invalid VZ runner bundle identifier")
	}
	return id, nil
}
