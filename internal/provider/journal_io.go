package provider

import (
	"encoding/json"
	"os"
)

// The journal keeps its existing write order. This narrow private boundary lets
// tests exercise failures even when the worker runs as root.
type journalFile interface {
	Name() string
	Write([]byte) (int, error)
	Sync() error
	Close() error
}
type journalDirectory interface {
	Sync() error
	Close() error
}
type journalIO struct {
	marshal func(any) ([]byte, error)
	create  func(string, string) (journalFile, error)
	rename  func(string, string) error
	open    func(string) (journalDirectory, error)
}

func defaultJournalIO() journalIO {
	return journalIO{
		marshal: json.Marshal,
		create:  func(dir, pattern string) (journalFile, error) { return os.CreateTemp(dir, pattern) },
		rename:  os.Rename,
		open:    func(dir string) (journalDirectory, error) { return os.Open(dir) },
	}
}
