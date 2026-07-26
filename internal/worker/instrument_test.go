package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// B6 needs the run's numbers to survive the run. A stdio MCP server's stderr is
// swallowed by whatever launched it (Claude Code files it under its own logs),
// so the measurement harness cannot rely on it. RIG_WORKER_LOG mirrors every
// logStderr line into a file the harness owns.
func TestWorkerLogMirrorsToFileWhenAsked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "worker.log")
	t.Setenv("RIG_WORKER_LOG", path)

	logStderr("worker.implement done tier=%s diff_tokens=%d", "manifest", 4096)
	logStderr("worker.show_change drilled_total=%d/%d", 2, 810)

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("log file not written: %v", err)
	}
	got := string(b)
	for _, want := range []string{"tier=manifest", "diff_tokens=4096", "drilled_total=2/810"} {
		if !strings.Contains(got, want) {
			t.Fatalf("mirrored log missing %q; got:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "[rig-worker]"); n != 2 {
		t.Fatalf("want 2 mirrored lines, got %d:\n%s", n, got)
	}
}

func TestWorkerLogAppendsAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "worker.log")
	t.Setenv("RIG_WORKER_LOG", path)

	logStderr("first")
	logStderr("second")

	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "first") || !strings.Contains(string(b), "second") {
		t.Fatalf("second line clobbered the first: %s", b)
	}
}

func TestWorkerLogUnsetIsHarmless(t *testing.T) {
	t.Setenv("RIG_WORKER_LOG", "")
	logStderr("no file configured") // must not panic
}

// The B6 tuning knobs are RIG_RETURN_THRESHOLD and the ×2 rollup rule. Neither
// can be tuned from a log line that omits what the gate actually decided, so the
// implement summary carries granularity and the manifest's own token count
// beside the tier it chose.
func TestImplementSummaryCarriesTheTuningNumbers(t *testing.T) {
	res := Result{Stopped: "done", Iterations: 3, FilesChanged: []string{"a.py"}}
	tiered := TieredResult{
		Tier: "manifest", Granularity: "file", DiffTokens: 9000, ThresholdTokens: 2000,
		Changes: []Change{{}, {}, {}},
	}
	line := implementSummary(res, tiered)

	for _, want := range []string{
		"tier=manifest", "granularity=file", "diff_tokens=9000",
		"threshold_tokens=2000", "changes=3", "manifest_tokens=",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("summary line missing %q; got: %s", want, line)
		}
	}
}

// Whether the tiering actually paid is manifest_tokens vs diff_tokens on the
// same return — so the number reported has to be the payload MAIN really sees,
// not a re-estimate of something else.
func TestImplementSummaryManifestTokensCountsWhatMainReceives(t *testing.T) {
	tiered := TieredResult{
		Tier: "manifest", Granularity: "symbol", DiffTokens: 9000,
		Changes: []Change{{Pointer: Pointer{File: "sympy/core/mul.py", Line: 120, EndLine: 140}, Intent: "guard the zero case"}},
	}
	line := implementSummary(Result{Stopped: "done"}, tiered)

	got := fieldValue(t, line, "manifest_tokens=")
	if got <= 0 {
		t.Fatalf("manifest_tokens must be positive, got %d in: %s", got, line)
	}
	if got >= 9000 {
		t.Fatalf("a manifest that does not undercut its diff is not compressing: %d vs 9000", got)
	}
}

// tier=full means the diff went through untiered — reporting a manifest cost
// there would read as a saving that never happened.
func TestImplementSummaryReportsFullTierHonestly(t *testing.T) {
	tiered := TieredResult{Tier: "full", DiffTokens: 300, ThresholdTokens: 2000, Diff: "@@ -1 +1 @@\n-a\n+b\n"}
	line := implementSummary(Result{Stopped: "done"}, tiered)

	if !strings.Contains(line, "tier=full") {
		t.Fatalf("want tier=full: %s", line)
	}
	if v := fieldValue(t, line, "manifest_tokens="); v != 0 {
		t.Fatalf("full tier ships no manifest, so manifest_tokens must be 0, got %d: %s", v, line)
	}
}

func fieldValue(t *testing.T, line, key string) int {
	t.Helper()
	i := strings.Index(line, key)
	if i < 0 {
		t.Fatalf("line has no %s: %s", key, line)
	}
	rest := line[i+len(key):]
	if j := strings.IndexAny(rest, " \t"); j >= 0 {
		rest = rest[:j]
	}
	n := 0
	for _, c := range rest {
		if c < '0' || c > '9' {
			t.Fatalf("non-numeric value for %s in: %s", key, line)
		}
		n = n*10 + int(c-'0')
	}
	return n
}
