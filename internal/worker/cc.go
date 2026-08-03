// B5 (map9): swap implement's inside from the hand-rolled 3-tool loop to a
// native `claude -p` subprocess whose inference is routed to the local worker
// model. The MCP surface, the Result contract, and everything MAIN sees stay
// byte-compatible with the C0 binary (this branch's base, adb2c47) — the one
// variable the B5 experiment moves is WHO iterates: a worker that carries the
// full Claude Code harness can investigate, edit, test and self-correct inside
// one delegation, so MAIN is not pulled back into the loop for every round.
// map9 Notes #4: the cost driver is the number of rounds MAIN thinks, not the
// bytes returned — this is the only lever left that attacks it directly.
//
// Selection (map10 P4, auto-default after the flip gate passed on all three
// axes): RIG_WORKER_ENGINE=cc forces the cc engine, any other non-empty value
// forces the loop, and UNSET picks cc exactly when RIG_CC_BASE_URL is
// configured — a user who wired the cc prerequisites gets the engine that won
// the gate, everyone else keeps the loop, and a plain install can never break
// on a missing claude CLI or base URL.
//
// Money safety: the subprocess MUST be pointed away from Anthropic
// (RIG_CC_BASE_URL, normally the local Anthropic-format endpoint in front of
// qwen). If the base URL is empty the run is refused rather than silently
// billing the worker leg to the account the product exists to protect.
package worker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/gate"
)

// ccEnabled reports whether this implement call should run on the native-CC
// engine instead of the 3-tool loop. See the selection rule in the package doc.
func ccEnabled() bool {
	switch strings.TrimSpace(os.Getenv("RIG_WORKER_ENGINE")) {
	case "cc":
		return true
	case "":
		return strings.TrimSpace(os.Getenv("RIG_CC_BASE_URL")) != ""
	default:
		return false
	}
}

// ccSystemPrompt frames the CC worker the same way systemPrompt frames the
// 3-tool loop, minus the tool inventory (CC brings its own). The self-correct
// clause is the B5 point: rounds that used to bounce back to MAIN happen here.
const ccSystemPrompt = `You are a code-fixing worker operating directly on a repository checkout.
Resolve the task by editing files and verifying with the repro/test command in the task.
Workflow — proof-of-flip is MANDATORY, in this order:
1. BEFORE your first edit to any file — before creating a new file too — write a regression
   test file named rig_proof_test.py at the repo root that asserts the EXPECTED behaviour from
   the task. When the task states an expected value or output, assert it concretely — "does not
   crash" alone is not a proof then.
2. Run it with the repo's test runner (e.g. ./.venv/bin/python -m pytest -q rig_proof_test.py).
   It MUST FAIL. If it passes, it does not capture the bug — rewrite it until it fails.
   GREENFIELD: when the task is to CREATE code that does not exist yet, do NOT skip this run
   because "there is nothing to break yet". Import/exercise the not-yet-existing module or file
   anyway: the resulting ModuleNotFoundError / ImportError / FileNotFoundError / collection
   error IS the required red state, and it is the only red a greenfield task can produce.
3. Make the MINIMAL source edit that fixes the bug (or creates the requested code).
4. Rerun the SAME test command byte-for-byte. It MUST PASS now.
5. Run the full test file(s) covering the code you changed and resolve any fallout you caused.
6. Delete rig_proof_test.py so the tree carries the source fix only.
Iterate yourself until the fix is verified — do not stop at an unverified attempt.
When verified, STOP and reply with a one-paragraph summary of what you changed and why, ending
with the line: PROOF: <the exact test command you ran red, then green>.
Rules: do not ask questions — act. Do not touch files under .gate/ or .gate.frozen/. Do not
refactor, rename, or "improve" anything you were not asked to change. Do not commit.`

// ccStashTag returns a label unique to one retry. `git stash pop` is positional
// — it restores stash@{0} no matter who pushed it — so a stash left on the
// stack by any earlier session is what a bare pop hands back (#54: a leftover
// from the previous day's volley was restored over a round's own finished work,
// leaving conflict markers in a source file and destroying 21 iterations). The
// tag is what lets the pop name this round's own entry instead of a position.
func ccStashTag() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Randomness is a nicety here, not a security property: any value that
		// does not collide with a stale entry works, and the pid plus clock
		// cannot collide with a stash pushed by an earlier process.
		return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func ccStashLabel(tag string) string { return "rig-fix-" + tag }

func ccStashPushCmd(tag string) string {
	return "git stash push -u -m " + ccStashLabel(tag)
}

// ccStashPopCmd resolves this round's stash by its label at pop time. Anything
// else on the stack — a stranger's leftover, or an entry the worker itself
// pushed meanwhile — stays where it is.
func ccStashPopCmd(tag string) string {
	label := ccStashLabel(tag)
	return "git stash pop \"$(git stash list | grep -F " + label + " | head -n1 | cut -d: -f1)\""
}

