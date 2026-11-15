package provider

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type diagnosticJournalFile struct {
	journalFile
	operation string
	cause     error
}

func (f diagnosticJournalFile) Write(b []byte) (int, error) {
	if f.operation == "write" {
		return 0, f.cause
	}
	return f.journalFile.Write(b)
}
func (f diagnosticJournalFile) Sync() error {
	if f.operation == "sync file" {
		return f.cause
	}
	return f.journalFile.Sync()
}
func (f diagnosticJournalFile) Close() error {
	e := f.journalFile.Close()
	if f.operation == "close" {
		return f.cause
	}
	return e
}

type diagnosticJournalDirectory struct {
	journalDirectory
	cause error
}

func (d diagnosticJournalDirectory) Sync() error { return d.cause }
func diagnosticSaveFault(p *Provider, op string, cause error) {
	ops := defaultJournalIO()
	create := ops.create
	open := ops.open
	switch op {
	case "marshal":
		ops.marshal = func(any) ([]byte, error) { return nil, cause }
	case "create":
		ops.create = func(string, string) (journalFile, error) { return nil, cause }
	case "write", "sync file", "close":
		ops.create = func(d, n string) (journalFile, error) {
			f, e := create(d, n)
			return diagnosticJournalFile{f, op, cause}, e
		}
	case "rename":
		ops.rename = func(string, string) error { return cause }
	case "open directory":
		ops.open = func(string) (journalDirectory, error) { return nil, cause }
	case "sync directory":
		ops.open = func(d string) (journalDirectory, error) {
			f, e := open(d)
			return diagnosticJournalDirectory{f, cause}, e
		}
	}
	p.Store.io = &ops
}
func TestDiagnosticJournalSaveCauses(t *testing.T) {
	for _, op := range []string{"marshal", "create", "write", "sync file", "close", "rename", "open directory", "sync directory"} {
		t.Run(op, func(t *testing.T) {
			p, _ := fixture(t)
			raw := errors.New("journal fixture \"" + op + "\"\nraw cause")
			diagnosticSaveFault(p, op, raw)
			e := p.Store.save()
			if !errors.Is(e, ErrState) || !errors.Is(e, raw) || !strings.Contains(e.Error(), raw.Error()) {
				t.Fatal(e)
			}
			if op != "marshal" {
				if !p.Store.broken || !errors.Is(p.Store.save(), raw) {
					t.Fatal("broken cause lost")
				}
				if !errors.Is(p.Check(t.Context()), raw) {
					t.Fatal("Check lost broken cause")
				}
				if _, e := p.Images(); !errors.Is(e, raw) {
					t.Fatal(e)
				}
				if _, e := p.Slots(); !errors.Is(e, raw) {
					t.Fatal(e)
				}
			}
			b, _ := os.ReadFile(filepath.Join(p.Config.StateDir, "state.json"))
			if strings.Contains(string(b), raw.Error()) {
				t.Fatal("diagnostic persisted")
			}
		})
	}
}
func TestDiagnosticJournalOpenFailures(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	os.WriteFile(file, []byte("PRIVATE-STATE"), 0600)
	if _, e := OpenStore(filepath.Join(file, "child"), "scope"); !errors.Is(e, ErrState) || !errors.Is(e, os.ErrInvalid) && !strings.Contains(e.Error(), "not a directory") {
		t.Fatal(e)
	}
	for _, body := range []string{"{", `{"version":9,"key":"PRIVATE-STATE"}`} {
		t.Run(body[:1], func(t *testing.T) {
			d := t.TempDir()
			os.Chmod(d, 0700)
			os.WriteFile(filepath.Join(d, "state.json"), []byte(body), 0600)
			_, e := OpenStore(d, "scope")
			if !errors.Is(e, ErrState) || strings.Contains(e.Error(), "PRIVATE-STATE") {
				t.Fatal(e)
			}
		})
	}
	p, _ := fixture(t)
	_, e := OpenStore(p.Config.StateDir, p.Config.Scope())
	if !errors.Is(e, ErrState) || !strings.Contains(e.Error(), "lock journal") {
		t.Fatal(e)
	}
}

type diagnosticOrderedFile struct {
	journalFile
	calls *[]string
}

func (f diagnosticOrderedFile) Write(b []byte) (int, error) {
	*f.calls = append(*f.calls, "write")
	return f.journalFile.Write(b)
}
func (f diagnosticOrderedFile) Sync() error {
	*f.calls = append(*f.calls, "sync-file")
	return f.journalFile.Sync()
}
func (f diagnosticOrderedFile) Close() error {
	*f.calls = append(*f.calls, "close-file")
	return f.journalFile.Close()
}

type diagnosticOrderedDirectory struct {
	journalDirectory
	calls *[]string
}

func (d diagnosticOrderedDirectory) Sync() error {
	*d.calls = append(*d.calls, "sync-directory")
	return d.journalDirectory.Sync()
}
func (d diagnosticOrderedDirectory) Close() error {
	*d.calls = append(*d.calls, "close-directory")
	return d.journalDirectory.Close()
}
func TestDiagnosticJournalWriteOrder(t *testing.T) {
	p, _ := fixture(t)
	ops := defaultJournalIO()
	original := ops
	calls := []string{}
	ops.marshal = func(v any) ([]byte, error) { calls = append(calls, "marshal"); return original.marshal(v) }
	ops.create = func(d, n string) (journalFile, error) {
		calls = append(calls, "create")
		f, e := original.create(d, n)
		return diagnosticOrderedFile{f, &calls}, e
	}
	ops.rename = func(a, b string) error { calls = append(calls, "rename"); return original.rename(a, b) }
	ops.open = func(d string) (journalDirectory, error) {
		calls = append(calls, "open-directory")
		f, e := original.open(d)
		return diagnosticOrderedDirectory{f, &calls}, e
	}
	p.Store.io = &ops
	if e := p.Store.save(); e != nil {
		t.Fatal(e)
	}
	if got := strings.Join(calls, ","); got != "marshal,create,write,sync-file,close-file,rename,open-directory,sync-directory,close-directory" {
		t.Fatal(got)
	}
}
