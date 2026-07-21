package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/internal/gatestate"
	"github.com/Cheevatech/rig-move-llm/pkg/translate"
)

// exploreRepo builds a committed repo with one file the task identifiers hit.
func exploreRepo(t *testing.T) string {
	t.Helper()
	dir := gitRepo(t)
	src := "def compute_total(items):\n    total = 0\n    for it in items:\n        total += it.price\n    return total\n"
	if err := os.WriteFile(filepath.Join(dir, "util.py"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("add", "-A")
	run("commit", "-qm", "util")
	return dir
}

const exploreTask = "compute_total sums item prices wrongly when the list is empty"

const goodReportJSON = `{"relevant_files":["util.py"],
"candidate_edit_sites":[{"path":"util.py","line":1,"snippet":"def compute_total(items):","rationale":"compute_total is the function under test"}],
"entrypoints_or_repro":["python -c 'import util'"],"open_questions":[],"unread_but_flagged":[]}`

func TestTaskIdentifiers(t *testing.T) {
	ids := TaskIdentifiers("Fix the bug where compute_total and OrderModel return the wrong value", 5)
	if len(ids) == 0 || ids[0] != "compute_total" && ids[0] != "OrderModel" {
		t.Fatalf("code-shaped tokens should rank first, got %v", ids)
	}
	for _, id := range ids {
		if id == "the" || id == "bug" || id == "Fix" {
			t.Fatalf("stopword/title word leaked into %v", ids)
		}
	}
}

func TestComputeAnchors(t *testing.T) {
	dir := exploreRepo(t)
	anchors := ComputeAnchors(context.Background(), dir, exploreTask, 8)
	if len(anchors) == 0 || anchors[0].Path != "util.py" {
		t.Fatalf("expected util.py as top anchor, got %+v", anchors)
	}
}

func TestExploreVerifiedFlow(t *testing.T) {
	t.Setenv("RIG_STATE_DIR", t.TempDir())
	dir := exploreRepo(t)
	srv := fakeBackend(t, []translate.OpenAIResponse{
		toolCallResp("c1", "read_file", `{"path":"util.py"}`),
		finalResp(goodReportJSON),
	})
	defer srv.Close()
	e := NewEngine(config.Config{WorkerAPIBase: srv.URL})
	res := e.Explore(context.Background(), dir, exploreTask)

	if res.Stopped != "done" {
		t.Fatalf("stopped=%s err=%s", res.Stopped, res.Err)
	}
	if len(res.CandidateEditSites) != 1 || res.CandidateEditSites[0].Path != "util.py" {
		t.Fatalf("verified site missing: %+v", res.CandidateEditSites)
	}
	if len(res.RejectedSites) != 0 {
		t.Fatalf("unexpected rejections: %v", res.RejectedSites)
	}
	if !contains(res.AnchorsRead, "util.py") {
		t.Fatalf("anchors_read should include util.py: %v", res.AnchorsRead)
	}
	// The digest must be persisted for the triage backstop.
	ev, fresh := gatestate.ReadExplore(os.Getenv("RIG_STATE_DIR"))
	if !fresh || ev.NSites != 1 || len(ev.EditSiteFiles) != 1 || ev.EditSiteFiles[0] != "util.py" {
		t.Fatalf("persisted explore digest wrong: %+v fresh=%v", ev, fresh)
	}
}

func TestExploreCoverageBounce(t *testing.T) {
	t.Setenv("RIG_STATE_DIR", t.TempDir())
	dir := exploreRepo(t)
	srv := fakeBackend(t, []translate.OpenAIResponse{
		finalResp(goodReportJSON), // emitted before reading the MUST anchor -> bounced
		toolCallResp("c1", "read_file", `{"path":"util.py"}`),
		finalResp(goodReportJSON),
	})
	defer srv.Close()
	e := NewEngine(config.Config{WorkerAPIBase: srv.URL})
	res := e.Explore(context.Background(), dir, exploreTask)
	if res.Stopped != "done" || res.CoverageIncomplete {
		t.Fatalf("expected clean done after coverage bounce: stopped=%s incomplete=%v err=%s", res.Stopped, res.CoverageIncomplete, res.Err)
	}
	if res.Iterations != 3 {
		t.Fatalf("expected 3 iterations (final, read, final), got %d", res.Iterations)
	}
}

func TestExploreCeilingReturnsBestEffort(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RIG_STATE_DIR", dir)
	t.Setenv("RIG_EXPLORE_MAX_ITERS", "2")
	repo := exploreRepo(t)
	// Worker only ever makes tool calls, never a valid final report -> rig runs the
	// full internal round loop and returns best-effort (max_rounds), not an error.
	var script []translate.OpenAIResponse
	for i := 0; i < maxExploreRounds*2; i++ {
		script = append(script, toolCallResp("c", "grep_repo", `{"pattern":"x"}`))
	}
	srv := fakeBackend(t, script)
	defer srv.Close()
	e := NewEngine(config.Config{WorkerAPIBase: srv.URL})
	res := e.Explore(context.Background(), repo, exploreTask)

	if res.Stopped != "max_rounds" || !res.CoverageIncomplete {
		t.Fatalf("hitting the ceiling must yield max_rounds + incomplete, got %+v", res)
	}
	if res.Rounds != maxExploreRounds {
		t.Fatalf("rig should run all %d internal rounds, got %d", maxExploreRounds, res.Rounds)
	}
	// Digest incomplete so a premature triage=solo is overridden to delegate.
	ev, fresh := gatestate.ReadExplore(dir)
	if !fresh || !ev.CoverageIncomplete {
		t.Fatalf("digest must mark coverage incomplete: %+v fresh=%v", ev, fresh)
	}
	// The loop is rig-owned and self-terminating: progress cleared on return.
	if _, err := os.Stat(filepath.Join(dir, "explore_progress.json")); !os.IsNotExist(err) {
		t.Fatalf("progress should be cleared after the run finishes")
	}
	if _, err := os.Stat(filepath.Join(dir, "explore_last.json")); err != nil {
		t.Fatalf("explore_last.json debug dump missing: %v", err)
	}
}

func TestExploreLoopsInternallyAcrossRounds(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RIG_STATE_DIR", dir)
	t.Setenv("RIG_EXPLORE_MAX_ITERS", "2")
	repo := exploreRepo(t)
	// Round 1: two tool calls, no report (budget runs out) -> rig starts round 2
	// ITSELF. Round 2: worker emits a valid report; util.py was read in round 1, so
	// coverage is satisfied via accumulated progress. All within ONE Explore call.
	srv := fakeBackend(t, []translate.OpenAIResponse{
		toolCallResp("c1", "read_file", `{"path":"util.py"}`),
		toolCallResp("c2", "grep_repo", `{"pattern":"x"}`),
		finalResp(goodReportJSON),
	})
	defer srv.Close()
	e := NewEngine(config.Config{WorkerAPIBase: srv.URL})
	res := e.Explore(context.Background(), repo, exploreTask)

	if res.Stopped != "done" {
		t.Fatalf("rig should loop internally to done, got %+v", res)
	}
	if res.Rounds != 2 {
		t.Fatalf("should have taken 2 internal rounds, got %d", res.Rounds)
	}
	if len(res.CandidateEditSites) != 1 || res.CandidateEditSites[0].Path != "util.py" {
		t.Fatalf("accumulated report should carry the verified site: %+v", res.CandidateEditSites)
	}
	if _, err := os.Stat(filepath.Join(dir, "explore_progress.json")); !os.IsNotExist(err) {
		t.Fatalf("progress should be cleared after done")
	}
	ev, fresh := gatestate.ReadExplore(dir)
	if !fresh || ev.CoverageIncomplete {
		t.Fatalf("final digest should be coverage-complete: %+v", ev)
	}
}

func TestExploreGroundingRejects(t *testing.T) {
	t.Setenv("RIG_STATE_DIR", t.TempDir())
	dir := exploreRepo(t)
	mixed := `{"relevant_files":["util.py"],
"candidate_edit_sites":[
 {"path":"util.py","line":1,"snippet":"def compute_total(items):","rationale":"compute_total definition"},
 {"path":"util.py","line":2,"snippet":"total = fabricated_nonsense()","rationale":"made up"}],
"entrypoints_or_repro":[],"open_questions":[],"unread_but_flagged":[]}`
	srv := fakeBackend(t, []translate.OpenAIResponse{
		toolCallResp("c1", "read_file", `{"path":"util.py"}`),
		finalResp(mixed),
	})
	defer srv.Close()
	e := NewEngine(config.Config{WorkerAPIBase: srv.URL})
	res := e.Explore(context.Background(), dir, exploreTask)
	if res.Stopped != "done" {
		t.Fatalf("stopped=%s err=%s", res.Stopped, res.Err)
	}
	if len(res.CandidateEditSites) != 1 {
		t.Fatalf("hallucinated site should be dropped, kept=%+v", res.CandidateEditSites)
	}
	if len(res.RejectedSites) != 1 || !strings.Contains(res.RejectedSites[0], "not found") {
		t.Fatalf("rejection reason missing: %v", res.RejectedSites)
	}
}

func TestParseReportSchema(t *testing.T) {
	if rep, problems := parseReport("prose only, no json"); rep != nil || len(problems) == 0 {
		t.Fatal("prose must fail the schema gate")
	}
	missing := `{"relevant_files":[],"candidate_edit_sites":[],"entrypoints_or_repro":[]}`
	if rep, problems := parseReport(missing); rep != nil || len(problems) != 2 {
		t.Fatalf("expected 2 missing-key problems, got rep=%v problems=%v", rep, problems)
	}
	fenced := "```json\n" + goodReportJSON + "\n```"
	rep, problems := parseReport(fenced)
	if rep == nil {
		t.Fatalf("fenced JSON should parse: %v", problems)
	}
}

func TestVerifySites(t *testing.T) {
	dir := exploreRepo(t)
	sites := []EditSite{
		{Path: "util.py", Line: 1, Snippet: "def compute_total(items):", Rationale: "compute_total"},
		{Path: "util.py", Line: 90, Snippet: "def compute_total(items):", Rationale: "line far off"},
		{Path: "util.py", Line: 2, Snippet: "totally_invented = 1", Rationale: "hallucinated"},
		{Path: "missing.py", Line: 1, Snippet: "def compute_total(items):", Rationale: "wrong file"},
	}
	kept, rejected := verifySites(dir, sites, exploreTask)
	if len(kept) != 1 || kept[0].Line != 1 {
		t.Fatalf("only the verbatim near-line site should survive: %+v", kept)
	}
	if len(rejected) != 3 {
		t.Fatalf("expected 3 rejections, got %v", rejected)
	}
}

func TestComprehensionMinCheck(t *testing.T) {
	dir := exploreRepo(t)
	// Rationale names a code term that exists nowhere near the cited span.
	sites := []EditSite{{Path: "util.py", Line: 1, Snippet: "def compute_total(items):", Rationale: "this is where frobnicate_widget breaks"}}
	if kept, rejected := verifySites(dir, sites, exploreTask); len(kept) != 0 || len(rejected) != 1 {
		t.Fatalf("0d should reject unrelated rationale terms: kept=%v rejected=%v", kept, rejected)
	}
}
