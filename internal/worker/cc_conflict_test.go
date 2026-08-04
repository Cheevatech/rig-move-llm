package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/config"
)

// The shape measured in the #54 probe: the worker's pop half-applied, it
// resolved the conflict itself, and its summary said the proof was complete.
const conflictDiff = `diff --git a/money.go b/money.go
index af69982..f5547c2 100644
--- a/money.go
+++ b/money.go
@@ -3,9 +3,12 @@ package smoke
 func SplitBill(totalCents, n int) []int {
+<<<<<<< Updated upstream
 	share := totalCents / n
+=======
+	share, rem := totalCents/n, totalCents%n
+>>>>>>> Stashed changes
 	out := make([]int, n)
 	return out
 }
`

func TestConflictMarkersAreReadOffTheDiff(t *testing.T) {
	res := Result{Summary: "restored the fix via git stash pop — the proof-of-flip is complete.", Diff: conflictDiff}
	noteConflictMarkers(&res)

	if len(res.ConflictMarkers) != 1 || res.ConflictMarkers[0] != "money.go" {
		t.Fatalf("conflict markers not reported: %v", res.ConflictMarkers)
	}
	if !strings.Contains(res.Summary, "CONFLICT MARKERS IN THE DIFF") {
		t.Errorf("the summary MAIN reads does not say the tree is broken:\n%s", res.Summary)
	}
	if !strings.Contains(res.Summary, "money.go") {
		t.Errorf("the summary does not name the file:\n%s", res.Summary)
	}
	// The work itself must survive: a broken tree is something the user has to
	// look at, and dropping the diff would take that away.
	if res.Diff == "" {
		t.Error("the diff was dropped — the round's work must still reach the user")
	}
}

func TestCleanRoundIsNotFlagged(t *testing.T) {
	clean := `diff --git a/money.go b/money.go
--- a/money.go
+++ b/money.go
@@ -3,7 +3,8 @@
+	rem := totalCents % n
 	out := make([]int, n)
`
	res := Result{Summary: "fixed it", Diff: clean}
	noteConflictMarkers(&res)
	if len(res.ConflictMarkers) != 0 {
		t.Errorf("a clean round was flagged: %v", res.ConflictMarkers)
	}
	if strings.Contains(res.Summary, "CONFLICT MARKERS") {
		t.Errorf("a clean round's summary was polluted:\n%s", res.Summary)
	}
}

// Markers that were already in the tree are not this round's doing, and a round
// that REMOVES them is cleaning up — neither may be reported as damage.
func TestPreexistingAndRemovedMarkersAreNotThisRoundsDoing(t *testing.T) {
	removing := `diff --git a/notes.md b/notes.md
--- a/notes.md
+++ b/notes.md
@@ -1,5 +1,3 @@
-<<<<<<< HEAD
-=======
->>>>>>> other
 clean line
`
	res := Result{Diff: removing}
	noteConflictMarkers(&res)
	if len(res.ConflictMarkers) != 0 {
		t.Errorf("a round that removed stale markers was flagged: %v", res.ConflictMarkers)
	}
}

// `=======` alone is ordinary markdown (a setext heading rule); only the
// bracketing markers, which carry a trailing space and a label, identify a
// conflict.
func TestMarkdownRuleIsNotAConflict(t *testing.T) {
	md := `diff --git a/README.md b/README.md
--- a/README.md
+++ b/README.md
@@ -1,2 +1,4 @@
+Heading
+=======
+
`
	res := Result{Diff: md}
	noteConflictMarkers(&res)
	if len(res.ConflictMarkers) != 0 {
		t.Errorf("a markdown heading rule was read as a conflict: %v", res.ConflictMarkers)
	}
}

// A check the engine never calls is a check that does not exist. This runs the
// real implement path with a worker that leaves markers behind and a summary
// that claims success — the #54 probe's shape, end to end.
func TestImplementReportsAConflictedTree(t *testing.T) {
	repo := ccTestRepo(t)
	outDir := t.TempDir()
	streamFile := filepath.Join(outDir, "stream.jsonl")
	if err := os.WriteFile(streamFile, []byte(ccHappyStream), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/bash\n" +
		"{ echo '<<<<<<< Updated upstream'; echo original; echo '======='; echo fixed; echo '>>>>>>> Stashed changes'; } > file.txt\n" +
		"cat '" + streamFile + "'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ccEnv(t, bin)

	e := NewEngine(config.Config{})
	res := e.Implement(context.Background(), repo, "fix the bug", "")

	if len(res.ConflictMarkers) == 0 {
		t.Fatalf("implement returned a conflicted tree as if it were clean; summary:\n%s\ndiff:\n%s", res.Summary, res.Diff)
	}
	if !strings.Contains(res.Summary, "CONFLICT MARKERS IN THE DIFF") {
		t.Errorf("MAIN would read this round as a success:\n%s", res.Summary)
	}
}

func TestEveryConflictedFileIsNamed(t *testing.T) {
	two := conflictDiff + `diff --git a/pkg/util.go b/pkg/util.go
--- a/pkg/util.go
+++ b/pkg/util.go
@@ -1,3 +1,5 @@
+<<<<<<< Updated upstream
+>>>>>>> Stashed changes
`
	res := Result{Diff: two}
	noteConflictMarkers(&res)
	if len(res.ConflictMarkers) != 2 || res.ConflictMarkers[0] != "money.go" || res.ConflictMarkers[1] != "pkg/util.go" {
		t.Fatalf("not every conflicted file was named: %v", res.ConflictMarkers)
	}
}
