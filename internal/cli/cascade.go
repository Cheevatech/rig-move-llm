package cli

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/config"
)

// cmdCascade runs the route-cc-on-qwen cascade: it launches the given command
// (typically a headless `claude -p "<task>"`) TWICE if needed —
//
//	Pass 1  on the qwen (worker) leg — FREE. Then rig runs the verify gate.
//	          verify passes → DONE (the cheap model resolved it).
//	          verify fails  → the worker's edits are stashed (FRESH restart) and…
//	Pass 2  on the Claude (main) leg — verbatim to Anthropic, OAuth preserved, PAID.
//
// The leg is selected per-invocation via a /r/<leg> base-URL path segment the proxy
// strips, so a single shared daemon serves both passes without a restart. This is
// FrugalGPT result-as-classifier: the executed verify — not a difficulty guess —
// decides whether the paid frontier is paid for.
//
// Intended for non-interactive use (claude -p). An interactive command would open
// two sessions in a row, which is rarely what you want.
func cmdCascade(args []string) int {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "cascade: expected a command, e.g. rig-move-llm cascade -- claude -p \"<task>\"")
		return 2
	}

	cfg := config.Load()
	if cfg.VerifyCmd == "" {
		fmt.Fprintln(os.Stderr, "cascade: WARNING — VERIFY_CMD is not set; using a weak floor gate")
		fmt.Fprintln(os.Stderr, "         (any non-empty diff counts as resolved). Set VERIFY_CMD (e.g. \"pytest -q\")")
		fmt.Fprintln(os.Stderr, "         so the qwen pass is judged by a real executed test before escalating.")
	}

	// Cascade owns a DEDICATED proxy on a free port started from THIS binary,
	// rather than reusing whatever listens on cfg.Port. A pre-existing daemon there
	// may be an older rig without /r/<leg> route support — it would forward
	// /r/worker/... verbatim upstream (404) and every pass would silently fail. A
	// private, correct-version proxy makes the two-pass routing deterministic.
	port, err := freePort()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cascade: could not find a free port:", err)
		return 1
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cascade: cannot locate own binary:", err)
		return 1
	}
	srv := exec.Command(self, "serve", "--port", port)
	srv.Stdout, srv.Stderr = nil, nil
	if err := srv.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "cascade: could not start proxy:", err)
		return 1
	}
	defer func() { _ = srv.Process.Kill() }()
	if !waitPort("127.0.0.1:"+port, 10*time.Second) {
		fmt.Fprintln(os.Stderr, "cascade: proxy did not come up on port "+port)
		return 1
	}

	repo, _ := os.Getwd()
	gitOK := isGitRepo(repo)
	if !gitOK {
		fmt.Fprintln(os.Stderr, "cascade: not a git repo — cannot reset between passes; escalation will INHERIT the qwen pass's edits")
	}

	// Pass 1 — qwen (worker) leg.
	fmt.Fprintln(os.Stderr, "cascade: pass 1 — running on qwen (worker leg, free)…")
	if code := launchLeg("worker", port, args); code != 0 {
		fmt.Fprintf(os.Stderr, "cascade: pass 1 command exited %d\n", code)
	}

	if ok, out := runVerify(cfg, repo); ok {
		fmt.Fprintln(os.Stderr, "cascade: verify PASSED after the qwen pass — resolved on the free leg. Done.")
		return 0
	} else {
		fmt.Fprintln(os.Stderr, "cascade: verify FAILED after the qwen pass — escalating to Claude.")
		if strings.TrimSpace(out) != "" {
			fmt.Fprintln(os.Stderr, indentTail(out, 20))
		}
	}

	// FRESH restart: stash the worker pass's edits so Claude starts from a clean
	// tree (film ruling F1=FRESH). Recoverable via `git stash list`.
	if gitOK {
		if err := stashWorkerPass(repo); err != nil {
			fmt.Fprintln(os.Stderr, "cascade: WARNING — could not stash the qwen pass for a fresh restart:", err)
			fmt.Fprintln(os.Stderr, "         Claude will inherit the current tree instead.")
		} else {
			fmt.Fprintln(os.Stderr, "cascade: stashed the qwen pass (recover with `git stash list`); tree reset for Claude.")
		}
	}

	// Pass 2 — Claude (main) leg, verbatim + OAuth.
	fmt.Fprintln(os.Stderr, "cascade: pass 2 — running on Claude (main leg, paid)…")
	code := launchLeg("main", port, args)
	if code != 0 {
		fmt.Fprintf(os.Stderr, "cascade: pass 2 command exited %d\n", code)
	}

	if ok, out := runVerify(cfg, repo); ok {
		fmt.Fprintln(os.Stderr, "cascade: verify PASSED after the Claude pass — resolved on escalation.")
		return 0
	} else {
		fmt.Fprintln(os.Stderr, "cascade: verify FAILED after the Claude pass — task unresolved.")
		if strings.TrimSpace(out) != "" {
			fmt.Fprintln(os.Stderr, indentTail(out, 20))
		}
		return 1
	}
}

// launchLeg runs args with ANTHROPIC_BASE_URL pointed at the cascade's private
// proxy (port) on the given leg ("worker" | "main"), carrying the per-project
// /p/<id> identity when the cwd is a registered project so the proxy loads this
// project's config.
func launchLeg(leg, port string, args []string) int {
	base := "http://127.0.0.1:" + port + "/r/" + leg + projectSuffix()

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "ANTHROPIC_BASE_URL="+base)
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "cascade: launch:", err)
		return 1
	}
	return 0
}

// freePort asks the OS for an unused TCP port and returns it as a string. There is
// a small window between closing the probe listener and the child binding it, but
// collisions are vanishingly rare for a short-lived private proxy.
func freePort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	_, port, err := net.SplitHostPort(l.Addr().String())
	return port, err
}

// projectSuffix returns "/p/<id>" when the cwd is a registered project, else "".
func projectSuffix() string {
	cwd, _ := os.Getwd()
	if canon, err := config.CanonicalPath(cwd); err == nil && config.ProjectAllowed(canon) {
		return "/p/" + config.EncodeProjectID(canon)
	}
	return ""
}

// runVerify decides whether the current repo state resolves the task. With a
// VERIFY_CMD it runs that command (exit 0 = resolved). Without one it falls back to
// the weak floor: any tracked-or-untracked change present = "resolved" (logged at
// call sites as weak). Returns (resolved, combinedOutput).
func runVerify(cfg config.Config, repo string) (bool, string) {
	if cfg.VerifyCmd != "" {
		cmd := exec.Command("sh", "-c", cfg.VerifyCmd)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		return err == nil, string(out)
	}
	// Floor: non-empty diff. Best-effort; non-git repos have no diff signal.
	out, _ := exec.Command("git", "-C", repo, "status", "--porcelain").CombinedOutput()
	return strings.TrimSpace(string(out)) != "", string(out)
}

// isGitRepo reports whether repo is inside a git work tree.
func isGitRepo(repo string) bool {
	out, err := exec.Command("git", "-C", repo, "rev-parse", "--is-inside-work-tree").CombinedOutput()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// stashWorkerPass stashes all working-tree changes (the qwen pass's edits, plus any
// pre-existing uncommitted work) so the next pass starts clean. Recoverable.
func stashWorkerPass(repo string) error {
	out, err := exec.Command("git", "-C", repo, "stash", "push", "--include-untracked",
		"-m", "rig-cascade: qwen pass (escalated to Claude)").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// indentTail returns the last n lines of s, each indented, for a compact failure
// excerpt in the cascade log.
func indentTail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}
