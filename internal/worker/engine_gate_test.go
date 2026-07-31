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
	if !strings.Contains(res.Err, "engine gate ran") {
		t.Errorf("diagnosis missing engine gate result:\n%s", res.Err)
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

// TestEngineGateNormalRoundUnchanged verifies that a normal (non-killed) round
// is unaffected — no engine-run marker, GateSource stays empty.
func TestEngineGateNormalRoundUnchanged(t *testing.T) {
	repo := ccTestRepo(t)
	bin, _ := ccFakeBin(t, t.TempDir(), ccHappyStream)
	ccEnv(t, bin)

	e := NewEngine(config.Config{})
	res := e.Implement(context.Background(), repo, "fix the bug", "")

	if res.Stopped != "done" {
		t.Fatalf("stopped=%q, want done", res.Stopped)
	}
	if res.GateSource != "" {
		t.Errorf("gate_source = %q, want empty for normal round", res.GateSource)
	}
	if strings.Contains(res.LastTest, "ENGINE-RUN") {
		t.Errorf("last_test should not contain engine marker on normal round: %q", res.LastTest)
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
