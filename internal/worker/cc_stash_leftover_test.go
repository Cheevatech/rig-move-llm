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

// stashRepo builds a git repo and returns it. run is a helper for git commands
// that must succeed.
func stashRepo(t *testing.T) (repo string, git func(args ...string) string) {
	t.Helper()
	repo = t.TempDir()
	git = func(args ...string) string {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "app.py"), []byte("def f():\n    return 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "init")
	return repo, git
}

// The last open branch of #54, measured against the real worker on 2026-08-03:
// the retry's pop failed, the worker resolved the conflict itself instead of
// stopping as instructed, and then reported "the proof-of-flip is complete" with
// its own stash still on the stack. Prose in the instruction did not hold. The
// engine reads the tree instead.
func TestNoteLeftoverStash(t *testing.T) {
	t.Run("a stash this round did not get back is named", func(t *testing.T) {
		repo, git := stashRepo(t)
		tag := "PROBE"
		if err := os.WriteFile(filepath.Join(repo, "app.py"), []byte("def f():\n    return 2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git("stash", "push", "-u", "-m", ccStashLabel(tag), "-q")

		var res Result
		noteLeftoverStash(context.Background(), NewEngine(config.Config{}), repo, tag, &res)

		if res.StashLeftBehind == "" {
			t.Fatal("a stash left on the stack was not reported")
		}
		if !strings.Contains(res.StashLeftBehind, ccStashLabel(tag)) {
			t.Errorf("the report does not name this round's entry: %q", res.StashLeftBehind)
		}
		for _, want := range []string{"STASH LEFT BEHIND", "did not fully succeed", "git stash show -p stash@{0}"} {
			if !strings.Contains(res.Summary, want) {
				t.Errorf("summary missing %q:\n%s", want, res.Summary)
			}
		}
		// A stash holds work. Reporting it is the job; discarding it is not.
		if out := git("stash", "list"); !strings.Contains(out, ccStashLabel(tag)) {
			t.Error("the engine dropped the stash — that destroys work to tidy up")
		}
	})

	t.Run("a pop that worked says nothing", func(t *testing.T) {
		repo, git := stashRepo(t)
		tag := "PROBE"
		if err := os.WriteFile(filepath.Join(repo, "app.py"), []byte("def f():\n    return 2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git("stash", "push", "-u", "-m", ccStashLabel(tag), "-q")
		git("stash", "pop", "-q")

		var res Result
		noteLeftoverStash(context.Background(), NewEngine(config.Config{}), repo, tag, &res)

		if res.StashLeftBehind != "" || strings.Contains(res.Summary, "STASH LEFT BEHIND") {
			t.Errorf("a clean round was flagged: %q / %q", res.StashLeftBehind, res.Summary)
		}
	})

	t.Run("another session's stash is not this round's to judge", func(t *testing.T) {
		repo, git := stashRepo(t)
		// The #54 lesson in the other direction: entries left by earlier sessions
		// are not ours to claim, drop, or report as our failure.
		if err := os.WriteFile(filepath.Join(repo, "app.py"), []byte("def f():\n    return 3\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git("stash", "push", "-u", "-m", ccStashLabel("SOMEONE-ELSE"), "-q")

		var res Result
		noteLeftoverStash(context.Background(), NewEngine(config.Config{}), repo, "PROBE", &res)

		if res.StashLeftBehind != "" {
			t.Errorf("claimed a stranger's stash: %q", res.StashLeftBehind)
		}
	})

	t.Run("a round that never stashed is not checked at all", func(t *testing.T) {
		repo, git := stashRepo(t)
		if err := os.WriteFile(filepath.Join(repo, "app.py"), []byte("def f():\n    return 4\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git("stash", "push", "-u", "-m", ccStashLabel("WHATEVER"), "-q")

		var res Result
		noteLeftoverStash(context.Background(), NewEngine(config.Config{}), repo, "", &res)

		if res.StashLeftBehind != "" || res.Summary != "" {
			t.Errorf("an empty tag must mean no check: %q / %q", res.StashLeftBehind, res.Summary)
		}
	})
}