// ccProofRetryInstrFor is the single worker-side retry (P5 fixup shape: one
// shot, decided by the engine, MAIN never re-enters the loop). The fix is
// already in the tree, so red needs a temporary revert that also removes any
// files the previous session created (untracked), not just tracked edits.
func ccProofRetryInstrFor(tag string) string {
	return `PROOF MISSING: a previous session already applied a fix to this
working tree but produced no red->green proof-of-flip evidence. Complete the proof now WITHOUT
redoing the fix:
1. Save AND revert the fix in one step:  ` + ccStashPushCmd(tag) + `
   (-u also stashes files the previous session CREATED; a plain ` + "`" + `git checkout -- .` + "`" + ` cannot
   revert an untracked new file, so the red state would be impossible without it.)
   Use that command verbatim — the label is this round's own, and the pop below finds it by
   that label. Do not run a bare ` + "`" + `git stash` + "`" + `, and do not touch any other entry on the
   stack: entries left by earlier sessions are not yours to restore or drop.
2. Only NOW write rig_proof_test.py at the repo root asserting the EXPECTED behaviour from the
   task — write it after the stash so it is not part of the stash and cannot conflict on pop.
3. Run it with the repo's test runner (e.g. ./.venv/bin/python -m pytest -q rig_proof_test.py)
   — it MUST FAIL (red). If the task was to CREATE new code, the failure is the missing
   module/file (ModuleNotFoundError / ImportError / FileNotFoundError) — that is the correct red.
4. Re-apply the fix, verbatim:  ` + ccStashPopCmd(tag) + `
   This pop restores THIS round's work and nothing else. If it still reports a conflict, STOP:
   do not resolve it and do not edit the conflicted file. Reply saying the pop conflicted and
   name the file — a tree with conflict markers in it is worse than a missing proof.
5. Rerun the SAME test command byte-for-byte — it MUST PASS (green).
6. Delete rig_proof_test.py, then reply with a short summary ending with the PROOF line.`
}

// implementCC runs one implement call as a `claude -p` subprocess in the repo,
// parses its stream-json output, and returns the same Result shape the 3-tool
// loop produces — summary, diff, last gate output, token counts.
func (e *Engine) implementCC(ctx context.Context, absRepo, task, gateDir string) Result {
	res := Result{Stopped: "error"}

	base := strings.TrimSpace(os.Getenv("RIG_CC_BASE_URL"))
	if base == "" {
		res.Err = "RIG_CC_BASE_URL is empty — refusing to launch the CC worker: without a local " +
			"base URL its inference would bill Anthropic from the worker leg. Point it at the " +
			"local Anthropic-format endpoint for the worker model."
		return res
	}
	bin := strings.TrimSpace(os.Getenv("RIG_CC_BIN"))
	if bin == "" {
		bin = "claude"
	}

	user := task
	if gateDir != "" {
		user += "\n\n(A frozen test contract exists under " + gateDir + " — do not modify it; make the product code pass it.)"
	}

	// The proof must be demonstrated WITHIN one worker session (red, then green,
	// same command) — evidence never pairs across sessions, so a stale red from
	// an earlier spawn cannot legitimize a later lone green.
	proof := &ccProof{}
	sawResult := e.runCCOnce(ctx, bin, base, absRepo, user, &res, proof, true)

	// V6 proof-of-flip: when the worker claims done without red->green evidence,
	// the engine grants exactly ONE worker-side retry (P5 fixup shape). MAIN is
	// never pulled back in — the whole cycle stays on the free leg.
	if sawResult && res.Stopped == "done" && !ccProofComplete(absRepo, proof) {
		logStderr("cc: done without proof-of-flip — one worker-side retry")
		var res2 Result
		retryProof := &ccProof{}
		if e.runCCOnce(ctx, bin, base, absRepo, user+"\n\n"+ccProofRetryInstrFor(ccStashTag()), &res2, retryProof, false) {
			res.Iterations += res2.Iterations
			res.InputTokens += res2.InputTokens
			res.OutputTokens += res2.OutputTokens
			// The retry is part of the same round: a write it made counts toward
			// the round's touched-files fact like any other.
			res.FilesTouched = res.FilesTouched || res2.FilesTouched
			if res2.LastTest != "" {
				res.LastTest = res2.LastTest
			}
		}
		if retryProof.Flip {
			proof = retryProof
		}
	}
	if sawResult && res.Stopped == "done" && !ccProofComplete(absRepo, proof) {
		// Honest, non-blocking: the fix ships to MAIN, labelled. The output style
		// forbids closing green on this marker; nothing escalates automatically.
		res.Summary += "\n\nUNPROVEN: no red->green proof-of-flip evidence in the worker stream — treat this fix as unverified."
	}
	// A leftover proof test is worker garbage either way — it must never leak
	// into the tree a later run (or the user) sees.
	os.Remove(absRepo + string(os.PathSeparator) + ccProofFile)

	res.Diff, res.FilesChanged = e.collectDiff(ctx, absRepo)

	// Engine gate: any round that returns a non-empty diff is gated.
	// - Killed rounds (Stopped=="timeout") keep the original adapter byte-identical.
	// - Normal rounds with a diff get the engine's own measurement alongside the worker's claim.
	// - Empty diff means the round changed nothing — nothing to gate.
	if strings.TrimSpace(res.Diff) != "" {
		if res.Stopped == "timeout" {
			// Killed rounds: prove the gate ourselves (byte-identical to the original path).
			e.runEngineGate(ctx, absRepo, &res)
		} else {
			applyEngineGate(&res, absRepo)
		}
	}

	// A round that ends on its own with nothing on disk is reported as its own
	// outcome on this engine too. Implement() applies this to the 3-tool loop,
	// but it returns here before reaching that call, and this is the engine the
	// product actually runs.
	determineUnproductive(&res, res.FilesTouched)

	return res
}

// ccWritesFiles reports whether a native Claude Code tool name mutates the
// filesystem. Bash is deliberately absent: it is how the worker reads and runs
// the gate, so counting it would make every round look like it touched files.
func ccWritesFiles(name string) bool {
	switch name {
	case "Write", "Edit", "MultiEdit", "NotebookEdit":
		return true
	}
	return false
}

