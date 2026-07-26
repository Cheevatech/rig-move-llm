package worker

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// The drill primitive (R9 §4).
//
// Everything else on this MCP surface returns pointers; show_change is the one
// tool that returns a body. That asymmetry is the whole design: the tiered return
// leaves the raw diff in the target repo's git working tree and hands MAIN a
// manifest of pointers, so review stays sharp by drilling the two or three
// entries that look wrong instead of paying for the whole diff up front.
//
// It is deliberately stateless. There is no artifact store to go stale, get
// evicted, or disagree with the checkout: the source of truth is git, read live,
// exactly as collectDiff reads it when the manifest is built.
//
// The safety property this file exists to hold: what comes back is git's own
// bytes. Not a summary, not a re-wrap, and never a hunk with a line clipped off
// it. A drill that quietly loses a line makes review blind to a defect sitting on
// it, which breaks the invariant the tiering is only acceptable under (R1).

const (
	drillKindDiff    = "diff"
	drillKindTestLog = "test_log"
)

// drillContext is how much unchanged context rides along with each hunk. It is
// git's own default; larger costs tokens per drill, smaller makes an edit harder
// to judge in place.
func drillContext() int { return envInt("RIG_DRILL_CONTEXT", 3) }

// DrillResult is one drill. Body is verbatim: for a diff it is a run of git's
// output, for a test log it is a byte range of the parked file.
type DrillResult struct {
	File      string `json:"file"`
	Kind      string `json:"kind"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Hunks     int    `json:"hunks"`
	Body      string `json:"body"`
}

// ShowChange serves the pointer-scoped body behind a manifest entry.
//
// file, startLine and endLine are exactly the fields the manifest publishes
// (Pointer.File, .Line, .EndLine), in post-change line numbering. kind selects
// the substrate: "diff" reads the working tree through git, "test_log" reads the
// log the tiered return parked (R9 §7). Empty kind means "diff".
func ShowChange(ctx context.Context, repo, file string, startLine, endLine int, kind string) (*DrillResult, error) {
	switch kind {
	case "":
		kind = drillKindDiff
	case drillKindDiff, drillKindTestLog:
	default:
		return nil, fmt.Errorf("unknown kind %q (want %q or %q)", kind, drillKindDiff, drillKindTestLog)
	}
	if startLine < 1 {
		startLine = 1
	}
	if endLine < startLine {
		endLine = startLine
	}

	abs, err := safeJoin(repo, file)
	if err != nil {
		return nil, err
	}
	// The gate contract is frozen for the worker and opaque to review; serving it
	// here would be a read-side hole in the same fence tools.go puts up.
	if isGatePath(repo, abs) {
		return nil, fmt.Errorf("path is inside the frozen gate contract: %s", file)
	}

	res := &DrillResult{File: file, Kind: kind, StartLine: startLine, EndLine: endLine}
	if kind == drillKindTestLog {
		b, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", file, err)
		}
		res.Body = sliceLines(string(b), startLine, endLine)
	} else {
		out := gitOut(ctx, repo, "diff", "-U"+strconv.Itoa(drillContext()), "--", file)
		res.Body, res.Hunks = sliceDiff(out, startLine, endLine)
	}

	meterDrill(len(res.Body))
	return res, nil
}

// sliceDiff keeps the hunks of a single-file unified diff whose post-change span
// intersects [start, end], with the file header they belong to.
//
// Selection is per hunk, never within one. Clipping a hunk at the requested
// boundary would be the cheaper answer and the wrong one: an edit's `-` and `+`
// halves straddle that boundary routinely, and half an edit reads as a different
// change than the whole. Over-including by a few context lines costs tokens;
// under-including costs correctness.
func sliceDiff(out string, start, end int) (string, int) {
	var (
		header  strings.Builder
		body    strings.Builder
		hunks   int
		inHunk  bool
		keeping bool
	)
	for _, ln := range diffLines(out) {
		if strings.HasPrefix(ln.text, "@@") {
			inHunk = true
			hStart, hCount := hunkNewSpan(ln.text)
			// hStart == 0 means the new side is empty (the file was deleted), so
			// there is no line number to intersect — the change is always in scope.
			keeping = hStart == 0 || (hStart <= end && hStart+hCount-1 >= start)
			if keeping {
				hunks++
			}
		}
		switch {
		case !inHunk:
			header.WriteString(ln.raw)
		case keeping:
			body.WriteString(ln.raw)
		}
	}
	if hunks == 0 {
		return "", 0
	}
	return header.String() + body.String(), hunks
}

// diffLine carries the line's text alongside its raw bytes (the line plus its
// newline, if it had one), so reassembly is byte-exact rather than a re-join.
type diffLine struct {
	text string
	raw  string
}

func diffLines(s string) []diffLine {
	var out []diffLine
	for i := 0; i < len(s); {
		j := strings.IndexByte(s[i:], '\n')
		if j < 0 {
			out = append(out, diffLine{text: s[i:], raw: s[i:]})
			break
		}
		out = append(out, diffLine{text: s[i : i+j], raw: s[i : i+j+1]})
		i += j + 1
	}
	return out
}

// hunkNewSpan reads the post-change side of "@@ -a,b +c,d @@": start c and count
// d, where an omitted d means 1. A pure-deletion hunk has d == 0; it is given a
// span of one line so it can still be found by the position it left behind, which
// is the line the manifest points at.
func hunkNewSpan(l string) (int, int) {
	i := strings.Index(l, "+")
	if i < 0 {
		return 0, 0
	}
	rest := l[i+1:]
	if j := strings.Index(rest, " @@"); j >= 0 {
		rest = rest[:j]
	}
	startStr, countStr := rest, "1"
	if j := strings.IndexByte(rest, ','); j >= 0 {
		startStr, countStr = rest[:j], rest[j+1:]
	}
	start, err := strconv.Atoi(strings.TrimSpace(startStr))
	if err != nil {
		return 0, 0
	}
	count, err := strconv.Atoi(strings.TrimSpace(countStr))
	if err != nil || count < 1 {
		count = 1
	}
	return start, count
}

// sliceLines returns lines [start, end] of s, 1-based and inclusive, as the
// original bytes. Out-of-range ends clamp; an out-of-range start yields nothing.
func sliceLines(s string, start, end int) string {
	lines := diffLines(s)
	// A trailing newline leaves a final empty line; the parked log's line count
	// includes it (parkTestLog counts newlines + 1), so it stays addressable.
	if start > len(lines) {
		return ""
	}
	if end > len(lines) {
		end = len(lines)
	}
	var b strings.Builder
	for _, ln := range lines[start-1 : end] {
		b.WriteString(ln.raw)
	}
	return b.String()
}

// --- the meter ------------------------------------------------------------

// Drilled bytes are the tiering's running cost: they re-enter MAIN's context and
// re-cache there, so a return that saves 30k on the manifest and gives it back in
// drills has saved nothing. B6 weighs this against the bytes the tier withheld —
// and against direct Read calls, which the PreTool hook does not deny, so MAIN
// can always route around the drill and must be observed doing it.
var drillMeter struct {
	sync.Mutex
	calls int
	bytes int64
}

func meterDrill(n int) {
	drillMeter.Lock()
	drillMeter.calls++
	drillMeter.bytes += int64(n)
	drillMeter.Unlock()
}

// DrilledSoFar reports the drills served in this worker process and their total
// body size.
func DrilledSoFar() (calls int, bytes int64) {
	drillMeter.Lock()
	defer drillMeter.Unlock()
	return drillMeter.calls, drillMeter.bytes
}
