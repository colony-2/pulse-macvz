//go:build darwin

// Install a completely signed app with one atomic rename. The caller verifies
// both bundles and owns the staging directory, including the retired app.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func install(source, destination string) error {
	if !filepath.IsAbs(source) || !filepath.IsAbs(destination) || filepath.Clean(source) == filepath.Clean(destination) {
		return fmt.Errorf("require distinct absolute staging and installation paths")
	}
	for _, path := range []string{source, destination} {
		info, err := os.Lstat(path)
		if path == destination && os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("not a real app directory: %s", path)
		}
	}
	if _, err := os.Lstat(destination); os.IsNotExist(err) {
		return unix.RenamexNp(source, destination, unix.RENAME_EXCL)
	}
	return unix.RenamexNp(source, destination, unix.RENAME_SWAP)
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: install-macos-app STAGED_APP INSTALLED_APP")
		os.Exit(2)
	}
	if err := install(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
