package thin

import (
	"strings"
	"testing"
)

// The supervisor's argv is hand-parsed rather than flag-parsed, because
// everything after `--` belongs to the child and must survive verbatim —
// including things that look like our own flags. These pin that.
func TestParseSuperviseArgs(t *testing.T) {
	t.Run("everything after -- is the child's, verbatim", func(t *testing.T) {
		got, err := parseSuperviseArgs([]string{"--parent", "42", "--",
			"claude", "-p", "task", "--parent", "not-ours", "--"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Parent != 42 {
			t.Errorf("parent = %d, want 42", got.Parent)
		}
		want := []string{"claude", "-p", "task", "--parent", "not-ours", "--"}
		if strings.Join(got.Command, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("command = %v, want %v — a flag after -- was eaten", got.Command, want)
		}
	})

	t.Run("no command is an error, not a silent no-op", func(t *testing.T) {
		if _, err := parseSuperviseArgs([]string{"--parent", "42", "--"}); err == nil {
			t.Error("an empty command must be refused; supervising nothing looks like success")
		}
		if _, err := parseSuperviseArgs(nil); err == nil {
			t.Error("no arguments at all must be refused")
		}
	})

	t.Run("a bad parent pid is named", func(t *testing.T) {
		_, err := parseSuperviseArgs([]string{"--parent", "abc", "--", "claude"})
		if err == nil || !strings.Contains(err.Error(), "abc") {
			t.Errorf("err = %v, want it to quote the bad value", err)
		}
	})

	t.Run("an unexpected argument before -- is refused", func(t *testing.T) {
		if _, err := parseSuperviseArgs([]string{"--wat", "--", "claude"}); err == nil {
			t.Error("an unknown flag before -- must be refused, not silently dropped")
		}
	})

	t.Run("parent 0 disables the watchdog rather than watching pid 0", func(t *testing.T) {
		got, err := parseSuperviseArgs([]string{"--", "claude"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Parent != 0 {
			t.Errorf("parent = %d, want 0 when unset", got.Parent)
		}
	})
}
