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
	defaultMaxIters   = 40
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
	Err          string   `json:"error,omitempty"`
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
Rules: paths are relative to the repo root. Do not ask questions — act. Do not touch files under .gate/ or .gate.frozen/. Keep edits minimal and scoped to the task.`

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

	for i := 0; i < e.maxIters; i++ {
		res.Iterations = i + 1
		if err := ctx.Err(); err != nil {
			res.Err = "context: " + err.Error()
			res.Stopped = "error"
			break
		}

		resp, usage, err := e.chat(ctx, msgs)
		if err != nil {
			res.Err = "chat: " + err.Error()
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
			if tc.Function.Name == "run_bash" {
				res.LastTest = out
			}
			msgs = append(msgs, translate.OpenAIMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    truncate(out, defaultMaxOutputB),
			})
		}
	}

	if res.Stopped == "" {
		res.Stopped = "max_iters"
	}
	res.Diff, res.FilesChanged = e.collectDiff(ctx, absRepo)
	return res
}

// chat issues one non-streaming Chat Completions request with the tool schema and
// returns the parsed response plus [inputTokens, outputTokens].
func (e *Engine) chat(ctx context.Context, msgs []translate.OpenAIMessage) (translate.OpenAIResponse, [2]int, error) {
	model := e.cfg.WorkerModel
	if model == "" {
		model = "local"
	}
	reqBody := translate.OpenAIRequest{
		Model:    model,
		Messages: msgs,
		Tools:    toolSchema(),
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