// applyEngineGate runs the repo's gate on a normal (non-killed) round with a
// non-empty diff, attaching the engine's own measurement to the result so the
// caller can compare it against the worker's claim. It is factored out of
// implementCC so it can be unit-tested without a live worker subprocess.
func applyEngineGate(res *Result, repo string) {
	res.WorkerVerdict = deriveWorkerVerdict(res.LastTest)
	// res.LastTestCmd is the gate the WORKER ran this round; it is the fallback
	// for a repo whose shape declares nothing (#59), and it must be read before
	// anything below overwrites it.
	o := runRepoGate(repo, res.LastTestCmd)
	if o.Ran {
		res.EngineGateCmd = o.Cmd
		out := o.Output
		if len(out) > 8000 {
			out = "[truncated]\n" + out[len(out)-8000:]
		}
		res.EngineGateOutput = out
		res.GateSource = "engine"
		res.GateVerdict = o.Verdict
	} else {
		res.Err += "\nengine gate not run: " + o.NotRunReason
	}
	res.Summary += engineGateNote(o, res.WorkerVerdict)
}

// ccProofComplete is the pass condition frozen in V6: a red->green flip seen in
// the stream AND no proof file left on disk (checked by the engine itself, not
// believed from prose).
func ccProofComplete(absRepo string, p *ccProof) bool {
	if p == nil || !p.Flip {
		return false
	}
	_, err := os.Stat(absRepo + string(os.PathSeparator) + ccProofFile)
	return os.IsNotExist(err)
}

// ccChildEnv builds the subprocess environment: the parent env minus every
// RIG_* variable, plus the worker-leg Anthropic wiring. Rig config must not
// leak into the worker's shell (#10) — a target repo whose tests read RIG_CC_*
// would fail for reasons unrelated to the change under test. The engine reads
// its own RIG_CC_* knobs in-process before spawning, so scrubbing costs nothing.
func ccChildEnv(base string) []string {
	parent := os.Environ()
	env := make([]string, 0, len(parent)+2)
	for _, kv := range parent {
		if strings.HasPrefix(kv, "RIG_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"ANTHROPIC_BASE_URL="+base,
		"ANTHROPIC_API_KEY="+ccAPIKey(),
		// The one RIG_* the child MUST carry (#24). The worker runs as its own
		// `claude -p` session, so its hook payloads have no agent_id — and with
		// every RIG_* stripped, the force-delegate hook read it as the paid MAIN
		// leg and denied its Edit/Write/Bash calls. Measured: a round that burned
		// the full wall guard with iters=0 and an empty diff, its last_test field
		// containing rig's own "Main agent is plan/delegate/review only" deny.
		// The posture applies to MAIN; the worker must be not-MAIN by
		// construction, and RIG_AGENT_ID is the existing seam that says so.
		"RIG_AGENT_ID="+ccWorkerAgentID)
}

// ccWorkerAgentID identifies the cc worker subprocess to rig's hooks. Any
// non-empty value works; a descriptive one makes force-delegate.log readable.
const ccWorkerAgentID = "cc-worker"

// runCCOnce spawns one `claude -p` subprocess and parses its stream into res,
// accumulating proof evidence. firstRun gates the skew diagnostics and the
// prompt-archive mode (write vs append).
func (e *Engine) runCCOnce(ctx context.Context, bin, base, absRepo, user string, res *Result, proof *ccProof, firstRun bool) bool {
	args := []string{
		"-p", user,
		"--output-format", "stream-json", "--verbose",
		"--max-turns", fmt.Sprint(envInt("RIG_CC_MAX_TURNS", 30)),
		"--model", ccModel(),
		"--strict-mcp-config",
		// The worker must edit and run tests unattended; its blast radius is the
		// repo checkout and its inference is local.
		"--dangerously-skip-permissions",
	}
	// Frozen map9 posture: the worker leg gets the same closed web as MAIN and
	// the N1 control. Override (e.g. "") is possible but is a posture change and
	// invalidates pairing with runs measured before it.
	deny := "WebFetch,WebSearch"
	if v, ok := os.LookupEnv("RIG_CC_DISALLOWED"); ok {
		deny = strings.TrimSpace(v)
	}
	if deny != "" {
		args = append(args, "--disallowedTools", deny)
	}
	args = append(args, "--append-system-prompt", ccSystemPrompt)

	// `claude -p` does not echo its prompt into the stream (measured: D4
	// ADDENDUM), so the harness archives what was actually fired. The proof
	// retry appends so both prompts survive.
	if p := strings.TrimSpace(os.Getenv("RIG_CC_PROMPT_ARCHIVE")); p != "" {
		archive := "BIN: " + bin + "\nARGS: " + strings.Join(args[2:], " ") + "\n--- PROMPT ---\n" + user + "\n--- SYSTEM (appended) ---\n" + ccSystemPrompt + "\n"
		var err error
		if firstRun {
			err = os.WriteFile(p, []byte(archive), 0o644)
		} else if f, ferr := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); ferr == nil {
			_, err = f.WriteString("--- PROOF RETRY ---\n" + archive)
			f.Close()
		} else {
			err = ferr
		}
		if err != nil {
			logStderr("cc: prompt archive failed (continuing): %v", err)
		}
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = absRepo
	// The round is bounded by two guards, and both must reach the whole process
	// tree, not just `claude` itself (#18): the ctx deadline (the engine wall,
	// deliberately shorter than the client's MCP watchdog) and the stall guard
	// below. Cancel replaces CommandContext's default single-process kill.
	setProcGroup(cmd)
	cmd.Cancel = func() error { return killProcGroup(cmd) }
	cmd.WaitDelay = 5 * time.Second
	// The dummy API key does two jobs: it keeps the subprocess off the user's
	// OAuth/keychain credentials entirely (auth hygiene — the worker leg must not
	// ride the subscription identity), and it satisfies CC's headless auth check.
	// The local endpoint does not validate it (proven by the smoke shot).
	cmd.Env = ccChildEnv(base)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		res.Err = "cc: stdout pipe: " + err.Error()
		return false
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	logStderr("worker.implement engine=cc bin=%s base=%s repo=%s retry=%v", bin, base, absRepo, !firstRun)
	if err := cmd.Start(); err != nil {
		res.Err = "cc: launch " + bin + ": " + err.Error()
		return false
	}

	// Mirror the raw stream to a harness-owned file (same rationale as
	// RIG_WORKER_LOG: the subprocess's stdout belongs to us, but the evidence
	// must survive the run).
	var mirror *os.File
	if p := strings.TrimSpace(os.Getenv("RIG_CC_STREAM")); p != "" {
		if f, ferr := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); ferr == nil {
			mirror = f
			defer mirror.Close()
		}
	}

	// Liveness watchdog: a round that goes silent must die with a diagnosis while
	// the client is still listening, not sit mute until the MCP layer aborts it
	// (#18). The stream itself is the liveness signal — every parsed line touches
	// the tracker.
	act := newCCActivity()
	stopWatchdog := e.watchStall(cmd, act)

	sawResult, jsonLines, totalLines := e.parseCCStream(stdout, mirror, res, proof, act)
	stopWatchdog()
	werr := cmd.Wait()

	// A killed round reports WHY it died and what it was last seen doing, so MAIN
	// has something to tell the human instead of re-delegating into the dark.
	if !sawResult && firstRun {
		if diag, stopped := ccTimeoutDiagnosis(ctx, act); diag != "" {
			res.Stopped = stopped
			res.Err = diag
			return false
		}
	}

	if !sawResult && firstRun {
		res.Stopped = "error"
		msg := "cc: stream ended without a result event"
		// Version-skew guard (map10 P2): a claude CLI whose output is not the
		// stream-json this parser knows must surface as a diagnosis, never as a
		// silent empty return. Two skews are distinguishable: output that is not
		// JSON at all, and stream-json whose events no longer carry a terminal
		// result.
		if totalLines > 0 && jsonLines == 0 {
			msg = fmt.Sprintf("cc: %s produced %d lines of output but none parsed as stream-json — "+
				"claude CLI version skew? Verify `%s --version` supports `--output-format stream-json`", bin, totalLines, bin)
		} else if jsonLines > 0 {
			msg += fmt.Sprintf(" (parsed %d stream-json lines but no terminal result — "+
				"claude CLI version skew, or the run was killed mid-stream)", jsonLines)
		}
		if werr != nil {
			msg += " (" + werr.Error() + ")"
		}
		if s := strings.TrimSpace(stderr.String()); s != "" {
			msg += ": " + truncate(s, 2000)
		}
		res.Err = msg
	} else if !sawResult {
		logStderr("cc: proof retry ended without a result event (keeping first run's result)")
	}
	return sawResult
}

