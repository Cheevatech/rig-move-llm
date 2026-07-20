package tui

import (
	"bufio"
	"strings"
	"testing"
)

// feed replaces the shared reader so a test can drive the line/key logic
// deterministically, independent of whether the test runs under a real TTY.
func feed(s string) { stdin = bufio.NewReader(strings.NewReader(s)) }

func TestSelectLineFallback(t *testing.T) {
	opts := []Option{{Label: "a"}, {Label: "b"}, {Label: "c"}}

	feed("2\n")
	if got := selectLine("pick", opts, 0); got != 1 {
		t.Errorf("numbered choice: got %d, want 1", got)
	}
	feed("\n")
	if got := selectLine("pick", opts, 2); got != 2 {
		t.Errorf("empty -> default: got %d, want 2", got)
	}
	feed("9\n")
	if got := selectLine("pick", opts, 0); got != 0 {
		t.Errorf("out-of-range -> default: got %d, want 0", got)
	}
	feed("nope\n")
	if got := selectLine("pick", opts, 1); got != 1 {
		t.Errorf("garbage -> default: got %d, want 1", got)
	}
}

func TestPromptFallback(t *testing.T) {
	feed("hello\n")
	if got := Prompt("label", "desc", "def"); got != "hello" {
		t.Errorf("got %q, want hello", got)
	}
	feed("\n")
	if got := Prompt("label", "", "def"); got != "def" {
		t.Errorf("empty -> default: got %q, want def", got)
	}
	feed("  spaced  \n")
	if got := Prompt("label", "", "def"); got != "spaced" {
		t.Errorf("trim: got %q, want spaced", got)
	}
}

func TestReadKeyDecoding(t *testing.T) {
	cases := []struct {
		in   string
		want key
	}{
		{"\x1b[A", keyUp},
		{"\x1b[B", keyDown},
		{"k", keyUp},
		{"j", keyDown},
		{"\r", keyEnter},
		{"\n", keyEnter},
		{" ", keySpace},
		{"q", keyQuit},
		{"\x03", keyQuit},   // Ctrl-C
		{"\x1b", keyQuit},   // lone ESC (nothing buffered after it)
		{"\x1b[C", keyNone}, // right arrow: recognised CSI, unmapped
	}
	for _, c := range cases {
		feed(c.in)
		if got := readKey(); got != c.want {
			t.Errorf("readKey(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
