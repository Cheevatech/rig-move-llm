package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/config"
)

// --- Tests that drive applyEngineGate directly (no live worker needed) -------

func ccTestGoRepo(t *testing.T) string {
	t.Helper()
	repo := ccTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A simple Go file so `go build` succeeds.
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc main(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Re-commit the new files.
	c := exec.Command("git", "-C", repo, "add", ".")
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	c = exec.Command("git", "-C", repo, "commit", "-q", "-m", "add go")
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	return repo
}

// a. TestEngineGateNormalRoundWithDiff — a Stopped=="done" round with a
// non-empty diff in a real Go-shaped temp repo.
func TestEngineGateNormalRoundWithDiff(t *testing.T) {
	repo := ccTestGoRepo(t)
	bin, _ := ccFakeBin(t, t.TempDir(), ccHappyStream)
	ccEnv(t, bin)

	e := NewEngine(config.Config{})
	res := e.Implement(context.Background(), repo, "fix the bug", "")

	if res.Stopped != "done" {
		t.Fatalf("stopped=%q, want done", res.Stopped)
	}
	if res.GateSource != "engine" {
		t.Errorf("GateSource=%q, want engine", res.GateSource)
	}
	if res.GateVerdict != "pass" && res.GateVerdict != "fail" {
		t.Errorf("GateVerdict=%q, want pass or fail", res.GateVerdict)
	}
	if res.EngineGateCmd == "" {
		t.Error("EngineGateCmd is empty, want the detected verify command")
	}
	// LastTest/LastTestCmd must still be the worker's own claim, not the
	// engine's measurement.
	if strings.Contains(res.LastTest, "[ENGINE-RUN GATE") {
		t.Errorf("LastTest contains engine marker — it should only hold the worker's output: %q", res.LastTest)
	}
	if res.LastTestCmd == "[ENGINE-RUN GATE" || strings.HasPrefix(res.LastTestCmd, "[ENGINE-RUN GATE") {
		t.Errorf("LastTestCmd was overwritten with engine output: %q", res.LastTestCmd)
	}
}

// b. TestEngineGateEmptyDiffSkipped — a round with an empty diff.
func TestEngineGateEmptyDiffSkipped(t *testing.T) {
	repo := ccTestGoRepo(t)
	// Fake claude that does not edit any file but returns success.
	stream := `{"type":"result","subtype":"success","result":"done.","num_turns":1,"usage":{"input_tokens":1,"output_tokens":1}}` + "\n"
	bin, _ := ccFakeBin(t, t.TempDir(), stream)
	// Use a fake that doesn't write to file.txt.
	binPath := filepath.Join(t.TempDir(), "fake-claude-nodiff")
	script := "#!/bin/sh\necho 'done.'\n" +
		"echo '" + strings.ReplaceAll(stream, "'", "'\\''") + "'\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	bin = binPath

	ccEnv(t, bin)

	e := NewEngine(config.Config{})
	res := e.Implement(context.Background(), repo, "fix the bug", "")

	if res.Stopped != "done" {
		t.Fatalf("stopped=%q, want done", res.Stopped)
	}
	if res.GateSource != "" {
		t.Errorf("GateSource=%q, want empty for empty diff", res.GateSource)
	}
	if res.GateVerdict != "" {
		t.Errorf("GateVerdict=%q, want empty for empty diff", res.GateVerdict)
	}
	if res.EngineGateCmd != "" {
		t.Errorf("EngineGateCmd=%q, want empty for empty diff", res.EngineGateCmd)
	}
	if res.EngineGateOutput != "" {
		t.Errorf("EngineGateOutput=%q, want empty for empty diff", res.EngineGateOutput)
	}
	if strings.Contains(res.Summary, "ENGINE GATE") {
		t.Errorf("Summary carries engine-gate note despite empty diff: %q", res.Summary)
	}
}

// c. TestEngineGateReportsBothVerdictsOnDisagreement — worker claims pass while
// the engine measures fail (a temp Go repo that does not compile, with
// res.LastTest set to a passing-looking string).
func TestEngineGateReportsBothVerdictsOnDisagreement(t *testing.T) {
	repo := t.TempDir()
	// A Go repo with broken syntax.
	c := exec.Command("git", "-C", repo, "init", "-q")
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc main(){ BROKEN SYNTAX },\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c = exec.Command("git", "-C", repo, "add", ".")
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	c = exec.Command("git", "-C", repo, "commit", "-q", "-m", "init")
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	res := Result{
		Summary:  "Fixed the bug.",
		Diff:     "@@ -1 +1 @@\n-main\n+fixed\n",
		LastTest: "1 passed in 0.01s\nall green\n",
		Stopped:  "done",
	}
	applyEngineGate(&res, repo)

	if res.WorkerVerdict != "pass" {
		t.Errorf("WorkerVerdict=%q, want pass", res.WorkerVerdict)
	}
	if res.GateVerdict != "fail" {
		t.Errorf("GateVerdict=%q, want fail (code does not compile)", res.GateVerdict)
	}
	if !strings.Contains(res.Summary, "ENGINE GATE DISAGREES WITH THE WORKER") {
		t.Errorf("Summary missing disagreement note:\n%s", res.Summary)
	}
	if !strings.Contains(res.Summary, "pass") {
		t.Error("Summary should contain the worker's 'pass' verdict")
	}
	if !strings.Contains(res.Summary, "fail") {
		t.Error("Summary should contain the engine's 'fail' verdict")
	}
}

// d. TestEngineGateCannotRunReportsWhy — a temp dir with no recognised repo
// shape: GateVerdict stays empty (NOT "fail"), Err contains "no recognised
// repo shape", and Summary contains "absence of a verdict, not a failing verdict".
func TestEngineGateCannotRunReportsWhy(t *testing.T) {
	dir := t.TempDir()
	// Not a git repo, no go.mod, no pyproject.toml — nothing recognised.
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := Result{
		Summary: "Fixed the bug.",
		Diff:    "@@ -1 +1 @@\n-old\n+new\n",
		Stopped: "done",
	}
	applyEngineGate(&res, dir)

	if res.GateVerdict != "" {
		t.Errorf("GateVerdict=%q, want empty (gate cannot run)", res.GateVerdict)
	}
	if !strings.Contains(res.Err, "no recognised repo shape") {
		t.Errorf("Err missing 'no recognised repo shape':\n%s", res.Err)
	}
	if !strings.Contains(res.Summary, "absence of a verdict, not a failing verdict") {
		t.Errorf("Summary missing 'absence of a verdict' note:\n%s", res.Summary)
	}
}

// e. TestEngineGateNoteShapes — direct table-driven unit test of engineGateNote
// over agree / disagree / unknown-worker-verdict / not-run.
func TestEngineGateNoteShapes(t *testing.T) {
	tests := []struct {
		name           string
		o              gateOutcome
		workerVerdict  string
		mustContain    []string
		mustNotContain []string
	}{
		{
			name:          "agree",
			o:             gateOutcome{Ran: true, Verdict: "pass", Cmd: "go build ./... && go test ./..."},
			workerVerdict: "pass",
			mustContain:   []string{"ENGINE GATE: pass", "WORKER CLAIMED: pass", "Agreed"},
		},
		{
			name:          "disagree",
			o:             gateOutcome{Ran: true, Verdict: "fail", Cmd: "pytest -q"},
			workerVerdict: "pass",
			mustContain:   []string{"ENGINE GATE DISAGREES WITH THE WORKER", "fail", "pass", "not evidence"},
		},
		{
			// No claim is not a disagreement: a round that ran no test of its own
			// must not read as a worker caught lying, or the caller is pushed to
			// distrust a good round (the false-reject failure this gate prevents).
			name:           "unknown worker verdict",
			o:              gateOutcome{Ran: true, Verdict: "pass", Cmd: "go test ./..."},
			workerVerdict:  "unknown",
			mustContain:    []string{"ENGINE GATE: pass", "no test result of its own", "only verdict"},
			mustNotContain: []string{"DISAGREES", "not evidence"},
		},
		{
			name:          "not run",
			o:             gateOutcome{Ran: false, NotRunReason: "no recognised repo shape"},
			workerVerdict: "pass",
			mustContain:   []string{"ENGINE GATE NOT RUN", "no recognised repo shape", "absence of a verdict, not a failing verdict", "unverified"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := engineGateNote(tt.o, tt.workerVerdict)
			for _, wc := range tt.mustContain {
				if !strings.Contains(got, wc) {
					t.Errorf("engineGateNote output missing %q:\n%s", wc, got)
				}
			}
			for _, wn := range tt.mustNotContain {
				if strings.Contains(got, wn) {
					t.Errorf("engineGateNote output should not contain %q:\n%s", wn, got)
				}
			}
		})
	}
}
