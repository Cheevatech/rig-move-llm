package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/config"
)

// TestEngineGateKilledRoundWithWork attaches a gate verdict when a killed round
// has a non-empty diff and a recognised repo shape.
func TestEngineGateKilledRoundWithWork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake worker is a shell script")
	}
	repo := setupTestRepoWithGoMod(t)
	bin := filepath.Join(t.TempDir(), "fake-claude")
	// Fake claude that writes a file and stalls (no result event)
	script := "#!/bin/sh\n" +
		`echo '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"1","name":"Edit","input":{"file_path":"file.txt"}}]}}'` + "\n" +
		"echo edited >> file.txt\n" +
		"sleep 120\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RIG_WORKER_ENGINE", "cc")
	t.Setenv("RIG_CC_BIN", bin)
	t.Setenv("RIG_CC_BASE_URL", "http://127.0.0.1:9/worker-leg")
	t.Setenv("RIG_CC_STALL_TIMEOUT", "1")

	e := NewEngine(config.Config{})
	start := time.Now()
	res := e.Implement(context.Background(), repo, "task", "")

	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("stall guard did not fire: call took %s", elapsed)
	}
	if res.Stopped != "timeout" {
		t.Fatalf("stopped = %q, want timeout", res.Stopped)
	}
	if res.GateSource != "engine" {
		t.Errorf("gate_source = %q, want engine", res.GateSource)
	}
	if !strings.Contains(res.LastTest, "[ENGINE-RUN GATE") {
		t.Errorf("last_test missing engine marker: %q", res.LastTest)
	}
	if res.LastTestCmd != "go build ./... && go test ./..." {
		t.Errorf("last_test_cmd = %q, want go build+test", res.LastTestCmd)
	}
	// The verdict is a value, not prose in the diagnosis: review must be able to
	// read pass/fail without parsing Err.
	if res.GateVerdict != "pass" && res.GateVerdict != "fail" {
		t.Errorf("gate_verdict = %q, want pass or fail", res.GateVerdict)
	}
}

// TestEngineGateUnrecognizedShape returns a diagnosis explaining why the gate
// was not run when the repo has no recognised shape.
func TestEngineGateUnrecognizedShape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake worker is a shell script")
	}
	repo := t.TempDir()
	// Git repo but no go.mod, no pyproject.toml — nothing recognised
	runGit := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".")
	runGit("commit", "-q", "-m", "init")

	bin := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/sh\n" +
		`echo '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"1","name":"Edit","input":{"file_path":"file.txt"}}]}}'` + "\n" +
		"echo edited >> file.txt\n" +
		"sleep 120\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RIG_WORKER_ENGINE", "cc")
	t.Setenv("RIG_CC_BIN", bin)
	t.Setenv("RIG_CC_BASE_URL", "http://127.0.0.1:9/worker-leg")
	t.Setenv("RIG_CC_STALL_TIMEOUT", "1")

	e := NewEngine(config.Config{})
	res := e.Implement(context.Background(), repo, "task", "")

	if res.Stopped != "timeout" {
		t.Fatalf("stopped = %q, want timeout", res.Stopped)
	}
	if res.GateSource != "" {
		t.Errorf("gate_source = %q, want empty", res.GateSource)
	}
	if res.LastTest != "" {
		t.Errorf("last_test = %q, want empty", res.LastTest)
	}
	if !strings.Contains(res.Err, "no recognised repo shape") {
		t.Errorf("diagnosis missing unrecognised shape note:\n%s", res.Err)
	}
}

