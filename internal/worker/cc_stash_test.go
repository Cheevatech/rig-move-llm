package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// #54: the proof retry used to tell the worker to run a bare `git stash pop`.
// Pop is positional — it restores stash@{0}, whoever pushed it. A leftover
// stash from an earlier session (dogfood carried one for a whole day) sits on
// top of the stack, so the retry restored a stranger's work over the round's
// own, left conflict markers in a source file, and destroyed 21 iterations of
// finished work in the measured run that found this.

// shRun runs a command string the way the worker's shell would, so the test
// exercises the exact text the instruction hands it — including the `$(...)`
// that resolves the stash ref.
func shRun(t *testing.T, dir, script string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// repoWithForeignStash builds the shape that broke: a repo whose stash stack
// already holds an unrelated entry from an earlier session.
func repoWithForeignStash(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "config", "user.email", "test@local")
	gitRun(t, dir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "source.go"), []byte("package p\n\nconst Original = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "base")

	// The stranger: a stash left behind by a previous session, touching the same
	// file so a positional pop conflicts rather than merging silently.
	if err := os.WriteFile(filepath.Join(dir, "source.go"), []byte("package p\n\nconst Original = 999 // stranger\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "stash", "push", "-m", "rig-fix")
	return dir
}

// applyRoundWork puts this round's finished work in the tree: an edit to a
// tracked file plus a file the session created, which is why the dance needs -u.
func applyRoundWork(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "source.go"), []byte("package p\n\nconst Original = 2 // the round's fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "newfile.go"), []byte("package p\n\nconst Added = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCCStashRoundTripSurvivesAForeignStash(t *testing.T) {
	dir := repoWithForeignStash(t)
	applyRoundWork(t, dir)

	tag := ccStashTag()
	if out, err := shRun(t, dir, ccStashPushCmd(tag)); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}

	// Red state: the round's work is gone from the tree, both the edit and the
	// created file — that is the whole point of the revert.
	if b, _ := os.ReadFile(filepath.Join(dir, "source.go")); !strings.Contains(string(b), "Original = 1") {
		t.Fatalf("edit not reverted, tree has: %s", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "newfile.go")); !os.IsNotExist(err) {
		t.Fatal("created file survived the stash push -u; red state is impossible")
	}

	if out, err := shRun(t, dir, ccStashPopCmd(tag)); err != nil {
		t.Fatalf("pop: %v\n%s", err, out)
	}

	// Green state: the round's own work is back, byte for byte.
	b, err := os.ReadFile(filepath.Join(dir, "source.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "the round's fix") {
		t.Errorf("the round's own work was not restored, tree has: %s", b)
	}
	if strings.Contains(string(b), "stranger") {
		t.Errorf("popped the foreign stash over the round's work — this is #54: %s", b)
	}
	if strings.Contains(string(b), "<<<<<<<") {
		t.Errorf("conflict markers left in a source file — this is #54: %s", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "newfile.go")); err != nil {
		t.Errorf("created file not restored: %v", err)
	}

	// The stranger's stash is not ours to consume: it must still be on the stack.
	if list := gitRun(t, dir, "stash", "list"); !strings.Contains(list, "rig-fix") {
		t.Errorf("the foreign stash was consumed; stack is now: %q", list)
	}
}

// This is the sequence that actually corrupted dogfood, reconstructed from the
// worker transcript: `git stash push -u -m rig-fix` ran 25 times and `git stash
// pop` 23 times in one session, so a push inevitably landed on a tree with
// nothing left to save. That push is a silent no-op — git prints "No local
// changes to save" and exits 0 — and the positional pop that follows therefore
// restores whatever was already on the stack. On dogfood that was a stash from
// the previous day's volley, whose base was a different commit, so the merge
// conflicted and wrote markers into a source file that had nothing to do with it.
func TestCCStashNoOpPushDoesNotLeaveAForeignStashOnTop(t *testing.T) {
	dir := repoWithForeignStash(t)
	// Work landed and was committed: the tree is clean, so there is nothing for
	// the push to save.
	if err := os.WriteFile(filepath.Join(dir, "source.go"), []byte("package p\n\nconst Original = 2 // the round's fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "round work")

	tag := ccStashTag()
	shRun(t, dir, ccStashPushCmd(tag)) // no-op: "No local changes to save", exit 0
	shRun(t, dir, ccStashPopCmd(tag))

	b, err := os.ReadFile(filepath.Join(dir, "source.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "<<<<<<<") {
		t.Errorf("pop reached past this round and merged a foreign stash, leaving conflict markers — this is #54:\n%s", b)
	}
	if strings.Contains(string(b), "stranger") {
		t.Errorf("pop restored a foreign stash over the round's work — this is #54:\n%s", b)
	}
	if !strings.Contains(string(b), "the round's fix") {
		t.Errorf("the round's work did not survive:\n%s", b)
	}
	if list := gitRun(t, dir, "stash", "list"); !strings.Contains(list, "rig-fix") {
		t.Errorf("the foreign stash was consumed; stack is now: %q", list)
	}
}

func TestCCStashPopResolvesRefRatherThanPosition(t *testing.T) {
	tag := ccStashTag()
	pop := ccStashPopCmd(tag)
	if !strings.Contains(pop, tag) {
		t.Errorf("pop command does not name this round's stash: %q", pop)
	}
	// A pop with no ref takes whatever is on top — the defect itself.
	if strings.TrimSpace(pop) == "git stash pop" {
		t.Error("pop is positional; it must resolve this round's own stash ref")
	}
	if !strings.Contains(pop, "git stash list") {
		t.Errorf("pop does not resolve the ref from the stash list: %q", pop)
	}
}

func TestCCStashTagIsPerRound(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tag := ccStashTag()
		if tag == "" {
			t.Fatal("empty stash tag: every round would share one label")
		}
		if seen[tag] {
			t.Fatalf("stash tag repeated (%q) — a leftover from an earlier round would match this one", tag)
		}
		seen[tag] = true
	}
}

func TestCCProofRetryInstrCarriesTheRoundsOwnTag(t *testing.T) {
	tag := ccStashTag()
	instr := ccProofRetryInstrFor(tag)
	if !strings.Contains(instr, ccStashPushCmd(tag)) {
		t.Error("retry instruction does not hand the worker the tagged push command")
	}
	if !strings.Contains(instr, ccStashPopCmd(tag)) {
		t.Error("retry instruction does not hand the worker the ref-resolving pop command")
	}
	// The old text told the worker to resolve a conflicting pop by keeping the
	// stashed side. With a correctly resolved ref a conflict means something the
	// worker cannot safely guess at, so it must stop rather than pick a side.
	if strings.Contains(instr, "KEEPING the stashed version") {
		t.Error("instruction still tells the worker to resolve a pop conflict by guessing")
	}
}