// ccStallTimeout bounds how long the worker subprocess may produce NO stream
// output at all before the engine kills it. Measured failure (#18): a cc round
// on a doc-heavy task went silent and stayed silent until Claude Code aborted
// the MCP call at 1800s — the caller learned nothing, and MAIN re-delegated into
// the same silence. A live worker streams an event per tool call, so silence is
// a real liveness signal — but it has ONE legitimate long form: a Bash tool call
// emits nothing until the command returns, and Claude Code lets a command run
// for roughly ten minutes. 600s therefore sits just past the longest honest
// silence (a slow test suite) and well under the wall guard, which in turn sits
// under the client watchdog. Lowering it below the bash ceiling starts killing
// healthy rounds mid-test-run. 0 disables the guard.
func ccStallTimeout() time.Duration {
	return time.Duration(envInt("RIG_CC_STALL_TIMEOUT", 600)) * time.Second
}

// StallGuard and WallGuard expose the two engine guards for reporting (doctor's
// guard rung). They are accessors, not knobs: the values still come from the same
// env overrides the engine itself reads, so what doctor prints is what a round
// will actually get.
func StallGuard() time.Duration { return ccStallTimeout() }

// WallGuard is the engine's own end-to-end bound on one implement call.
func WallGuard() time.Duration { return runTimeout() }

// ccStallCheckInterval is how often the watchdog samples the tracker. It only
// bounds the overshoot of the kill, not the guard itself, so it stays coarse for
// the production limit and scales down for the short limits tests use.
func ccStallCheckInterval(limit time.Duration) time.Duration {
	if d := limit / 5; d < 10*time.Second {
		return d
	}
	return 10 * time.Second
}

// ccActivity is the liveness record of one worker subprocess: when its stream
// last produced anything, and what it was last seen doing. The parser writes it,
// the watchdog reads it, so it is mutex-guarded.
type ccActivity struct {
	mu      sync.Mutex
	started time.Time
	at      time.Time
	last    string
	lines   int
	stalled bool
}

func newCCActivity() *ccActivity {
	now := time.Now()
	return &ccActivity{started: now, at: now}
}

// touch records one line of stream activity. desc, when non-empty, replaces the
// human-readable "last seen doing" note.
func (a *ccActivity) touch(desc string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.at = time.Now()
	a.lines++
	if desc != "" {
		a.last = desc
	}
}

func (a *ccActivity) idle() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return time.Since(a.at)
}

func (a *ccActivity) elapsed() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return time.Since(a.started)
}

func (a *ccActivity) markStalled() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stalled = true
}

func (a *ccActivity) isStalled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stalled
}

// lastSeen is the diagnosis line: what the worker was last observed doing, and
// how many stream lines it produced before going quiet. "no output at all" is
// itself a finding — it separates a hung launch from a worker that worked and
// then died.
func (a *ccActivity) lastSeen() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lines == 0 {
		return "no stream output at all (the subprocess never produced a single event)"
	}
	if a.last == "" {
		return fmt.Sprintf("%d stream events, none of them a tool call", a.lines)
	}
	return fmt.Sprintf("%s (after %d stream events)", a.last, a.lines)
}

