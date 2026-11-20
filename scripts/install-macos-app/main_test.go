//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallAndReplaceWholeBundle(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "stage")
	target := filepath.Join(root, "App.app")
	write := func(value string) {
		t.Helper()
		if err := os.Mkdir(source, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, "version"), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path, want string) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(path, "version"))
		if err != nil || string(got) != want {
			t.Fatal(string(got), want, err)
		}
	}
	write("old")
	if err := install(source, target); err != nil {
		t.Fatal(err)
	}
	read(target, "old")
	write("new")
	if err := install(source, target); err != nil {
		t.Fatal(err)
	}
	read(target, "new")
	read(source, "old")
	// An invalid staging path leaves the installed bundle intact.
	if err := install(filepath.Join(root, "missing"), target); err == nil {
		t.Fatal("missing source accepted")
	}
	read(target, "new")
	link := filepath.Join(root, "alias.app")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := install(source, link); err == nil {
		t.Fatal("destination symlink accepted")
	}
	read(target, "new")
}
