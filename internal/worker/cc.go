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
// Selection: RIG_WORKER_ENGINE=cc. Anything else (or unset) keeps the original
// loop, so a binary built from this branch behaves exactly like C0's until the
// experiment arm opts in per-run.
//
// Money safety: the subprocess MUST be pointed away from Anthropic
// (RIG_CC_BASE_URL, normally the local Anthropic-format endpoint in front of
// qwen). If the base URL is empty the run is refused rather than silently
// billing the worker leg to the account the product exists to protect.
package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ccEnabled reports whether this implement call should run on the native-CC
// engine instead of the 3-tool loop.
func ccEnabled() bool { return strings.TrimSpace(os.Getenv("RIG_WORKER_ENGINE")) == "cc" }

// ccSystemPrompt frames the CC worker the same way systemPrompt frames the
// 3-tool loop, minus the tool inventory (CC brings its own). The self-correct
// clause is the B5 point: rounds that used to bounce back to MAIN happen here.
const ccSystemPrompt = `You are a code-fixing worker operating directly on a repository checkout.
Resolve the task by editing files and verifying with the repro/test command in the task.
Workflow: read the relevant code, make the MINIMAL edit that fixes it, run the failing test to
verify, then run the full test file(s) covering the code you changed and resolve any fallout you
caused. Iterate yourself until the fix is verified — do not stop at an unverified attempt.
When verified, STOP and reply with a one-paragraph summary of what you changed and why.
Rules: do not ask questions — act. Do not touch files under .gate/ or .gate.frozen/. Do not
refactor, rename, or "improve" anything you were not asked to change. Do not commit.`

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
	// ADDENDUM), so the harness archives what was actually fired.
	if p := strings.TrimSpace(os.Getenv("RIG_CC_PROMPT_ARCHIVE")); p != "" {
		archive := "BIN: " + bin + "\nARGS: " + strings.Join(args[2:], " ") + "\n--- PROMPT ---\n" + user + "\n--- SYSTEM (appended) ---\n" + ccSystemPrompt + "\n"
		if err := os.WriteFile(p, []byte(archive), 0o644); err != nil {
			logStderr("cc: prompt archive failed (continuing): %v", err)
		}
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = absRepo
	cmd.Env = append(os.Environ(), "ANTHROPIC_BASE_URL="+base)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		res.Err = "cc: stdout pipe: " + err.Error()
		return res
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	logStderr("worker.implement engine=cc bin=%s base=%s repo=%s", bin, base, absRepo)
	if err := cmd.Start(); err != nil {
		res.Err = "cc: launch " + bin + ": " + err.Error()
		return res
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

	sawResult := e.parseCCStream(stdout, mirror, &res)
	werr := cmd.Wait()

	if !sawResult {
		res.Stopped = "error"
		msg := "cc: stream ended without a result event"
		if werr != nil {
			msg += " (" + werr.Error() + ")"
		}
		if s := strings.TrimSpace(stderr.String()); s != "" {
			msg += ": " + truncate(s, 2000)
		}
		res.Err = msg
	}

	res.Diff, res.FilesChanged = e.collectDiff(ctx, absRepo)
	return res
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
func (e *Engine) parseCCStream(r interface{ Read([]byte) (int, error) }, mirror *os.File, res *Result) bool {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24) // tool results can be large

	bashCmd := map[string]string{} // tool_use id -> Bash command
	sawResult := false

	for sc.Scan() {
		line := sc.Bytes()
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

		switch ev.Type {
		case "assistant":
			for _, b := range ccContentBlocks(ev.Message.Content) {
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
				// Same rule as the 3-tool loop's gate pick: only a command that
				// RUNS code can verify it; `git diff`/`ls` cannot. The pick
				// travels with the output via LastTestCmd... which C0's Result
				// does not carry — so here it stays internal and LastTest holds
				// the latest gate-shaped output only.
				if !ok || !ccIsGateCommand(cmd) {
					continue
				}
				// Untruncated, matching C0's LastTest: the summary line lives at the
				// END of a verbose pytest run, and cutting the tail is exactly how a
				// green gate goes missing from the return.
				if txt := ccResultText(b); txt != "" {
					res.LastTest = txt
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
	return sawResult
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
		if len(fields) == 0 {
			continue
		}
		head := fields[0]
		if i := strings.LastIndex(head, "/"); i >= 0 {
			head = head[i+1:]
		}
		if !ccInspectionCmds[head] {
			return true
		}
	}
	return false
}
