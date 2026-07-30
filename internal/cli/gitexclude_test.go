package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return repo
}

func TestExcludeRigArtifacts(t *testing.T) {
	repo := gitRepo(t)

	n, err := excludeRigArtifacts(repo)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(rigArtifacts) {
		t.Fatalf("added %d entries, want %d", n, len(rigArtifacts))
	}

	body, err := os.ReadFile(filepath.Join(repo, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range rigArtifacts {
		if !strings.Contains(string(body), p) {
			t.Errorf("exclude file missing %q:\n%s", p, body)
		}
	}
	if !strings.Contains(string(body), rigExcludeHeader) {
		t.Error("rig's block is unlabelled — a human reading the file cannot tell who wrote it")
	}

	// git must actually stop reporting them, which is the whole point: this is what
	// keeps them out of the worker's returned diff and out of `git stash -u`.
	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".claude", "settings.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".mcp.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "real.py"), []byte("x=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", repo, "ls-files", "--others", "--exclude-standard").Output()
	if err != nil {
		t.Fatal(err)
	}
	listed := strings.TrimSpace(string(out))
	if strings.Contains(listed, ".claude") || strings.Contains(listed, ".mcp.json") {
		t.Errorf("git still reports rig wiring as the user's untracked work:\n%s", listed)
	}
	if !strings.Contains(listed, "real.py") {
		t.Errorf("a real file went missing from git's view:\n%s", listed)
	}
}

// Re-running init must not stack duplicates.
func TestExcludeRigArtifactsIsIdempotent(t *testing.T) {
	repo := gitRepo(t)
	if _, err := excludeRigArtifacts(repo); err != nil {
		t.Fatal(err)
	}
	n, err := excludeRigArtifacts(repo)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("second run added %d entries, want 0", n)
	}
	body, _ := os.ReadFile(filepath.Join(repo, ".git", "info", "exclude"))
	if strings.Count(string(body), ".mcp.json") != 1 {
		t.Errorf("entries were duplicated:\n%s", body)
	}
}

// The user's own exclude entries are theirs; rig appends and never rewrites.
func TestExcludeRigArtifactsPreservesExistingEntries(t *testing.T) {
	repo := gitRepo(t)
	path := filepath.Join(repo, ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# mine\nscratch/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := excludeRigArtifacts(repo); err != nil {
		t.Fatal(err)
	}

	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "scratch/") || !strings.Contains(string(body), "# mine") {
		t.Errorf("the user's entries were lost:\n%s", body)
	}
}

// A worktree or submodule has .git as a FILE pointing elsewhere; rig must follow
// it rather than crash or silently write nothing.
func TestExcludeRigArtifactsFollowsAGitFile(t *testing.T) {
	base := t.TempDir()
	realGit := filepath.Join(base, "actual-git-dir")
	if err := os.MkdirAll(realGit, 0o755); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, ".git"), []byte("gitdir: ../actual-git-dir\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	n, err := excludeRigArtifacts(work)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(rigArtifacts) {
		t.Fatalf("added %d entries in a worktree layout, want %d", n, len(rigArtifacts))
	}
	if _, err := os.Stat(filepath.Join(realGit, "info", "exclude")); err != nil {
		t.Errorf("exclude not written into the real git dir: %v", err)
	}
}

// A plain directory is not an error: init works outside git too.
func TestExcludeRigArtifactsOutsideGitIsNoOp(t *testing.T) {
	n, err := excludeRigArtifacts(t.TempDir())
	if err != nil || n != 0 {
		t.Fatalf("outside a repo: n=%d err=%v, want 0/nil", n, err)
	}
}
