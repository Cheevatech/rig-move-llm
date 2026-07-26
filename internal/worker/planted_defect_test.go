package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The planted defect: a line the gate is green on and only a human review can catch.
// testdata/planted/README.md explains why this particular one.
const plantedDefect = "return total / len(values)"

// plantedRepo builds the fixture as a real git repo — before-state committed, the
// worker's return applied on top — so everything downstream reads the same git the
// live path reads.
func plantedRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	src := filepath.Join("testdata", "planted")

	copyIn := func(stage string) {
		for _, name := range []string{"stats.py", "test_stats.py"} {
			b, err := os.ReadFile(filepath.Join(src, stage, name))
			if err != nil {
				t.Fatalf("fixture %s/%s: %v", stage, name, err)
			}
			if err := os.WriteFile(filepath.Join(repo, name), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	copyIn("before")
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.email=b6@local", "-c", "user.name=b6", "commit", "-qm", "before"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	copyIn("after")
	return repo
}

// plantedResult is what the engine hands TierResult after the worker's run: a green
// gate and the diff it produced, taken from git rather than written by hand.
func plantedResult(t *testing.T, repo string) Result {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "diff", "--", "stats.py").Output()
	if err != nil {
		t.Fatalf("git diff: %v", err)
	}
	if !strings.Contains(string(out), plantedDefect) {
		t.Fatalf("fixture produced no diff carrying the defect:\n%s", out)
	}
	return Result{
		Stopped:      "done",
		FilesChanged: []string{"stats.py"},
		Diff:         string(out),
		LastTest:     "6 passed in 0.02s\n[exit 0]",
	}
}

// The safety axis of R13 in miniature. Under the manifest tier MAIN never sees the
// diff — it sees pointers. If no pointer leads back to the defect, the review is
// blind by construction and no cost win can redeem it.
func TestPlantedDefectIsReachableThroughTheManifest(t *testing.T) {
	repo := plantedRepo(t)
	res := plantedResult(t, repo)

	// Threshold of 1 token forces the manifest tier on a fixture this small; the
	// live run reaches it by having a diff that is genuinely large.
	tiered := TierResult(res, repo, 1)
	if tiered.Tier != "manifest" {
		t.Fatalf("want the manifest tier, got %q", tiered.Tier)
	}
	if strings.Contains(tiered.Diff, plantedDefect) {
		t.Fatal("the manifest tier still shipped the diff body — nothing was tiered")
	}
	if len(tiered.Changes) == 0 {
		t.Fatal("manifest has no entries: nothing to drill, nothing reviewable")
	}

	// Drill exactly what the manifest offers, the way MAIN would.
	var found bool
	var drilled int
	for _, ch := range tiered.Changes {
		got, err := ShowChange(context.Background(), repo, ch.File, ch.Line, ch.EndLine, "diff")
		if err != nil {
			t.Fatalf("manifest entry %s:%d-%d does not drill: %v", ch.File, ch.Line, ch.EndLine, err)
		}
		drilled += len(got.Body)
		if strings.Contains(got.Body, plantedDefect) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no manifest entry drilled back to %q — the review is blind to the defect", plantedDefect)
	}
	if drilled == 0 {
		t.Fatal("every drill came back empty")
	}
}

// A pointer that leads to the new line but not the line it replaced would let the
// defect read as ordinary code. The review needs the change, not the result.
func TestPlantedDefectDrillShowsTheLineItReplaced(t *testing.T) {
	repo := plantedRepo(t)
	tiered := TierResult(plantedResult(t, repo), repo, 1)

	var body strings.Builder
	for _, ch := range tiered.Changes {
		got, err := ShowChange(context.Background(), repo, ch.File, ch.Line, ch.EndLine, "diff")
		if err != nil {
			t.Fatal(err)
		}
		body.WriteString(got.Body)
	}
	all := body.String()

	if !strings.Contains(all, "-    return total / wsum") {
		t.Fatalf("drilled bodies never show the removed line, so the defect looks like ordinary code:\n%s", all)
	}
	if !strings.Contains(all, "+    return total / len(values)") {
		t.Fatalf("drilled bodies never show the added line:\n%s", all)
	}
}

// The fixture only works if the defect is invisible to the tests. If someone later
// adds a case that covers non-uniform weights, this fixture stops testing what B6
// needs it to test — and should say so here rather than in a live run.
func TestPlantedFixtureKeepsTheDefectOutOfTheGate(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "planted", "after", "test_stats.py"))
	if err != nil {
		t.Fatal(err)
	}
	suite := string(b)

	// Every weight literal in the suite is 1.0 or 0.0: uniform or rejected. Under
	// uniform weights total/wsum and total/len(values) agree, which is exactly why
	// the gate stays green.
	for _, bad := range []string{"[2.0, 1.0]", "[3.0, 1.0]", "[1.0, 2.0, 3.0], [1.0, 2.0"} {
		if strings.Contains(suite, bad) {
			t.Fatalf("suite now covers non-uniform weights (%s) — the planted defect would fail the gate, "+
				"which makes it the wrong kind of defect for the safety probe", bad)
		}
	}
}
