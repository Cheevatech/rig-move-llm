// Package tui is rig-move-llm's tiny, dependency-free terminal UI: ANSI colour plus
// an arrow-key/space-bar single-select, built on a hand-rolled termios raw mode (see
// raw_unix.go). It deliberately uses no TUI framework so the binary stays a single
// static stdlib build. When the terminal is not an interactive TTY, or raw mode is
// unsupported (Windows), every widget degrades to a colored numbered line prompt that
// still explains each option — so a human always understands what to pick, and a
// pipe/`-p` run never hangs.
package tui

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// stdin is a single shared reader for both raw key reads and line reads, so bytes
// buffered during a raw Select are never lost when a later step reads a line.
var stdin = bufio.NewReader(os.Stdin)

// useColor is decided once: honour NO_COLOR and only colour a real terminal.
var useColor = detectColor()

func detectColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return isCharDevice(os.Stdout)
}

func isCharDevice(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// IsInteractive reports whether we can drive an interactive raw-mode widget: stdin
// and stdout are both terminals and this build implements raw mode.
func IsInteractive() bool {
	return rawSupported && isCharDevice(os.Stdin) && isCharDevice(os.Stdout)
}

// --- colour helpers (no-op when useColor is false) -------------------------------

func paint(code, s string) string {
	if !useColor {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func bold(s string) string  { return paint("1", s) }
func dim(s string) string   { return paint("2", s) }
func cyan(s string) string  { return paint("36", s) }
func green(s string) string { return paint("32", s) }

// Option is one choice in a Select: a short label, a one-line explanation shown so
// the user understands what it does, and an optional "recommended" tag.
type Option struct {
	Label       string
	Description string
	Recommended bool
}

// Banner prints the oil-rig themed header. Colour only; safe on a dumb terminal.
func Banner() {
	rig := cyan("  ___\n |'''|\n |   |   ") + bold("rig-move-llm") + cyan("\n /|   |\\  ") + dim("move the heavy lifting off your paid LLM") + cyan("\n/_|___|_\\")
	fmt.Println(rig)
	fmt.Println()
}

// Select shows a single-choice menu and returns the chosen index. def is the
// pre-highlighted option. It returns -1 if the user aborts (Ctrl-C or q). Interactive
// terminals get arrow/j-k navigation with space or enter to choose; everything else
// gets a numbered line prompt.
func Select(title string, opts []Option, def int) int {
	if def < 0 || def >= len(opts) {
		def = 0
	}
	if !IsInteractive() {
		return selectLine(title, opts, def)
	}
	st, err := makeRaw(os.Stdin)
	if err != nil {
		return selectLine(title, opts, def)
	}
	defer st.restore()

	fmt.Print("\033[?25l")       // hide cursor
	defer fmt.Print("\033[?25h") // show cursor
	fmt.Print("\0337")           // save cursor position

	cursor := def
	for {
		renderSelect(title, opts, cursor)
		switch readKey() {
		case keyUp:
			cursor = (cursor - 1 + len(opts)) % len(opts)
		case keyDown:
			cursor = (cursor + 1) % len(opts)
		case keyEnter, keySpace:
			fmt.Println()
			return cursor
		case keyQuit:
			fmt.Println()
			return -1
		}
	}
}

// renderSelect redraws the menu in place: restore to the saved cursor, clear to end
// of screen, then paint the frame. Variable-height descriptions are handled by the
// clear-to-end each frame.
func renderSelect(title string, opts []Option, cursor int) {
	fmt.Print("\0338")   // restore to saved position
	fmt.Print("\033[0J") // clear from here to end of screen

	var b strings.Builder
	fmt.Fprintf(&b, "%s   %s\n", bold(title), dim("↑↓ move · space/enter select · q cancel"))
	for i, o := range opts {
		marker, label := "  "+dim("◯"), o.Label
		if i == cursor {
			marker, label = cyan("▸ ")+cyan("◉"), bold(o.Label)
		}
		line := fmt.Sprintf("  %s  %s", marker, label)
		if o.Description != "" {
			line += dim("  — " + o.Description)
		}
		if o.Recommended {
			line += "  " + green("(recommended)")
		}
		b.WriteString(line + "\n")
	}
	fmt.Print(b.String())
}

// selectLine is the non-interactive fallback: a numbered, colored list read from a
// single line. Empty input picks the default. Each option still shows its
// explanation so the choice is informed.
func selectLine(title string, opts []Option, def int) int {
	fmt.Println(bold(title))
	for i, o := range opts {
		line := fmt.Sprintf("  %d) %s", i+1, o.Label)
		if o.Description != "" {
			line += dim("  — " + o.Description)
		}
		if o.Recommended {
			line += "  " + green("(recommended)")
		}
		fmt.Println(line)
	}
	fmt.Printf("  choose [1-%d] (%d): ", len(opts), def+1)
	line, _ := stdin.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(opts) {
		return n - 1
	}
	return def
}

// Confirm is a yes/no Select. def picks the highlighted answer.
func Confirm(title, yesDesc, noDesc string, def bool) bool {
	opts := []Option{
		{Label: "Yes", Description: yesDesc},
		{Label: "No", Description: noDesc},
	}
	d := 0
	if !def {
		d = 1
	}
	return Select(title, opts, d) == 0
}

// Prompt reads a free-text line, showing the label, an optional explanation, and the
// default in brackets. Empty input returns def. Used for values a menu cannot capture
// (URLs, model names, keys).
func Prompt(label, desc, def string) string {
	if desc != "" {
		fmt.Println(dim("  " + desc))
	}
	if def != "" {
		fmt.Printf("  %s [%s]: ", bold(label), def)
	} else {
		fmt.Printf("  %s: ", bold(label))
	}
	line, err := stdin.ReadString('\n')
	if err != nil && line == "" {
		return def
	}
	if s := strings.TrimSpace(line); s != "" {
		return s
	}
	return def
}

// --- key decoding ----------------------------------------------------------------

type key int

const (
	keyNone key = iota
	keyUp
	keyDown
	keyEnter
	keySpace
	keyQuit
)

// readKey reads one logical keypress in raw mode. Arrow keys arrive as a 3-byte CSI
// sequence (ESC [ A/B) delivered together, so after an ESC we only consume more bytes
// when the terminal already buffered them — a lone ESC is treated as cancel.
func readKey() key {
	b, err := stdin.ReadByte()
	if err != nil {
		return keyQuit
	}
	switch b {
	case '\r', '\n':
		return keyEnter
	case ' ':
		return keySpace
	case 'j':
		return keyDown
	case 'k':
		return keyUp
	case 'q', 3: // q or Ctrl-C
		return keyQuit
	case 27: // ESC — possibly the start of an arrow-key CSI sequence
		if stdin.Buffered() < 2 {
			return keyQuit
		}
		b1, _ := stdin.ReadByte()
		b2, _ := stdin.ReadByte()
		if b1 == '[' {
			switch b2 {
			case 'A':
				return keyUp
			case 'B':
				return keyDown
			}
		}
	}
	return keyNone
}