// watchStall starts the liveness watchdog and returns the function that stops
// it. The watchdog kills the whole process group, which ends the stream and so
// unblocks the parser — the caller then reads the verdict off the tracker.
func (e *Engine) watchStall(cmd *exec.Cmd, act *ccActivity) (stop func()) {
	limit := ccStallTimeout()
	if limit <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(ccStallCheckInterval(limit))
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if act.idle() < limit {
					continue
				}
				act.markStalled()
				logStderr("cc: no stream activity for %s — killing the worker process group (last: %s)",
					act.idle().Round(time.Second), act.lastSeen())
				_ = killProcGroup(cmd)
				return
			}
		}
	}()
	return func() { close(done) }
}

// ccTimeoutDiagnosis turns a round that died on one of the two guards into the
// message MAIN reads. It returns ("", "") when neither guard fired, leaving the
// existing version-skew diagnostics to explain the missing result event.
func ccTimeoutDiagnosis(ctx context.Context, act *ccActivity) (msg, stopped string) {
	switch {
	case act.isStalled():
		return fmt.Sprintf(
			"cc: worker killed by the engine stall guard — no stream output for %s (limit %s), %s wall. "+
				"Last seen: %s. Any diff below is the partial work it had already written. "+
				"Do NOT simply re-delegate the same task: a stall repeats. Report this to the human, "+
				"or delegate a SMALLER, more concrete slice of the task.",
			ccStallTimeout(), ccStallTimeout(), act.elapsed().Round(time.Second), act.lastSeen()), "timeout"
	case ctx.Err() != nil:
		return fmt.Sprintf(
			"cc: worker killed by the engine wall guard after %s (RIG_WORKER_RUN_TIMEOUT). "+
				"Last seen: %s. Any diff below is the partial work it had already written. "+
				"Do NOT simply re-delegate the same task: it will hit the same wall. Report this to the "+
				"human, or delegate a SMALLER, more concrete slice of the task.",
			act.elapsed().Round(time.Second), act.lastSeen()), "timeout"
	}
	return "", ""
}

// ccAPIKey is the placeholder key the subprocess authenticates with against the
// LOCAL endpoint. Overridable for endpoints that do validate (RIG_CC_API_KEY).
func ccAPIKey() string {
	if v := strings.TrimSpace(os.Getenv("RIG_CC_API_KEY")); v != "" {
		return v
	}
	return "sk-rig-cc-worker-local"
}

// ccModel is the model name the subprocess runs as. It doubles as the routing
// key when RIG_CC_BASE_URL points at the model-routing shim (haiku == worker
// leg), and is passed through verbatim to a direct local endpoint.
func ccModel() string {
	if v := strings.TrimSpace(os.Getenv("RIG_CC_MODEL")); v != "" {
		return v
	}
	return "haiku"
}

// parseCCStream consumes `claude -p --output-format stream-json` line by line,
// filling res from the events. Returns whether a terminal result event was seen.
//
// Events used (everything else is mirrored and skipped):
//
//	{"type":"assistant","message":{"content":[{"type":"tool_use","id":..,"name":"Bash","input":{"command":..}}]}}
//	{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":..,"content":..}]}}
//	{"type":"result","subtype":"success","result":..,"num_turns":..,"usage":{..}}
//
// act is the liveness tracker the stall watchdog reads; it may be nil in tests
// that only exercise parsing.
func (e *Engine) parseCCStream(r interface{ Read([]byte) (int, error) }, mirror *os.File, res *Result, proof *ccProof, act *ccActivity) (sawResult bool, jsonLines, totalLines int) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24) // tool results can be large

	bashCmd := map[string]string{} // tool_use id -> Bash command

	for sc.Scan() {
		line := sc.Bytes()
		totalLines++
		if act != nil {
			act.touch("")
		}
		if mirror != nil {
			mirror.Write(append(append([]byte{}, line...), '\n'))
		}

		var ev struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			Result   string `json:"result"`
			NumTurns int    `json:"num_turns"`
			Usage    struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		jsonLines++

		switch ev.Type {
		case "assistant":
			for _, b := range ccContentBlocks(ev.Message.Content) {
				if b.Type == "tool_use" && act != nil {
					act.touch(ccToolDesc(b))
				}
				// The cc engine's worker is native Claude Code, so its filesystem
				// writes arrive as these tool names rather than the 3-tool loop's
				// write_file. Bash is excluded on the same reasoning as there: it
				// is the tool the worker reads and tests with.
				if b.Type == "tool_use" && ccWritesFiles(b.Name) {
					res.FilesTouched = true
				}
				if b.Type == "tool_use" && b.Name == "Bash" {
					var in struct {
						Command string `json:"command"`
					}
					_ = json.Unmarshal(b.Input, &in)
					if c := strings.TrimSpace(in.Command); c != "" {
						bashCmd[b.ID] = c
					}
				}
			}
		case "user":
			for _, b := range ccContentBlocks(ev.Message.Content) {
				if b.Type != "tool_result" {
					continue
				}
				cmd, ok := bashCmd[b.ToolUseID]
				if ok && proof != nil {
					proof.observe(cmd, ccResultText(b), b.IsError)
				}
				// Same rule as the 3-tool loop's gate pick: only a command that
				// RUNS code can verify it; `git diff`/`ls` cannot.
				if !ok || !ccIsGateCommand(cmd) {
					continue
				}
				// Untruncated, matching C0's LastTest: the summary line lives at the
				// END of a verbose pytest run, and cutting the tail is exactly how a
				// green gate goes missing from the return.
				if txt := ccResultText(b); txt != "" {
					res.LastTest = txt
					// The command travels with its output, as it already does on
					// the 3-tool loop (engine parity). It is also the engine
					// gate's last-resort fallback on a repo whose shape rig does
					// not recognise (#59) — for a language rig has never heard
					// of, this is the only gate command that exists.
					res.LastTestCmd = cmd
				}
			}
		case "result":
			sawResult = true
			res.Summary = strings.TrimSpace(ev.Result)
			res.Iterations = ev.NumTurns
			res.InputTokens = ev.Usage.InputTokens
			res.OutputTokens = ev.Usage.OutputTokens
			switch ev.Subtype {
			case "success":
				res.Stopped = "done"
			case "error_max_turns":
				res.Stopped = "max_iters"
				res.HitIterationCap = true
				res.Summary = "CC worker hit its turn cap before declaring done. " +
					"The diff below is best-effort and may be incomplete — review it with extra scrutiny."
			default:
				res.Stopped = "error"
				res.Err = "cc: result subtype " + ev.Subtype
			}
		}
	}
	return sawResult, jsonLines, totalLines
}

