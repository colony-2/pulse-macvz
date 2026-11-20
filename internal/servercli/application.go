package servercli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Register the actual runner's owning app, including when invoked by a PATH
// symlink. VM and bridge children keep that executable and signing identity.
func registerApplication(runner string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	var err error
	if runner == "" {
		runner, err = os.Executable()
	} else {
		runner, err = exec.LookPath(runner)
	}
	if err != nil {
		return err
	}
	runner, err = filepath.EvalSymlinks(runner)
	if err != nil {
		return err
	}
	macos := filepath.Dir(runner)
	contents := filepath.Dir(macos)
	app := filepath.Dir(contents)
	if filepath.Base(macos) != "MacOS" || filepath.Base(contents) != "Contents" || !strings.HasSuffix(app, ".app") {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister", "-f", app).CombinedOutput(); err != nil {
		return fmt.Errorf("register provider app for Docker Local Network access: %w: %s; run setup from an ordinary macOS terminal with access to LaunchServices", err, strings.TrimSpace(string(output)))
	}
	return nil
}
