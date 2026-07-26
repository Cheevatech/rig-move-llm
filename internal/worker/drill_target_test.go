package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The drill must land on the repo the work happened in. Live fire (B6 run 2)
// found the opposite: show_change's repo defaulted to the server's own working
// directory — under the product wiring, rig's checkout, not the target repo —
// and the miss came back looking like a successful call reporting "nothing
// there". A review that believes that signs off blind (invariant R1).
//
// Three layers are tested here, because one is not enough: the server remembers
// where implement ran (so the default is right), the manifest publishes the repo
// (so MAIN can be explicit), and a miss diagnoses itself instead of reading as
// "no change" (so a wrong tree can never look like a clean one).

func showChange(t *testing.T, s *Server, args map[string]any) (string, bool) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out := s.onShowChange(raw)
	content, _ := out["content"].([]map[string]any)
	if len(content) == 0 {
		t.Fatalf("no content in %v", out)
	}
	text, _ := content[0]["text"].(string)
	isErr, _ := out["isError"].(bool)
	return text, isErr
}

// --- layer 1: the default target is where implement ran --------------------

func TestShowChangeDefaultsToTheRepoImplementRan(t *testing.T) {
	repo := drillRepo(t, "app.py",
		"def f():\n    return 1\n",
		"def f():\n    return 2\n")

	s := &Server{}
	s.noteRepo(repo)

	// No repo argument — exactly the call the manifest's guidance produces.
	text, isErr := showChange(t, s, map[string]any{
		"file": "app.py", "start_line": 1, "end_line": 2,
	})
	if isErr {
		t.Fatalf("drill errored on the implement repo: %s", text)
	}
	if !strings.Contains(text, "return 2") {
		t.Errorf("drill missed the change it should have served:\n%s", text)
	}
}

func TestShowChangeExplicitRepoBeatsTheRemembered(t *testing.T) {
	remembered := drillRepo(t, "app.py", "x = 1\n", "x = 2\n")
	other := drillRepo(t, "other.py", "y = 1\n", "y = 9\n")

	s := &Server{}
	s.noteRepo(remembered)

	text, isErr := showChange(t, s, map[string]any{
		"file": "other.py", "start_line": 1, "repo": other,
	})
	if isErr {
		t.Fatalf("explicit repo was not honoured: %s", text)
	}
	if !strings.Contains(text, "y = 9") {
		t.Errorf("served the wrong tree:\n%s", text)
	}
}

// --- layer 2: a miss diagnoses itself --------------------------------------

func TestShowChangeOnACleanTreeIsAnErrorNotNothingThere(t *testing.T) {
	// A checkout with no working-tree change at all: the signature of drilling
	// the wrong repo. Reporting "nothing there" here is what let a real change
	// pass as reviewed.
	clean := drillRepo(t, "app.py", "x = 1\n", "x = 1\n")

	s := &Server{}
	text, isErr := showChange(t, s, map[string]any{
		"file": "app.py", "start_line": 1, "repo": clean,
	})
	if !isErr {
		t.Errorf("a clean tree must be reported as an error, got isError=false:\n%s", text)
	}
	if strings.Contains(text, "nothing there") {
		t.Errorf("still reads as an ordinary empty result:\n%s", text)
	}
	if !strings.Contains(text, "working-tree changes") {
		t.Errorf("does not name the actual condition:\n%s", text)
	}
	if !strings.Contains(text, "repo") {
		t.Errorf("does not point at the repo argument as the fix:\n%s", text)
	}
}

func TestShowChangeOnAnUnchangedFileNamesTheChangedOnes(t *testing.T) {
	// untouched.py is committed and never edited; app.py carries the working-tree
	// change. Drilling untouched.py must not read as "no change here" — the tree
	// is dirty, so the honest answer names where the change actually is.
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "app.py"), []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "untouched.py"), []byte("z = 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-qm", "base")
	if err := os.WriteFile(filepath.Join(repo, "app.py"), []byte("x = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{}
	text, isErr := showChange(t, s, map[string]any{
		"file": "untouched.py", "start_line": 1, "repo": repo,
	})
	if !isErr {
		t.Errorf("drilling an unchanged file in a dirty tree should be an error:\n%s", text)
	}
	if !strings.Contains(text, "app.py") {
		t.Errorf("does not name the files that did change:\n%s", text)
	}
}

func TestShowChangeOutOfRangeNamesTheRealSpans(t *testing.T) {
	repo := drillRepo(t, "app.py",
		"a = 1\nb = 2\nc = 3\nd = 4\ne = 5\nf = 6\ng = 7\nh = 8\n",
		"a = 1\nb = 2\nc = 3\nd = 4\ne = 5\nf = 6\ng = 7\nh = 99\n")

	s := &Server{}
	text, isErr := showChange(t, s, map[string]any{
		"file": "app.py", "start_line": 1, "end_line": 1, "repo": repo,
	})
	if !isErr {
		t.Errorf("a range that covers no hunk should be an error:\n%s", text)
	}
	if !strings.Contains(text, "8") {
		t.Errorf("does not name the span that does carry the change:\n%s", text)
	}
}

// --- layer 3: the manifest carries its own drill target --------------------

func TestManifestPublishesTheRepoAndGuidanceSaysToPassIt(t *testing.T) {
	repo := drillRepo(t, "app.py",
		"def f():\n    return 1\n",
		"def f():\n    return 2\n")
	diff := gitRun(t, repo, "diff")

	out := TierResult(Result{Diff: diff, Summary: "bump", LastTest: "ok\n[exit 0]"}, repo, 1)
	if out.Tier != "manifest" {
		t.Fatalf("tier=%q, wanted manifest", out.Tier)
	}
	if out.Repo != repo {
		t.Errorf("manifest does not publish the repo: %q", out.Repo)
	}
	// The word "repo" appears in the guidance's prose anyway, so assert on the
	// argument itself and on the instruction to pass it.
	if !strings.Contains(out.DrillWith, "repo:") {
		t.Errorf("guidance does not show `repo` as a drill argument:\n%s", out.DrillWith)
	}
	if !strings.Contains(out.DrillWith, "ALWAYS pass `repo`") {
		t.Errorf("guidance does not insist on passing it:\n%s", out.DrillWith)
	}
	if out.Verify == nil || out.Verify.Drill == nil {
		t.Fatal("no verify drill ref")
	}
	if out.Verify.Drill.Repo != repo {
		t.Errorf("verify drill ref does not carry the repo: %q", out.Verify.Drill.Repo)
	}
}
