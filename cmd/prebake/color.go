package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
)

// palette paints text with ANSI colors, or leaves it alone when color is off.
type palette struct{ on bool }

// paint colors a piece of running text.
func (p palette) paint(code, s string) string {
	if !p.on || s == "" || code == "" {
		return s
	}
	return code + s + ansiReset
}

// cell is one table cell: its visible text and an optional color.
type cell struct{ text, code string }

// table lays rows out in columns separated by two spaces, padding by the visible width of the
// text, so escape sequences never disturb the alignment (text/tabwriter would count them).
// The last column is not padded.
func (p palette) table(rows [][]cell) string {
	var widths []int
	for _, r := range rows {
		for i, c := range r {
			if n := utf8.RuneCountInString(c.text); i >= len(widths) {
				widths = append(widths, n)
			} else if n > widths[i] {
				widths[i] = n
			}
		}
	}
	var b strings.Builder
	for _, r := range rows {
		for i, c := range r {
			b.WriteString(p.paint(c.code, c.text))
			if i < len(r)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(c.text)+2))
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// stateCode maps a job or repo state to its color: green is fine, yellow is in motion or
// waiting, red needs attention.
func stateCode(state string) string {
	switch state {
	case "ok", "idle":
		return ansiGreen
	case "queued", "building", "retrying", "starting", "waiting-for-deploy-key":
		return ansiYellow
	default: // parked, skipped, degraded, fetch-error, host-key-error, ...
		return ansiRed
	}
}

// isTerminal reports whether w is a character device (an interactive terminal).
var isTerminal = func(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// newPalette resolves --color: "always" and "never" are absolute; "auto" (the default)
// colors a terminal unless NO_COLOR is set (https://no-color.org) or TERM is "dumb".
func newPalette(mode string, w io.Writer) (palette, error) {
	switch mode {
	case "always":
		return palette{true}, nil
	case "never":
		return palette{false}, nil
	case "", "auto":
		if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
			return palette{false}, nil
		}
		return palette{isTerminal(w)}, nil
	}
	return palette{}, fmt.Errorf("--color %q: want auto, always or never", mode)
}
