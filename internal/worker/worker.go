// Package worker is rig-move-llm's OPTION-2 offload mechanism: instead of trying
// to intercept Claude Code subagent traffic at ANTHROPIC_BASE_URL (which CC
// 2.1.214 no longer egresses — see .wayfinder ticket P9), the heavy work is
// delegated to an explicit MCP tool the agent calls: worker.implement.
//
// implement runs a small, self-contained agentic loop against the user's
// configured OpenAI-compatible endpoint (the "worker" / local model). Because an
// MCP server is always a separate process, that loop's inference is dispatched to
// the worker endpoint by construction — egress is guaranteed by the MCP contract,
// independent of how CC runs its own agents.
//
// The loop exposes three tools to the model — read_file, write_file, run_bash —
// all executing natively on the target repo. When the model stops (or the
// iteration budget is hit) the tool returns a summary + diff + last test output;
// the authoritative pass/fail verdict is produced separately by the deterministic
// PostToolUse gate against the frozen .gate/ contract (unchanged from map4).
//
// stdlib + internal/config + pkg/translate only.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/pkg/translate"
)

// defaults for the loop; overridable via env for experiments (RIG_WORKER_*).
const (
	// defaultMaxIters bounds the implement loop. The unbounded-feeling 40 let the
	// worker wander (E2E: 30 calls for a 3-line fix, stray docstring edits); a
	// tight budget plus the hit_iteration_cap flag keeps runs scoped and tells
	// MAIN when the result may be incomplete.
	defaultMaxIters   = 10
	defaultBashTOSec  = 120
	defaultHTTPTOSec  = 600
	defaultMaxOutputB = 24000 // per-tool-result byte cap fed back to the model
)

// Result is the outcome of one implement run. It is what the MCP tool serializes
// back to the calling agent (as text), and what tests assert on.
type Result struct {
	Summary      string   `json:"summary"`
	FilesChanged []string `json:"files_changed"`
	Diff         string   `json:"diff"`
	LastTest     string   `json:"last_test,omitempty"`
	Iterations   int      `json:"iterations"`
	InputTokens  int      `json:"input_tokens"`
	OutputTokens int      `json:"output_tokens"`
	Stopped      string   `json:"stopped"` // "done" | "max_iters" | "error"
	// HitIterationCap mirrors Stopped=="max_iters" as an explicit flag so MAIN's
	// review knows the worker ran out of budget before declaring done.
	HitIterationCap bool `json:"hit_iteration_cap,omitempty"`
	// Checkpoints counts how many times the context budget (RIG_WORKER_CTX_LIMIT)
	// tripped and rig reset the conversation to a rig-assembled digest (option B).
	// >0 means the run spanned multiple context windows without hallucinating from
	// an over-long context.
	Checkpoints int    `json:"checkpoints,omitempty"`
	Err         string `json:"error,omitempty"`
}

// Engine drives implement runs against a configured worker endpoint.
type Engine struct {
	cfg      config.Config
	client   *http.Client
	maxIters int
	bashTO   time.Duration
}

// NewEngine builds an Engine from resolved config, honoring RIG_WORKER_* env
// overrides used by experiments.
func NewEngine(cfg config.Config) *Engine {
	e := &Engine{
		cfg:      cfg,
		maxIters: envInt("RIG_WORKER_MAX_ITERS", defaultMaxIters),
		bashTO:   time.Duration(envInt("RIG_WORKER_BASH_TIMEOUT", defaultBashTOSec)) * time.Second,
	}
	e.client = &http.Client{Timeout: time.Duration(envInt("RIG_WORKER_HTTP_TIMEOUT", defaultHTTPTOSec)) * time.Second}
	return e
}

// systemPrompt frames the worker's job. It is deliberately narrow: resolve the
// task by editing files and verifying with the repro, then stop.
const systemPrompt = `You are a code-fixing worker operating directly on a repository checkout.
You have three tools: read_file, write_file, run_bash. Use them to resolve the task.
Workflow:
1. Read the relevant files to understand the failure.
2. Make the minimal edit that fixes it via write_file.
3. Run the failing test (or the command in the task) with run_bash to verify it now passes.
4. REGRESSION CHECK — run the FULL test file(s) that cover the code you changed (not just the
   one failing test). If your change makes a previously-passing test fail (e.g. an existing test
   that exercises the exact behaviour you just constrained), that test is fallout you must resolve:
   update it to match the new correct behaviour, or adjust your fix. Do not stop with a
   self-inflicted new failure.
5. When the fix is verified AND no new failures remain in the affected test file(s), STOP and reply
   with a one-paragraph summary of what you changed and why.
Rules: paths are relative to the repo root. Do not ask questions — act. Do not touch files under .gate/ or .gate.frozen/.
Make the MINIMAL change that resolves the task: do not touch code, tests, comments, or docstrings
unrelated to the task; do not refactor, rename, or "improve" anything you were not asked to change.
Your iteration budget is small — stop as soon as the fix is verified, with no extra polish passes.`

