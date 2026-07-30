package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// rigArtifacts are the paths rig itself writes into a project. They are wiring,
// not the user's work — and two mechanisms had already mistaken them for work:
// the worker's returned diff listed them as files the worker had authored (#26),
// and the proof-retry protocol's `git stash -u` would sweep the live hook config
// out of the tree mid-round. Excluding them locally fixes both at the source.
var rigArtifacts = []string{
	".claude/",
	".mcp.json",
	".rig-move-llm/",
	".gate/",
	".gate.frozen/",
	"rig_proof_test.py",
}

// rigExcludeHeader marks rig's block in .git/info/exclude so a re-run is
// idempotent and a human reading the file knows who wrote it.
const rigExcludeHeader = "# rig-move-llm (local wiring, not your work)"

// excludeRigArtifacts appends any missing rig path to the repo's
// .git/info/exclude and returns how many it added. info/exclude is deliberate:
// it is local and never committed, so rig never edits a .gitignore the user owns
// and shares with their team. A directory without .git (or a worktree/submodule
// layout rig does not recognise) is not an error — there is simply nothing to
// exclude, and every caller treats it as best-effort.
func excludeRigArtifacts(repo string) (int, error) {
	gitDir := filepath.Join(repo, ".git")
	fi, err := os.Stat(gitDir)
	if err != nil {
		return 0, nil // not a git repo: nothing to do
	}
	if !fi.IsDir() {
		// A linked worktree/submodule: .git is a file pointing at the real dir.
		resolved, rerr := resolveGitFile(gitDir)
		if rerr != nil || resolved == "" {
			return 0, nil
		}
		gitDir = resolved
	}

	path := filepath.Join(gitDir, "info", "exclude")
	existing := map[string]bool{}
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			existing[strings.TrimSpace(sc.Text())] = true
		}
		f.Close()
	}

	var missing []string
	for _, p := range rigArtifacts {
		if !existing[p] {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return 0, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var b strings.Builder
	if !existing[rigExcludeHeader] {
		b.WriteString("\n" + rigExcludeHeader + "\n")
	}
	for _, p := range missing {
		b.WriteString(p + "\n")
	}
	if _, err := f.WriteString(b.String()); err != nil {
		return 0, fmt.Errorf("append to %s: %w", path, err)
	}
	return len(missing), nil
}

// resolveGitFile reads a `.git` FILE ("gitdir: <path>") and returns the real git
// dir, absolute. Relative targets are resolved against the file's own directory,
// which is how git itself reads them.
func resolveGitFile(gitFile string) (string, error) {
	data, err := os.ReadFile(gitFile)
	if err != nil {
		return "", err
	}
	target := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir:"))
	if target == "" {
		return "", nil
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(gitFile), target)
	}
	return filepath.Clean(target), nil
}
