package servercli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"

	"golang.org/x/term"
	"golang.org/x/text/width"
)

func newPreparationReporter(w io.Writer, mode string) *preparationReporter {
	p := &preparationReporter{}
	f, ok := w.(interface{ Fd() uintptr })
	if mode == "linear" || !ok || os.Getenv("TERM") == "dumb" || !term.IsTerminal(int(f.Fd())) {
		return p
	}
	p.interactive = true
	p.terminalSize = func() (int, int, error) { return term.GetSize(int(f.Fd())) }
	return p
}

func progressLines(rows []progressRow, duration time.Duration) []string {
	lines := []string{"Preparation progress (elapsed " + elapsed(duration) + ")"}
	for _, section := range []string{"Base images", "Slots"} {
		lines = append(lines, "", section)
		count := 0
		for _, row := range rows {
			if row.section != section {
				continue
			}
			lines = append(lines, "  "+row.name+"  "+row.detail)
			count++
		}
		if count == 0 {
			lines = append(lines, "  None requested.")
		}
	}
	return lines
}

func (p *preparationReporter) render(w io.Writer, rows []progressRow, duration time.Duration, force bool) error {
	if p.interactive {
		columns, height, err := p.terminalSize()
		// Never move over content that the terminal may have reflowed on resize.
		if columns != p.width || height != p.height {
			p.frame = nil
		}
		p.width, p.height = columns, height
		var frame []string
		if err == nil && columns >= 20 {
			for _, line := range progressLines(rows, duration) {
				frame = append(frame, wrapProgressLine(line, columns-1)...)
			}
		}
		// Keep the entire owned block visible, with a line for the resting cursor.
		// Large pools and small terminals get complete linear output instead.
		if err == nil && len(frame) > 0 && len(frame) < height {
			var out strings.Builder
			if len(p.frame) == 0 {
				out.WriteString(strings.Join(frame, "\n") + "\n")
			} else if len(frame) != len(p.frame) {
				fmt.Fprintf(&out, "\x1b[%dA\r\x1b[J", len(p.frame))
				out.WriteString(strings.Join(frame, "\n") + "\n")
			} else {
				// The cursor rests below the block. Rewrite changed physical lines
				// only, clearing the previous text so shorter updates leave no tail.
				for i, line := range frame {
					if line == p.frame[i] {
						continue
					}
					distance := len(frame) - i
					fmt.Fprintf(&out, "\x1b[%dA\r\x1b[2K%s\x1b[%dB\r", distance, line, distance)
				}
			}
			if _, err := io.WriteString(w, out.String()); err != nil {
				return err
			}
			p.frame = frame
			return nil
		}
		p.interactive, p.frame, force = false, nil, true
	}
	return p.renderLinear(w, rows, duration, force)
}

func (p *preparationReporter) renderLinear(w io.Writer, rows []progressRow, duration time.Duration, force bool) error {
	var out strings.Builder
	fmt.Fprintf(&out, "Preparation progress (elapsed %s):\n", elapsed(duration))
	changes := 0
	next := map[string]string{}
	for _, section := range []string{"Base images", "Slots"} {
		header := false
		for _, row := range rows {
			if row.section != section {
				continue
			}
			key := row.section + "\x00" + row.name
			next[key] = row.detail
			if !force && p.previous != nil && p.previous[key] == row.detail {
				continue
			}
			if !header {
				fmt.Fprintln(&out, section)
				header = true
			}
			fmt.Fprintf(&out, "  %s  %s\n", cleanProgressText(row.name), cleanProgressText(row.detail))
			changes++
		}
		if force && !header {
			fmt.Fprintf(&out, "%s\n  None requested.\n", section)
		}
	}
	if changes == 0 && !force {
		fmt.Fprintln(&out, "  No state changes.")
	}
	if _, err := io.WriteString(w, out.String()); err != nil {
		return err
	}
	p.previous = next
	return nil
}

func cleanProgressText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

func progressRuneWidth(r rune) int {
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r) {
		return 0
	}
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	}
	return 1
}

// Explicit wrapping avoids terminal autowrap changing the cursor coordinates.
// Wide characters count as two cells; combining characters consume no cells.
func wrapProgressLine(s string, columns int) []string {
	runes := []rune(cleanProgressText(s))
	var lines []string
	for len(runes) > 0 {
		cells, end, space := 0, 0, -1
		for end < len(runes) && cells+progressRuneWidth(runes[end]) <= columns {
			cells += progressRuneWidth(runes[end])
			if runes[end] == ' ' && end > 4 {
				space = end
			}
			end++
		}
		if end == len(runes) {
			lines = append(lines, string(runes))
			break
		}
		if space > 0 {
			end = space
		}
		lines = append(lines, strings.TrimRight(string(runes[:end]), " "))
		runes = []rune("    " + strings.TrimLeft(string(runes[end:]), " "))
	}
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}