// Implement runs the agentic loop for a task against repo. gateDir is advisory
// (the frozen contract the deterministic gate will check); the loop is told not
// to touch it. ctx bounds the whole run.
func (e *Engine) Implement(ctx context.Context, repo, task, gateDir string) Result {
	res := Result{Stopped: "error"}
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		res.Err = "bad repo path: " + err.Error()
		return res
	}
	if fi, err := os.Stat(absRepo); err != nil || !fi.IsDir() {
		res.Err = "repo is not a directory: " + absRepo
		return res
	}

	user := task
	if gateDir != "" {
		user += "\n\n(A frozen test contract exists under " + gateDir + " — do not modify it; make the product code pass it.)"
	}

	msgs := []translate.OpenAIMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: user},
	}
	res.Stopped = ""

	// Ground truth rig accumulates for a context checkpoint (option B): the files
	// the worker has read (so a reseeded conversation knows what it already saw).
	// The other two inputs — the git diff and the last test output — are read from
	// disk / res.LastTest at checkpoint time, never from the (possibly confused)
	// worker.
	read := map[string]bool{}
	limit := ctxLimit()

	for i := 0; i < e.maxIters; i++ {
		res.Iterations = i + 1
		if err := ctx.Err(); err != nil {
			res.Err = "context: " + err.Error()
			res.Stopped = "error"
			break
		}

		resp, usage, err := e.chat(ctx, msgs, toolSchema())
		if err != nil {
			res.Err = "chat: " + err.Error()
			if i == 0 {
				// The worker endpoint was unreachable before any work was done. Steer
				// MAIN to fall back instead of retrying a dead endpoint — this is the
				// call-time counterpart to the UserPromptSubmit health-check fallback.
				res.Err += " — worker endpoint unreachable; implement this task yourself (local edits/tests are permitted this turn) instead of re-calling the worker."
			}
			res.Stopped = "error"
			break
		}
		res.InputTokens += usage[0]
		res.OutputTokens += usage[1]

		if len(resp.Choices) == 0 {
			res.Err = "worker returned no choices"
			res.Stopped = "error"
			break
		}
		m := resp.Choices[0].Message

		// Record the assistant turn (content + any tool calls) into history.
		msgs = append(msgs, translate.OpenAIMessage{Role: "assistant", Content: m.Content, ToolCalls: m.ToolCalls})

		if len(m.ToolCalls) == 0 {
			// Model produced a final answer — done.
			res.Summary = strings.TrimSpace(m.Content)
			res.Stopped = "done"
			break
		}

		// Execute each requested tool call and feed results back.
		for _, tc := range m.ToolCalls {
			out := e.execTool(ctx, absRepo, tc)
			switch tc.Function.Name {
			case "run_bash":
				res.LastTest = out
			case "read_file":
				if p := readPathArg(tc); p != "" {
					read[p] = true
				}
			}
			msgs = append(msgs, translate.OpenAIMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    truncate(out, defaultMaxOutputB),
			})
		}

		// Context-budget checkpoint (option B). usage[0] is the real context size the
		// endpoint just saw; once it reaches the budget, the conversation is too long
		// and the next turn only grows it — the point at which the worker starts to
		// hallucinate. Reset to a rig-assembled digest (task + git diff from disk +
		// last test + files read) so a fresh, sharp context continues the SAME work.
		// The edits are already on disk (write_file), so nothing is lost across the
		// reset; the worker re-derives its plan from the diff.
		if overCtxBudget(usage[0], limit) {
			res.Checkpoints++
			logStderr("context checkpoint #%d at iter %d (prompt_tokens=%d >= %d) — conversation reset to rig-assembled digest", res.Checkpoints, res.Iterations, usage[0], limit)
			msgs = e.reassembleImplementMsgs(ctx, absRepo, task, gateDir, sortedKeys(read), res.LastTest)
		}
	}

	if res.Stopped == "" {
		res.Stopped = "max_iters"
		res.HitIterationCap = true
		res.Summary = "Worker hit the iteration cap (" + fmt.Sprint(e.maxIters) + ") before declaring done. " +
			"The diff below is best-effort and may be incomplete — review it with extra scrutiny."
	}
	res.Diff, res.FilesChanged = e.collectDiff(ctx, absRepo)
	return res
}

