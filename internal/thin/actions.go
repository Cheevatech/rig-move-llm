package thin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ActionsFile is the human-readable log of what the worker did, one line per
// action, written as it happens. `rig watch` tails it.
//
// It is a SECOND log, next to the raw stream, and the split is the point. The
// raw stream is for digging afterwards — it is what A1 was reconstructed from —
// and it is unreadable live: thousands of thinking-token events per run, ~80% of
// everything the worker generates. Tailing it tells you nothing.
//
// The question this file answers is narrower and is the one that actually costs
// money: for 20–50 minutes, is it working or is it stuck? A1's worst case ran 48
// minutes and produced 75,900 tokens of degenerate repetition AFTER the work was
// done and the tests were green. With a line of action flowing, that is visible
// as "nothing since 17:24" and killable in the second minute.
const ActionsFile = "actions.log"

// actionLog appends one line per action. It is written from the stream parser,
// which is single-threaded per run, but the mutex is kept because the final
// status line is written from Run after the parser has returned.
type actionLog struct {
	mu    sync.Mutex
	f     *os.File
	start time.Time
	// pending maps a tool_use id to its short label, so a result can be attributed
	// to the call it answers rather than to whatever ran last.
	pending map[string]string
}

func newActionLog(dir, repo, task string) *actionLog {
	f, err := os.OpenFile(filepath.Join(dir, ActionsFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		logf("could not open %s: %v", ActionsFile, err)
		return &actionLog{pending: map[string]string{}}
	}
	a := &actionLog{f: f, start: time.Now(), pending: map[string]string{}}
	// The header makes one file answer "which run is this" without a second read,
	// so `rig watch` opens exactly one thing.
	fmt.Fprintf(f, "repo:    %s\ntask:    %s\nstarted: %s\n\n",
		repo, firstLine(task), a.start.Format("2006-01-02 15:04:05"))
	return a
}

func (a *actionLog) close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f != nil {
		_ = a.f.Close()
		a.f = nil
	}
}

// write emits one line, stamped with elapsed time since the run started. Elapsed
// beats wall-clock here: the question is always "how long has this been going",
// and a reader should not have to do arithmetic to answer it.
func (a *actionLog) write(format string, args ...any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f == nil {
		return
	}
	d := time.Since(a.start)
	fmt.Fprintf(a.f, "+%02d:%02d  %s\n",
		int(d.Minutes()), int(d.Seconds())%60, fmt.Sprintf(format, args...))
}

// toolCall records a tool the worker just invoked.
func (a *actionLog) toolCall(id, name string, input json.RawMessage) {
	label := describeTool(name, input)
	a.mu.Lock()
	a.pending[id] = name
	a.mu.Unlock()
	a.write("%-6s %s", name, label)
}

// toolResult records the outcome, attributed to the call it answers. Only the
// gist: a result can be a megabyte, and this file is read by a human at a glance.
func (a *actionLog) toolResult(id string, body string, isErr bool) {
	a.mu.Lock()
	name, known := a.pending[id]
	delete(a.pending, id)
	a.mu.Unlock()
	if !known {
		return
	}
	// Reads and edits are noisy and their success is implied by what follows.
	// A FAILURE is never implied, so those are always shown.
	if !isErr && name != "Bash" {
		return
	}
	mark := "ok"
	if isErr {
		mark = "FAILED"
	}
	if s := gist(body); s != "" {
		a.write("  ↳    %s · %s", mark, s)
		return
	}
	a.write("  ↳    %s", mark)
}

// finish writes the closing line, which is also how `rig watch` knows to stop
// waiting rather than hang on a file nobody will append to again.
func (a *actionLog) finish(status string) {
	a.write("──     %s", status)
}

// describeTool reduces a tool's input to the one thing worth seeing in a live
// feed: which file, or which command.
func describeTool(name string, input json.RawMessage) string {
	var in struct {
		Command  string `json:"command"`
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
		Pattern  string `json:"pattern"`
	}
	_ = json.Unmarshal(input, &in)
	switch {
	case in.Command != "":
		return truncateMiddle(firstLine(in.Command), 90)
	case in.FilePath != "":
		return shortPath(in.FilePath)
	case in.Path != "":
		return shortPath(in.Path)
	case in.Pattern != "":
		return in.Pattern
	}
	return ""
}

// gist is the most informative single line of a tool result. A test runner's
// verdict is on its LAST meaningful line, not its first, which is why this
// scans from the end.
func gist(body string) string {
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	for i := len(lines) - 1; i >= 0 && i > len(lines)-40; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" || strings.Trim(l, ".=-_ ") == "" {
			continue
		}
		return truncateMiddle(l, 90)
	}
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// shortPath keeps the tail of a path: the leading directories are the same for
// every line in a run and push the part that differs off the screen.
func shortPath(p string) string {
	parts := strings.Split(filepath.ToSlash(p), "/")
	if len(parts) <= 3 {
		return p
	}
	return ".../" + strings.Join(parts[len(parts)-2:], "/")
}

// truncateMiddle elides the middle rather than the end: a long command's tail
// (the flags, the target) is usually more identifying than its head.
func truncateMiddle(s string, max int) string {
	if len(s) <= max {
		return s
	}
	half := (max - 3) / 2
	return s[:half] + "..." + s[len(s)-half:]
}