// TestEngineGateNormalRoundKeepsTheWorkersClaimSeparate.
//
// This test used to assert that a normal round leaves GateSource empty. That
// premise died at #46, which gates EVERY round with a diff — it only kept
// passing because ccTestRepo has no recognisable shape, so no gate could run.
// #59 gave that repo a gate (the worker's own command), which is what made the
// stale premise visible: it failed on Linux CI, where `python` exists, and
// passed on macOS only because `python` does not and the run was classified as
// the gate being inapplicable. A test that passes for a reason unrelated to what
// it claims is worse than no test.
//
// What it was really protecting, and still does: the engine's own measurement
// must never masquerade as the worker's claim. LastTest is what the WORKER
// demonstrated; the engine's number lives in its own fields. A caller that
// cannot tell the two apart cannot detect the round where they disagree.
func TestEngineGateNormalRoundKeepsTheWorkersClaimSeparate(t *testing.T) {
	repo := ccTestRepo(t)
	bin, _ := ccFakeBin(t, t.TempDir(), ccHappyStream)
	ccEnv(t, bin)

	e := NewEngine(config.Config{})
	res := e.Implement(context.Background(), repo, "fix the bug", "")

	if res.Stopped != "done" {
		t.Fatalf("stopped=%q, want done", res.Stopped)
	}
	if strings.Contains(res.LastTest, "ENGINE-RUN") {
		t.Errorf("last_test carries the engine marker on a normal round: %q", res.LastTest)
	}
	// The worker's claim is its own last gate output, untouched by the engine.
	if !strings.Contains(res.LastTest, "1 passed") {
		t.Errorf("last_test should hold the worker's own gate output, got %q", res.LastTest)
	}
	if res.WorkerVerdict != "pass" {
		t.Errorf("worker_verdict = %q, want the worker's own claim", res.WorkerVerdict)
	}
	// Whatever the engine measured, it is reported as the engine's, separately.
	if res.GateSource != "" && res.GateSource != "engine" {
		t.Errorf("gate_source = %q, want empty or engine", res.GateSource)
	}
	if res.GateVerdict != "" && res.EngineGateCmd == "" {
		t.Error("a verdict with no command behind it is not a measurement")
	}
}

// The engine must not invent a gate. With no repo shape AND no gate command from
// the worker, the honest answer is that nothing verified this round — #59's
// fallback widens where a gate comes from, it does not lower the bar for
// claiming one ran.
func TestEngineGateInventsNothingWithoutAGate(t *testing.T) {
	repo := ccTestRepo(t) // no marker of any kind
	// A worker that only ever inspected: `git diff` runs no code, so there is
	// nothing here that could stand in for a gate.
	stream := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"git diff"}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"diff --git a/file.txt"}]}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"w1","name":"Write","input":{"file_path":"file.txt"}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"w1","content":"ok"}]}}` + "\n" +
		`{"type":"result","subtype":"success","result":"changed it","num_turns":2,"usage":{"input_tokens":1,"output_tokens":1}}` + "\n"
	bin, _ := ccFakeBin(t, t.TempDir(), stream)
	ccEnv(t, bin)

	e := NewEngine(config.Config{})
	res := e.Implement(context.Background(), repo, "fix the bug", "")

	if res.GateVerdict != "" {
		t.Errorf("gate_verdict = %q — the engine invented a verdict with no gate to run", res.GateVerdict)
	}
	if !strings.Contains(res.Err, "no recognised repo shape") {
		t.Errorf("the absence of a gate must be stated, got err=%q", res.Err)
	}
}

// TestEngineGateKilledRoundEmptyDiff does not run the gate when the killed
// round has no work on disk.
func TestEngineGateKilledRoundEmptyDiff(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake worker is a shell script")
	}
	repo := ccTestRepo(t) // has a committed file but no edits
	bin := filepath.Join(t.TempDir(), "fake-claude")
	// Fake claude that produces no file edits and stalls
	script := "#!/bin/sh\n" +
		`echo '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"1","name":"TodoWrite","input":{}}]}}'` + "\n" +
		"sleep 120\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RIG_WORKER_ENGINE", "cc")
	t.Setenv("RIG_CC_BIN", bin)
	t.Setenv("RIG_CC_BASE_URL", "http://127.0.0.1:9/worker-leg")
	t.Setenv("RIG_CC_STALL_TIMEOUT", "1")

	e := NewEngine(config.Config{})
	res := e.Implement(context.Background(), repo, "task", "")

	if res.Stopped != "timeout" {
		t.Fatalf("stopped = %q, want timeout", res.Stopped)
	}
	if res.GateSource != "" {
		t.Errorf("gate_source = %q, want empty — no work on disk", res.GateSource)
	}
	if strings.Contains(res.Err, "engine gate") {
		// The gate should not be mentioned at all when diff is empty.
		t.Errorf("diagnosis should not mention engine gate when diff is empty:\n%s", res.Err)
	}
}

// setupTestRepoWithGoMod builds a throwaway git repo with go.mod so the
// detector recognises it as a Go project.
func setupTestRepoWithGoMod(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGit := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".")
	runGit("commit", "-q", "-m", "init")
	return repo
}
