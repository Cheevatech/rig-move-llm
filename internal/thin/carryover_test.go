package thin

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// internal/worker has 32 test files. Most of them guard the contract layer and
// die with it, correctly. These are the ones that did NOT — they guard the
// switch itself, and the trap S2 names is deleting them by association. Carried
// over here, each against the behaviour it originally pinned:
//
//	untracked_diff_test.go   -> TestDiffIncludesNewFiles, TestDiffExcludesRigsOwnFiles
//	timeout_diff_test.go     -> TestKilledRunStillReturnsTheDiff (killpath_test.go)
//	cc_test.go (spawn half)  -> TestChildEnvStripsRigVariables, TestRunRefusesWithoutALocalBaseURL,
//	                            TestVersionSkewIsDiagnosedNotSwallowed (surface_test.go, here)
//	cc_stall_test.go         -> TestStallGuardKillsSilentWorker (killpath_test.go)
//	procgroup_unix_test.go   -> the whole of killpath_test.go
//	instrument_test.go (log) -> TestRunWritesItsEvidenceToTheLogDir
//
// The rest — proof, tier, drill, triage, explore, gate, refund, ctx budget,
// parity — have nothing left to test here by design.

func repoWithCommit(t *testing.T) string {
	t.Helper()
	dir := gitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "kept.txt"), []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

// #26: `git diff` shows tracked files only, so a worker that CREATED files
// returned an empty diff — indistinguishable from doing nothing, on the tasks
// where the new file was the entire point.
func TestDiffIncludesNewFiles(t *testing.T) {
	repo := repoWithCommit(t)
	if err := os.WriteFile(filepath.Join(repo, "brand-new.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "kept.txt"), []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff, stat := collectDiff(context.Background(), repo)
	for _, want := range []string{"brand-new.txt", "hello", "kept.txt", "after"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff is missing %q:\n%s", want, diff)
		}
	}
	if !strings.Contains(stat, "brand-new.txt") {
		t.Errorf("stat does not name the new file:\n%s", stat)
	}
}

// rig's own bookkeeping is not part of the change a human is reviewing.
func TestDiffExcludesRigsOwnFiles(t *testing.T) {
	repo := repoWithCommit(t)
	for _, dir := range []string{".rig-move-llm", ".claude"} {
		if err := os.MkdirAll(filepath.Join(repo, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, dir, "state"), []byte("noise\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	diff, _ := collectDiff(context.Background(), repo)
	if strings.Contains(diff, "noise") {
		t.Errorf("rig's own files leaked into the diff:\n%s", diff)
	}
}

// map10 P2: a claude CLI whose output is not the stream-json this parser knows
// must surface as a diagnosis naming the skew, never as a silent empty return.
func TestVersionSkewIsDiagnosedNotSwallowed(t *testing.T) {
	repo := gitRepo(t)
	stub := filepath.Join(t.TempDir(), "fake-claude")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho 'not json at all'\necho 'nor this'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	thinEnv(t, stub, t.TempDir())

	out := Run(context.Background(), repo, "whatever")
	if !strings.Contains(out.Status, "version skew") {
		t.Fatalf("status = %q, want a diagnosis naming the skew", out.Status)
	}
	if !strings.HasPrefix(out.Status, "error:") {
		t.Errorf("a skewed CLI must be an error, not a quiet success: %q", out.Status)
	}
}

// The return quotes a log path (S1), so there has to be something at it. A run
// whose evidence is not on disk cannot be reviewed after the fact.
func TestRunWritesItsEvidenceToTheLogDir(t *testing.T) {
	repo := gitRepo(t)
	stub := filepath.Join(t.TempDir(), "fake-claude")
	// A stub that emits one well-formed terminal event, so the run ends cleanly.
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho '{\"type\":\"result\",\"subtype\":\"success\"}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	thinEnv(t, stub, t.TempDir())

	out := Run(context.Background(), repo, "a task worth recording")
	if out.Status != statusFinished {
		t.Fatalf("status = %q, want finished", out.Status)
	}
	for _, name := range []string{"command.txt", "task.txt", "stream.jsonl", "diff.patch"} {
		if _, err := os.Stat(filepath.Join(out.LogDir, name)); err != nil {
			t.Errorf("%s is missing from the run log: %v", name, err)
		}
	}
	task, _ := os.ReadFile(filepath.Join(out.LogDir, "task.txt"))
	if !strings.Contains(string(task), "a task worth recording") {
		t.Errorf("the run log does not record the task it was given: %q", task)
	}
}

// The last command is the fourth section of the return, and it is read straight
// off the stream rather than taken from anything the worker says about itself.
func TestLastCommandComesFromTheStream(t *testing.T) {
	repo := gitRepo(t)
	stub := filepath.Join(t.TempDir(), "fake-claude")
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","tools":["Bash","Read","Edit"]}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"pytest -q"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"1939 passed"}]}}`,
		`{"type":"result","subtype":"success"}`,
	}, "'\necho '")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho '"+stream+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	thinEnv(t, stub, t.TempDir())

	out := Run(context.Background(), repo, "run the tests")
	if !strings.Contains(out.LastCommand, "pytest -q") || !strings.Contains(out.LastCommand, "1939 passed") {
		t.Fatalf("last command = %q, want the command and its output", out.LastCommand)
	}
	inv, err := os.ReadFile(filepath.Join(out.LogDir, "inventory.txt"))
	if err != nil || !strings.Contains(string(inv), "Bash") {
		t.Errorf("the init event's tool inventory was not recorded (err=%v): %q", err, inv)
	}
}

// The compact branch of the return has to stay compact. Measured in the S3 live
// fire: an un-ignored .venv put ~8 KB of filenames into the stat — the branch
// taken *because* the diff was too big to paste — and MAIN spent a turn telling
// the human those files were not the change.
func TestNewFileNoiseDoesNotDrownTheChange(t *testing.T) {
	repo := repoWithCommit(t)
	// The real change.
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("the actual work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A virtualenv the user never gitignored, plus rig's and Serena's bookkeeping.
	for _, p := range []string{
		".venv/bin/python", ".venv/lib/python3.12/site-packages/pytest/__init__.py",
		"__pycache__/mod.cpython-312.pyc", "node_modules/left-pad/index.js",
		".serena/project.yml", ".rig-move-llm/config.env",
	} {
		full := filepath.Join(repo, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("noise\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	diff, stat := collectDiff(context.Background(), repo)

	if !strings.Contains(diff, "the actual work") {
		t.Errorf("the real change is missing from the diff:\n%s", stat)
	}
	for _, noise := range []string{".venv", "node_modules", "__pycache__", ".serena", ".rig-move-llm"} {
		if strings.Contains(diff, noise) {
			t.Errorf("%s reached the diff a human is meant to read", noise)
		}
		if strings.Contains(stat, noise) {
			t.Errorf("%s reached the stat", noise)
		}
	}
}

// When there genuinely are many new files, the stat names some and counts the
// rest rather than listing all of them.
func TestManyNewFilesAreCountedNotListed(t *testing.T) {
	repo := repoWithCommit(t)
	for i := 0; i < maxNamedNewFiles+8; i++ {
		name := filepath.Join(repo, "gen", "file"+strconv.Itoa(i)+".txt")
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, stat := collectDiff(context.Background(), repo)
	if !strings.Contains(stat, "and 8 more") {
		t.Errorf("the stat listed every new file instead of summarizing:\n%s", stat)
	}
}
