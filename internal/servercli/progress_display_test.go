package servercli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func displayRows() []progressRow {
	return []progressRow{
		{section: "Slots", name: "standard-0001", detail: "waiting for base"},
		{section: "Base images", name: "macos-26-arm64", detail: "downloading 10 / 100 MiB"},
	}
}

func TestInteractiveProgressRewritesOnlyChangedLines(t *testing.T) {
	p := &preparationReporter{interactive: true, terminalSize: func() (int, int, error) { return 100, 30, nil }}
	var out bytes.Buffer
	rows := displayRows()
	if err := p.render(&out, rows, time.Second, true); err != nil {
		t.Fatal(err)
	}
	initial := out.String()
	if strings.Index(initial, "Base images") > strings.Index(initial, "Slots") {
		t.Fatal("sections out of order", initial)
	}
	if strings.Contains(initial, "\x1b") {
		t.Fatal("initial display moved over unrelated output")
	}
	out.Reset()
	rows[1].detail = "ready"
	oldFrame := append([]string{}, p.frame...)
	if err := p.render(&out, rows, 2*time.Second, false); err != nil {
		t.Fatal(err)
	}
	update := out.String()
	if strings.Contains(update, "standard-0001") || strings.Contains(update, "Base images") {
		t.Fatal("unchanged rows were rewritten", update)
	}
	for i, line := range p.frame {
		if line == oldFrame[i] {
			continue
		}
		distance := len(p.frame) - i
		want := fmt.Sprintf("\x1b[%dA\r\x1b[2K%s\x1b[%dB\r", distance, line, distance)
		if !strings.Contains(update, want) {
			t.Fatalf("incorrect cursor movement/clearing: %q missing %q", update, want)
		}
	}
	if !strings.Contains(update, "macos-26-arm64  ready") {
		t.Fatal("shorter replacement missing", update)
	}
	out.Reset()
	if err := p.render(&out, rows, 2*time.Second, true); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatal("identical final display was repeated", out.String())
	}
}

func TestInteractiveProgressHandlesChangingHeightAndResize(t *testing.T) {
	columns, height := 100, 30
	p := &preparationReporter{interactive: true, terminalSize: func() (int, int, error) { return columns, height, nil }}
	var out bytes.Buffer
	rows := displayRows()
	if err := p.render(&out, rows, 0, true); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	rows = append(rows, progressRow{section: "Slots", name: "second", detail: "ready"})
	if err := p.render(&out, rows, time.Second, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "\r\x1b[J") {
		t.Fatal("changed block height did not clear old block", out.String())
	}
	out.Reset()
	columns = 40
	if err := p.render(&out, rows, 2*time.Second, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Fatal("resize rewrote potentially reflowed history")
	}
	for _, line := range p.frame {
		cells := 0
		for _, r := range line {
			cells += progressRuneWidth(r)
		}
		if cells >= columns {
			t.Fatal("line can trigger terminal autowrap", line)
		}
	}
	out.Reset()
	height = 3
	if err := p.render(&out, rows, 3*time.Second, false); err != nil {
		t.Fatal(err)
	}
	if p.interactive || strings.Contains(out.String(), "\x1b") || !strings.Contains(out.String(), "second") {
		t.Fatal("small terminal lost rows instead of falling back", out.String())
	}
}

func TestLinearProgressReportsChangesWithoutCursorControls(t *testing.T) {
	p := newPreparationReporter(&bytes.Buffer{}, "linear")
	var out bytes.Buffer
	rows := displayRows()
	if err := p.render(&out, rows, 0, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Base images\n") || !strings.Contains(out.String(), "Slots\n") {
		t.Fatal(out.String())
	}
	out.Reset()
	rows[1].detail = "downloaded 20 / 100 MiB"
	if err := p.render(&out, rows, time.Second, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b") || strings.Contains(out.String(), "standard-0001") || strings.Contains(out.String(), "Slots\n") {
		t.Fatal("linear update repeats unchanged section", out.String())
	}
	if !strings.Contains(out.String(), "20 / 100 MiB") {
		t.Fatal(out.String())
	}
	out.Reset()
	if err := p.render(&out, rows, 2*time.Second, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No state changes") {
		t.Fatal("missing liveness heartbeat")
	}
}

func TestRedirectedProgressIsAlwaysLinear(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, mode := range []string{"auto", "interactive", "linear"} {
		if p := newPreparationReporter(f, mode); p.interactive {
			t.Fatal("cursor controls enabled for redirected output", mode)
		}
	}
}

func TestProgressWrapPreservesWideTextAndRemovesControls(t *testing.T) {
	for _, line := range wrapProgressLine("  日本語 e\u0301 image\x1b[0m with a long description", 20) {
		cells := 0
		for _, r := range line {
			cells += progressRuneWidth(r)
		}
		if cells > 20 || strings.Contains(line, "\x1b") {
			t.Fatalf("unsafe terminal line %q", line)
		}
	}
}

type failingProgressWriter struct{}

func (failingProgressWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestProgressOutputErrorsAreReturned(t *testing.T) {
	p := &preparationReporter{}
	if err := p.render(failingProgressWriter{}, displayRows(), 0, true); err == nil {
		t.Fatal("write failure hidden")
	}
}

func TestInvalidProgressModeNeverOpensProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cmd := providerCommand("setup", "test", func(context.Context, serverOptions) error {
		t.Fatal("invalid display mode reached provider")
		return nil
	})
	cmd.SetArgs([]string{"--progress", "typo"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--progress") {
		t.Fatal(err)
	}
}