// ccToolDesc renders one tool_use block as a short "what it was doing" note for
// the stall diagnosis: the tool name plus whichever argument identifies the
// target (the command for Bash, the path for the file tools).
func ccToolDesc(b ccBlock) string {
	name := b.Name
	if name == "" {
		name = "tool"
	}
	var in struct {
		Command string `json:"command"`
		Path    string `json:"file_path"`
		Pattern string `json:"pattern"`
	}
	_ = json.Unmarshal(b.Input, &in)
	for _, arg := range []string{in.Command, in.Path, in.Pattern} {
		if arg = strings.TrimSpace(arg); arg != "" {
			return name + " " + truncate(strings.Join(strings.Fields(arg), " "), 160)
		}
	}
	return name
}

// ccBlock is one content block of an assistant/user stream event.
type ccBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	Text      string          `json:"text"`
	IsError   bool            `json:"is_error"`
}

// ccProofFile is the canonical proof-test filename the worker is instructed to
// use; the detector keys on it, so a proof under any other name does not count.
const ccProofFile = "rig_proof_test.py"

// ccProof accumulates red->green evidence for the proof-of-flip contract (V6).
// Evidence is read from the worker's OWN tool_use/tool_result stream events —
// never from its prose (D3: prose can be faked wholesale; a tool_result cannot
// without also faking the run). The flip counts only when a FAIL and a later
// PASS were produced by the byte-normalized SAME command.
type ccProof struct {
	redSeen map[string]bool // normalized cmd -> a failing run was seen
	Flip    bool            // some cmd went red first, green later
}

func (p *ccProof) observe(cmd, resultText string, isError bool) {
	if !strings.Contains(cmd, ccProofFile) {
		return
	}
	if p.redSeen == nil {
		p.redSeen = map[string]bool{}
	}
	key := strings.Join(strings.Fields(cmd), " ")
	switch ccProofOutcome(resultText, isError) {
	case "red":
		p.redSeen[key] = true
	case "green":
		if p.redSeen[key] {
			p.Flip = true
		}
	}
}

// ccProofOutcome classifies one proof-test run. is_error mirrors the Bash
// tool's exit status (pytest exits non-zero on any failure), and the textual
// patterns back it up for CC versions that do not set the flag.
// Greenfield red patterns (ModuleNotFoundError, ImportError, etc.) are checked
// before the green pattern so a mixed run is still classified as red.
func ccProofOutcome(txt string, isError bool) string {
	t := strings.ToLower(txt)
	// Greenfield red: the proof test ran against code that does not exist yet.
	// Checked before " passed" so a mixed run (other tests passed, the proof test
	// errored during collection) is still red.
	for _, pat := range []string{
		"modulenotfounderror", "importerror", "no such file or directory",
		"filenotfounderror", "collection error", "errors during collection",
	} {
		if strings.Contains(t, pat) {
			return "red"
		}
	}
	failed := strings.Contains(t, " failed") || strings.Contains(t, "error") ||
		strings.Contains(t, "traceback")
	passed := strings.Contains(t, " passed")
	switch {
	case isError || failed:
		return "red"
	case passed:
		return "green"
	default:
		return ""
	}
}

func ccContentBlocks(raw json.RawMessage) []ccBlock {
	var blocks []ccBlock
	if json.Unmarshal(raw, &blocks) == nil {
		return blocks
	}
	return nil
}