// resumePreamble frames a rig-assembled checkpoint for the worker: the prior
// conversation was reset (context budget), but the work is safe on disk and shown
// as a diff — continue, don't restart.
const resumePreamble = `CONTEXT CHECKPOINT — your prior working conversation grew past the context budget and was reset to keep you sharp. Nothing is lost: the edits you already made are written to disk and are shown below as the current diff. Continue the SAME task from here — do NOT restart from scratch and do NOT re-apply changes already present in the diff. Re-read any file with read_file if you need its current contents.`

// reassembleImplementMsgs rebuilds a fresh worker conversation from ground truth
// rig holds — the task, the current git diff (work already written to disk), the
// last test output, and the files the worker has read — so a context-bloated
// conversation can be reset WITHOUT asking the (possibly confused) worker to
// summarize itself. This mirrors explore's progress-digest reseed: the digest is
// rig-assembled and deterministic, never worker-authored, so the reset removes the
// bloated context that causes hallucination rather than distilling it through the
// same confused model.
func (e *Engine) reassembleImplementMsgs(ctx context.Context, absRepo, task, gateDir string, readFiles []string, lastTest string) []translate.OpenAIMessage {
	var b strings.Builder
	b.WriteString(resumePreamble)
	b.WriteString("\n\nTASK:\n")
	b.WriteString(task)
	if gateDir != "" {
		b.WriteString("\n\n(A frozen test contract exists under " + gateDir + " — do not modify it; make the product code pass it.)")
	}

	diff, _ := e.collectDiff(ctx, absRepo)
	b.WriteString("\n\n--- WORK SO FAR (current uncommitted git diff — this is your progress, already on disk) ---\n")
	if strings.TrimSpace(diff) == "" {
		b.WriteString("(no changes written to disk yet)")
	} else {
		b.WriteString(truncate(diff, defaultMaxOutputB))
	}

	if t := strings.TrimSpace(lastTest); t != "" {
		b.WriteString("\n\n--- LAST TEST OUTPUT ---\n")
		b.WriteString(truncate(t, 8000))
	}
	if len(readFiles) > 0 {
		b.WriteString("\n\n--- FILES ALREADY READ (re-read with read_file if you need their current contents) ---\n")
		b.WriteString(strings.Join(readFiles, ", "))
	}

	return []translate.OpenAIMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: b.String()},
	}
}

// readPathArg extracts the path argument from a read_file tool call, for tracking
// which files the worker has seen (fed into the checkpoint digest). Empty on any
// parse failure — tracking is best-effort.
func readPathArg(tc translate.OpenAIToolCall) string {
	var a struct {
		Path string `json:"path"`
	}
	if json.Unmarshal([]byte(tc.Function.Arguments), &a) != nil {
		return ""
	}
	return strings.TrimSpace(a.Path)
}

// chat issues one non-streaming Chat Completions request with the given tool
// schema and returns the parsed response plus [inputTokens, outputTokens].
func (e *Engine) chat(ctx context.Context, msgs []translate.OpenAIMessage, tools []translate.OpenAITool) (translate.OpenAIResponse, [2]int, error) {
	model := e.cfg.WorkerModel
	if model == "" {
		model = "local"
	}
	reqBody := translate.OpenAIRequest{
		Model:    model,
		Messages: msgs,
		Tools:    tools,
	}
	buf, _ := json.Marshal(reqBody)

	url := strings.TrimRight(e.cfg.WorkerAPIBase, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return translate.OpenAIResponse{}, [2]int{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.cfg.WorkerAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.cfg.WorkerAPIKey)
	}
	for k, v := range e.cfg.Backend.ExtraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return translate.OpenAIResponse{}, [2]int{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return translate.OpenAIResponse{}, [2]int{}, fmt.Errorf("worker %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out translate.OpenAIResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return translate.OpenAIResponse{}, [2]int{}, fmt.Errorf("decode worker response: %w", err)
	}
	var toks [2]int
	if out.Usage != nil {
		toks = [2]int{out.Usage.PromptTokens, out.Usage.CompletionTokens}
	}
	return out, toks, nil
}

// collectDiff returns the repo's uncommitted diff and the list of changed paths.
// Non-git repos yield empty strings (the gate is what actually judges the run).
func (e *Engine) collectDiff(ctx context.Context, repo string) (string, []string) {
	diff := gitOut(ctx, repo, "diff")
	names := gitOut(ctx, repo, "diff", "--name-only")
	var files []string
	for _, l := range strings.Split(strings.TrimSpace(names), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			files = append(files, l)
		}
	}
	return diff, files
}

func gitOut(ctx context.Context, repo string, args ...string) string {
	c := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	out, _ := c.Output()
	return string(out)
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// truncate shortens s to at most n bytes, appending a marker when it does.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…[truncated]"
}
