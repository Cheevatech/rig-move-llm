package thin

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// untrackedBudget caps how many bytes of NEW-file content enter the diff. A
// worker that leaves an unignored build directory behind would otherwise bury
// the change a human is trying to read. Whatever is dropped is named.
const untrackedBudget = 256 * 1024

// collectDiff reads the working tree and returns the diff and its stat. Both
// come from git; nothing here is synthesized.
//
// Untracked files are included, and that is not an edge case: `git diff` shows
// tracked files only, so a worker that CREATED files used to return an empty
// diff — indistinguishable from having done nothing (#26), on tasks where the
// new file WAS the work.
func collectDiff(ctx context.Context, repo string) (diff, stat string) {
	diff = gitOut(ctx, repo, "diff")
	stat = gitOut(ctx, repo, "diff", "--stat")

	untracked, names, dropped := collectUntracked(ctx, repo)
	diff += untracked
	if len(names) > 0 {
		stat = strings.TrimRight(stat, "\n")
		if stat != "" {
			stat += "\n"
		}
		stat += "new files: " + summarizeNames(names)
	}
	if dropped > 0 {
		stat += "\n(" + strconv.Itoa(dropped) + " new file(s) omitted from the diff: over the untracked budget — see the checkout itself)"
	}
	return diff, stat
}

// maxNamedNewFiles bounds how many new-file paths the STAT lists by name.
//
// The stat is the compact branch of the return — the one taken precisely because
// the diff was too big to paste. Measured 2026-08-04 (S3 live fire): a repo whose
// .venv was untracked and un-ignored produced a stat of ~8 KB of filenames, and
// MAIN spent a whole turn telling the human those were not part of the change. A
// summary that has to be explained away is not a summary.
const maxNamedNewFiles = 12

func summarizeNames(names []string) string {
	if len(names) <= maxNamedNewFiles {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:maxNamedNewFiles], ", ") +
		fmt.Sprintf(", and %d more (the full list is in the diff at the log path)", len(names)-maxNamedNewFiles)
}

// collectUntracked renders each untracked, non-ignored file as a git-format
// addition. `git diff --no-index` produces hunks review already knows how to
// read WITHOUT touching the index — `git add -N` is shorter but the index
// belongs to the user, not to rig.
func collectUntracked(ctx context.Context, repo string) (diff string, names []string, dropped int) {
	out := gitOut(ctx, repo, "ls-files", "--others", "--exclude-standard")
	var b strings.Builder
	for _, f := range strings.Split(strings.TrimSpace(out), "\n") {
		f = strings.TrimSpace(f)
		if f == "" || skipPath(f) {
			continue
		}
		if b.Len() >= untrackedBudget {
			dropped++
			continue
		}
		// --no-index exits 1 when the files differ, which is the normal case here.
		d := gitOut(ctx, repo, "diff", "--no-index", "--", "/dev/null", f)
		if strings.TrimSpace(d) == "" {
			continue
		}
		b.WriteString(d)
		names = append(names, f)
	}
	return b.String(), names, dropped
}

// skipPath keeps out of the change the things a human is not reviewing: rig's own
// bookkeeping, and the environment directories a checkout accumulates.
//
// The environment list is a heuristic, and it is only reachable when the user has
// NOT gitignored these — `git ls-files --others --exclude-standard` already
// respects .gitignore, so a well-kept repo never gets here. It exists because the
// failure is asymmetric: nobody reviews the contents of a virtualenv, and one
// un-ignored .venv buries the actual change under a thousand files (measured in
// the S3 live fire).
func skipPath(p string) bool {
	for _, prefix := range []string{
		".rig-move-llm/", ".claude/", ".serena/",
		".venv/", "venv/", "node_modules/", "__pycache__/", ".pytest_cache/",
	} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	// The same directories nested anywhere, not only at the repo root.
	for _, seg := range []string{"/node_modules/", "/__pycache__/", "/.venv/", "/site-packages/"} {
		if strings.Contains(p, seg) {
			return true
		}
	}
	return false
}

// gitOut runs git and returns its stdout, ignoring the exit status: every call
// here has a meaningful non-zero case (no repo, --no-index differing files), and
// an empty string is the honest answer in all of them.
func gitOut(ctx context.Context, repo string, args ...string) string {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repo
	out, _ := cmd.Output()
	return string(out)
}