// ccResultText extracts the text of a tool_result block, whose content is a
// string in some CC versions and a block list in others.
func ccResultText(b ccBlock) string {
	var s string
	if json.Unmarshal(b.Content, &s) == nil {
		return s
	}
	var inner []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(b.Content, &inner) == nil {
		var parts []string
		for _, i := range inner {
			if i.Type == "text" && i.Text != "" {
				parts = append(parts, i.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// ccInspectionCmds mirrors the gate heuristic later added to the 3-tool loop
// (9af68fa): commands that only READ state cannot verify a change. Kept local
// to the cc engine because this branch's base predates that fix.
var ccInspectionCmds = map[string]bool{
	"git": true, "ls": true, "cat": true, "head": true, "tail": true,
	"grep": true, "rg": true, "ag": true, "find": true, "fd": true,
	"echo": true, "printf": true, "pwd": true, "cd": true, "wc": true,
	"which": true, "type": true, "tree": true, "stat": true, "file": true,
	"du": true, "df": true, "sed": true, "awk": true, "cut": true,
	"sort": true, "uniq": true, "diff": true, "env": true, "export": true,
	"true": true, "false": true, "touch": true, "mkdir": true, "cp": true,
	"mv": true, "rm": true, "chmod": true, "less": true, "more": true,
	// `test`/`[` check state, they never run it — and the omission bit for
	// real: a worker's final `test -f rig_proof_test.py && echo GONE` was
	// classified as a gate run, so its echo CLOBBERED the real `go test`
	// output already captured as LastTest and the worker's verdict came back
	// "unknown" on a demonstrated round (v0.7.4 smoke, 2026-07-31).
	"test": true, "[": true,
}

// ccIsGateCommand reports whether a bash command verifies the change rather
// than inspecting it; a compound command counts if ANY segment runs code.
func ccIsGateCommand(cmd string) bool {
	repl := strings.NewReplacer("&&", "\x00", "||", "\x00", ";", "\x00", "|", "\x00", "\n", "\x00")
	for _, seg := range strings.Split(repl.Replace(cmd), "\x00") {
		fields := strings.Fields(strings.TrimSpace(seg))
		for len(fields) > 0 && strings.Contains(fields[0], "=") && !strings.ContainsAny(fields[0], "/") {
			fields = fields[1:]
		}
		// Same env unwrapping as the loop's classifier (stripEnvWrapper): rig's
		// own task prompts tell the worker to run its gate under `env -u`.
		fields = stripEnvWrapper(fields)
		if len(fields) == 0 {
			continue
		}
		if !ccInspectionCmds[baseName(fields[0])] {
			return true
		}
	}
	return false
}

// engineGateTimeout bounds the engine's own gate run. It is generous enough for
// a COLD build of a real repo (a warm `go build ./... && go test ./...` on this
// one is seconds, the same gate from a cold cache is minutes) and still far
// below the round guard it is meant to rescue: the gate compiles and tests what
// is already on disk, it never iterates, so anything past this is a hang.
const engineGateTimeout = 5 * time.Minute

// gateEnv is the environment the ENGINE-run gate executes with: the rig
// process's own environment minus every RIG_* variable.
//
// Same rule as ccChildEnv, for the same reason (#10): a build or test suite of
// the user's repo must never see rig's configuration, or it fails for reasons
// that have nothing to do with the change under test. It bites hardest exactly
// here — rig's own repo has tests that read RIG_AGENT_ID, so an unscrubbed
// engine gate would report `fail` on the very repo this feature was built in
// (#42), and a verdict that is wrong is worse than the missing verdict #41 set
// out to fix.
//
// Unlike ccChildEnv this adds nothing back: the gate is a build command, not an
// agent, so it needs no identity of its own.
func gateEnv() []string {
	parent := os.Environ()
	env := make([]string, 0, len(parent))
	for _, kv := range parent {
		if strings.HasPrefix(kv, "RIG_") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

// gateShell returns the interpreter for tool.Verify. The verify strings are
// shell one-liners (`go build ./... && go test ./...`), so they need a shell —
// and the one that exists is not the same everywhere.
func gateShell() (string, string) {
	if runtime.GOOS == "windows" {
		return "cmd", "/C"
	}
	return "bash", "-c"
}

// gateOutcome is what one run of the repo's gate produced. Ran is false when
// no gate executed at all, and NotRunReason says which of the reasons.
type gateOutcome struct {
	Ran          bool
	Verdict      string // "pass" | "fail"; empty when !Ran
	Cmd          string
	Output       string
	NotRunReason string // e.g. "no recognised repo shape"; empty when Ran

	// Source says where Cmd came from: "repo-shape" when a marker in the repo
	// implied it, "worker-observed" when it is the command the worker itself ran
	// (#59). The engine executed it either way — the verdict is the engine's own
	// measurement, never the worker's word — but a gate the WORKER chose is
	// weaker evidence than one the repo declares, and the caller is told which
	// kind it got rather than being left to assume the stronger one.
	Source string
}

// runRepoGate runs the repo's gate and reports the engine's own verdict.
//
// workerGateCmd is the gate command the worker was observed running in this repo
// during the round, used only when the repo's shape implies nothing. It is the
// escape hatch that makes the gate language-agnostic without a table that has to
// grow forever (#59): rig cannot know every ecosystem's test command, but the
// worker just demonstrated one that works HERE. Re-running it is not trusting the
// worker — the engine runs it itself, with RIG_* scrubbed, and reads the exit
// code itself, so a worker that lied about the result is still caught. It buys no
// new blast radius either: that command already ran in this repo, in this round.
func runRepoGate(repo, workerGateCmd string) gateOutcome {
	verify, source := "", "repo-shape"
	if tool, ok := gate.DetectGateTool(repo); ok {
		if _, found := tool.Available(repo, exec.LookPath); !found {
			if off := gate.FindOffPath(tool, repo); off != "" {
				return gateOutcome{NotRunReason: fmt.Sprintf("toolchain %s not on PATH (found at %s)", tool.Cmd, off)}
			}
			return gateOutcome{NotRunReason: fmt.Sprintf("toolchain %s not on PATH", tool.Cmd)}
		}
		verify = tool.Verify
	} else if c := strings.TrimSpace(workerGateCmd); c != "" && ccIsGateCommand(c) {
		verify, source = c, "worker-observed"
	} else {
		return gateOutcome{NotRunReason: "no recognised repo shape, and the worker ran no gate command of its own to fall back on"}
	}

	gateCtx, cancel := context.WithTimeout(context.Background(), engineGateTimeout)
	defer cancel()

	shell, shellFlag := gateShell()
	cmd := exec.CommandContext(gateCtx, shell, shellFlag, verify)
	cmd.Dir = repo
	cmd.Env = gateEnv()
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	output := out.String()

	if err != nil {
		if gateCtx.Err() != nil {
			return gateOutcome{NotRunReason: fmt.Sprintf("gate exceeded %v engine timeout", engineGateTimeout)}
		}
		// A command that does not APPLY to this repo exits non-zero too, and
		// calling that "fail" would blame the change for the gate being wrong.
		// The reasoning is the one already written into gateEnv: a verdict that is
		// wrong is worse than the missing verdict #41 set out to fix.
		if why := gateInapplicable(output); why != "" {
			return gateOutcome{NotRunReason: fmt.Sprintf("`%s` does not apply to this repo (%s)", verify, why)}
		}
		return gateOutcome{Ran: true, Verdict: "fail", Cmd: verify, Output: output, Source: source}
	}

	return gateOutcome{Ran: true, Verdict: "pass", Cmd: verify, Output: output, Source: source}
}

// gateInapplicableMarkers are the unmistakable ways a build tool says "you asked
// me to run something I do not have", as opposed to "your tests failed". Each one
// is a message the tool emits INSTEAD of running anything, so misreading it as a
// failing gate accuses a change that was never tested.
//
// The list is deliberately short and literal. A fuzzy match here would silently
// convert real failures into "not run", which is the dangerous direction: a
// missing verdict is visible in the return, a suppressed one is not.
var gateInapplicableMarkers = []struct{ needle, why string }{
	{"missing script: test", "package.json declares no test script"},
	{"no rule to make target 'test'", "the Makefile declares no test target"},
	{"no rule to make target `test'", "the Makefile declares no test target"},
	{"no such command", "the subcommand does not exist for this toolchain"},
	{"command not found", "the gate command is not installed"},
	{"is not recognized as an internal or external command", "the gate command is not installed"},
	{"could not find or load main class", "the build tool is not configured in this repo"},
	{"no tests were found", "the toolchain found no tests to run"},
	{"error: no test specified", "the project declares no test command"},
}

func gateInapplicable(output string) string {
	lower := strings.ToLower(output)
	for _, m := range gateInapplicableMarkers {
		if strings.Contains(lower, m.needle) {
			return m.why
		}
	}
	return ""
}

// runEngineGate runs the repo's gate after a killed round with partial work on
// disk. It creates its own short-lived context — independent from the round's
// cancelled one — because the gate is an independent check bounded by its own
// timeout. It is now the killed-round adapter over the shared runRepoGate runner.
//
// The verdict lands in GateVerdict (machine-readable) and the output in
// LastTest; Err carries only the cases where no gate ran at all, which are
// diagnoses of the round rather than judgements of the work.
func (e *Engine) runEngineGate(_ context.Context, repo string, res *Result) {
	o := runRepoGate(repo, res.LastTestCmd)
	if !o.Ran {
		res.Err += "\nengine gate not run: " + o.NotRunReason
		return
	}
	res.LastTestCmd = o.Cmd
	res.LastTest = "[ENGINE-RUN GATE — not demonstrated by the worker]\n" + o.Output
	res.GateSource = "engine"
	res.GateVerdict = o.Verdict
}

// engineGateNote is the block appended to the round summary so the caller
// reads the engine's own measurement next to the worker's claim. The two are
// different facts and are always stated as two facts.
func engineGateNote(o gateOutcome, workerVerdict string) string {
	prefix := "\n\n"
	if !o.Ran {
		return prefix + "ENGINE GATE NOT RUN: " + o.NotRunReason +
			". This is the absence of a verdict, not a failing verdict. " +
			"The worker reported: " + workerVerdict + " — unverified."
	}
	cmdStr := "`" + o.Cmd + "`"
	if o.Source == "worker-observed" {
		// Stated, not buried: rig did not know this repo's shape, so the command
		// is the worker's own pick. The engine still RAN it and read the exit code
		// itself, so the verdict is measured — but a worker that picks a narrow
		// command gets a narrow gate, and the caller has to be able to see that.
		cmdStr += " (the gate command the WORKER used — rig does not recognise this repo's shape, so it could not infer one of its own)"
	}
	switch {
	case o.Verdict == workerVerdict:
		return prefix + "ENGINE GATE: " + o.Verdict +
			" — measured by rig running " + cmdStr + " (RIG_* scrubbed). " +
			"WORKER CLAIMED: " + workerVerdict + ". Agreed."
	case workerVerdict == "unknown":
		// No claim is not a disagreement. A round that never reported a test
		// (common on small changes) must not be framed as a worker caught lying —
		// that framing pushes the caller to distrust a good round, which is the
		// false-reject failure this gate exists to prevent.
		return prefix + "ENGINE GATE: " + o.Verdict +
			" — measured by rig running " + cmdStr + " (RIG_* scrubbed). " +
			"The worker reported no test result of its own; the engine's measurement above is the only verdict for this round."
	default:
		return prefix + "ENGINE GATE DISAGREES WITH THE WORKER. " +
			"The engine measured: " + o.Verdict + ", running " + cmdStr +
			" itself. The worker reported: " + workerVerdict +
			". The engine-measured verdict is the one that was demonstrated; " +
			"the worker's account of this round is not evidence."
	}
}

// deriveWorkerVerdict classifies what the WORKER's own reported test output
// claims. It is a claim, never a measurement — it stays in its own field so a
// caller can never mistake it for something the engine demonstrated.
func deriveWorkerVerdict(lastTest string) string {
	if lastTest == "" {
		return "unknown"
	}
	failMarkers := []string{"--- FAIL", "FAIL", "build failed", "panic:", "Traceback", "error:", "Error:"}
	passMarkers := []string{"ok ", "PASS", "passed"}
	haveFail := false
	havePass := false
	for _, m := range failMarkers {
		if strings.Contains(lastTest, m) {
			haveFail = true
			break
		}
	}
	for _, m := range passMarkers {
		if strings.Contains(lastTest, m) {
			havePass = true
			break
		}
	}
	// Failure markers win over pass markers.
	if haveFail {
		return "fail"
	}
	if havePass {
		return "pass"
	}
	return "unknown"
}
