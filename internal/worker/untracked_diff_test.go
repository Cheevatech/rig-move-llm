package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func untrackedRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	if err := os.WriteFile(filepath.Join(repo, "tracked.py"), []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "tracked.py")
	run("commit", "-m", "init")
	return repo
}

func write(t *testing.T, repo, rel, body string) {
	t.Helper()
	p := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// #26, as measured: a worker that CREATES a file returned diff "" and
// files_changed [] — MAIN could not review work it was told did not exist, so it
// re-delegated instead.
func TestCollectDiffIncludesNewFiles(t *testing.T) {
	repo := untrackedRepo(t)
	write(t, repo, "docs/TROUBLESHOOTING.md", "# Troubleshooting\n\nthe worker wrote this\n")

	diff, files := (&Engine{}).collectDiff(context.Background(), repo)

	if len(files) != 1 || files[0] != "docs/TROUBLESHOOTING.md" {
		t.Fatalf("files_changed = %v, want the created file", files)
	}
	for _, want := range []string{"b/docs/TROUBLESHOOTING.md", "new file mode", "+the worker wrote this"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff missing %q:\n%s", want, diff)
		}
	}
}

// Edits to tracked files must keep working exactly as before, alongside additions.
func TestCollectDiffKeepsTrackedEdits(t *testing.T) {
	repo := untrackedRepo(t)
	write(t, repo, "tracked.py", "x = 2\n")
	write(t, repo, "new.py", "y = 3\n")

	diff, files := (&Engine{}).collectDiff(context.Background(), repo)

	if len(files) != 2 {
		t.Fatalf("files_changed = %v, want both the edit and the addition", files)
	}
	if !strings.Contains(diff, "-x = 1") || !strings.Contains(diff, "+x = 2") {
		t.Errorf("tracked edit lost from the diff:\n%s", diff)
	}
	if !strings.Contains(diff, "+y = 3") {
		t.Errorf("new file lost from the diff:\n%s", diff)
	}
}

// Ignored files are not the worker's work product, and rig's own machinery is not
// part of the change under review.
func TestCollectDiffExcludesIgnoredAndRigFiles(t *testing.T) {
	repo := untrackedRepo(t)
	write(t, repo, ".gitignore", "build/\n")
	write(t, repo, "build/out.o", "binary-ish\n")
	write(t, repo, ".gate/repro.py", "assert False\n")
	write(t, repo, ".gate.frozen/repro.py", "assert False\n")
	write(t, repo, ".rig-move-llm/config.env", "ENABLED=true\n")
	write(t, repo, ".claude/settings.json", "{}\n")
	write(t, repo, ".claude/output-styles/rig-delegate.md", "style\n")
	write(t, repo, ".mcp.json", "{}\n")
	write(t, repo, ccProofFile, "def test_x(): pass\n")
	write(t, repo, "real.py", "z = 1\n")

	diff, files := (&Engine{}).collectDiff(context.Background(), repo)

	for _, unwanted := range []string{"build/out.o", ".gate/repro.py", ".gate.frozen/repro.py",
		".rig-move-llm/config.env", ".claude/settings.json", ".claude/output-styles/rig-delegate.md",
		".mcp.json", ccProofFile} {
		for _, f := range files {
			if f == unwanted {
				t.Errorf("%s must not be reported as the worker's change", unwanted)
			}
		}
		if strings.Contains(diff, unwanted) {
			t.Errorf("%s leaked into the diff:\n%s", unwanted, diff)
		}
	}
	// .gitignore itself is a real, reviewable change the worker made.
	var sawReal, sawIgnoreFile bool
	for _, f := range files {
		switch f {
		case "real.py":
			sawReal = true
		case ".gitignore":
			sawIgnoreFile = true
		}
	}
	if !sawReal || !sawIgnoreFile {
		t.Errorf("files_changed = %v, want real.py and .gitignore", files)
	}
}

// A worker that leaves a large tree behind must not blow up the payload MAIN
// pays to read — and whatever is dropped must be NAMED, or a truncated return
// reads as a complete one.
func TestCollectDiffBudgetsNewFilesAndNamesTheRest(t *testing.T) {
	repo := untrackedRepo(t)
	big := strings.Repeat("line of content\n", 8000) // ~128 KB each
	for _, n := range []string{"a.txt", "b.txt", "c.txt", "d.txt"} {
		write(t, repo, "gen/"+n, big)
	}

	diff, files := (&Engine{}).collectDiff(context.Background(), repo)

	if len(diff) > 2*untrackedDiffBudget {
		t.Errorf("diff is %d bytes, well past the %d byte budget", len(diff), untrackedDiffBudget)
	}
	if !strings.Contains(diff, "omitted from this diff") {
		t.Errorf("the truncation is not disclosed in the diff:\n%s", diff[:min(len(diff), 400)])
	}
	// Every file is still accounted for by name, dropped content or not.
	if len(files) != 4 {
		t.Errorf("files_changed = %v, want all four named", files)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
