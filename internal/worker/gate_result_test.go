package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/pkg/translate"
)

// Result.LastTest is what `verify` reports and what the checkpoint digest
// re-seeds from, so it has to be the gate — not whatever bash ran last. Live
// fire (B6 runs 1 and 2) found every delegate finishing on `git diff`, so
// last_test came back holding the diff a second time and verify reported a pass
// no test had established. MAIN caught it both times, at the cost of three extra
// implement round-trips.
//
// An inspection command reads the repo's state; a gate runs its code. Only the
// second can verify anything, so only the second is recorded — and the command
// itself travels with the result, so a misjudgement by this classifier is
// visible to review rather than silent.

func TestLastTestSurvivesATrailingGitDiff(t *testing.T) {
	repo := gitRepo(t)
	be := fakeBackend(t, []translate.OpenAIResponse{
		toolCallResp("c1", "write_file", `{"path":"app.py","content":"def f():\n    return 2\n"}`),
		toolCallResp("c2", "run_bash", `{"command":"python3 -c \"import app; print('PASS' if app.f()==2 else 'FAIL')\""}`),
		toolCallResp("c3", "run_bash", `{"command":"git diff"}`),
		finalResp("done"),
	})
	defer be.Close()

	res := NewEngine(config.Config{WorkerAPIBase: be.URL, WorkerModel: "test"}).
		Implement(context.Background(), repo, "make f return 2", "")

	if strings.Contains(res.LastTest, "diff --git") {
		t.Errorf("last_test holds the diff, not the gate:\n%s", res.LastTest)
	}
	if !strings.Contains(res.LastTestCmd, "python3") {
		t.Errorf("last_test_cmd=%q, wanted the gate command", res.LastTestCmd)
	}
}

func TestNoGateRanLeavesVerifyMissing(t *testing.T) {
	repo := gitRepo(t)
	be := fakeBackend(t, []translate.OpenAIResponse{
		toolCallResp("c1", "write_file", `{"path":"app.py","content":"def f():\n    return 2\n"}`),
		toolCallResp("c2", "run_bash", `{"command":"git diff"}`),
		toolCallResp("c3", "run_bash", `{"command":"ls -la"}`),
		finalResp("all tests pass"), // the worker's claim, unbacked
	})
	defer be.Close()

	res := NewEngine(config.Config{WorkerAPIBase: be.URL, WorkerModel: "test"}).
		Implement(context.Background(), repo, "make f return 2", "")

	if res.LastTest != "" {
		t.Errorf("inspection commands were recorded as a gate:\n%s", res.LastTest)
	}

	// And the tiered return must say so rather than inherit a pass.
	out := TierResult(res, repo, 0)
	if out.Verify == nil || out.Verify.Status != "missing" {
		t.Fatalf("verify=%+v, wanted status=missing", out.Verify)
	}
}

func TestVerifyNamesTheCommandItReports(t *testing.T) {
	res := Result{
		Diff:        "",
		LastTest:    "31 passed, 1 xfailed\n[exit 0]",
		LastTestCmd: "python -m pytest sympy/physics/units/tests/ -q",
	}
	out := TierResult(res, t.TempDir(), 0)
	if out.Verify == nil {
		t.Fatal("no verify")
	}
	if out.Verify.Status != "pass" {
		t.Errorf("status=%q", out.Verify.Status)
	}
	if !strings.Contains(out.Verify.Command, "pytest") {
		t.Errorf("verify does not name the command it reports: %+v", out.Verify)
	}
}

func TestGateCommandClassification(t *testing.T) {
	gates := []string{
		"pytest -q",
		"python -m pytest sympy/physics/units/tests/test_unitsystem.py",
		"python3 -c \"import app; print(app.f())\"",
		"go test ./...",
		"npm test",
		"make test",
		"./run_tests.sh",
		"cd sympy && python -m pytest -q", // compound: the runner is what counts
		"bin/rspec spec/",
	}
	for _, c := range gates {
		if !isGateCommand(c) {
			t.Errorf("should count as a gate: %q", c)
		}
	}

	inspections := []string{
		"git diff",
		"git status --short",
		"git diff -- sympy/physics/units/unitsystem.py",
		"ls -la",
		"cat app.py",
		"grep -rn 'def f' .",
		"find . -name '*.py'",
		"head -50 app.py",
		"echo PASS",
		"grep -q 'return 2' app.py && echo PASS", // the shape that faked a pass
		"pwd",
		"wc -l app.py",
	}
	for _, c := range inspections {
		if isGateCommand(c) {
			t.Errorf("should NOT count as a gate: %q", c)
		}
	}
}
