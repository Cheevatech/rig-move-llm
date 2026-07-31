package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/internal/gatestate"
)

// mcpProtocolVersion is the MCP revision we advertise. 2024-11-05 is broadly
// supported by strict clients (incl. Claude Code); the handshake echoes whatever
// the client asks for when it is one we understand, else this default.
const mcpProtocolVersion = "2024-11-05"

// runTimeout bounds a single implement call end-to-end (the agentic loop can take
// minutes on a local model). Overridable via RIG_WORKER_RUN_TIMEOUT (seconds).
//
// INVARIANT (ordering, #33): stall guard < runTimeout < ClientCallTimeout. The
// engine must be the one that kills its own round, so it can return a diagnosis
// and the partial diff instead of leaving MAIN with a bare client-side abort.
//
// The 1500s that used to live here was calibrated against a timer that does not
// exist. Claude Code runs TWO timers on an MCP tool call, and #18 conflated them:
// the wall-clock limit is the per-server `timeout` in .mcp.json, else
// MCP_TOOL_TIMEOUT, else about 28 hours — while the 1800s abort we actually
// measured is the IDLE window, which for a stdio server is 30 minutes of no
// response and no progress notification. rig's worker answers once, at the end,
// so it is idle for the whole round by construction.
//
// So the honest bound is the per-server timeout rig now writes itself (see
// ClientCallTimeout), which also raises the stdio idle floor above this guard.
// 3000s is what a real cc round needs: measured 2026-07-30 (map13 volley 3,
// task11) the worker was still streaming tool calls and had just written two real
// files when the old guard cut it at 25 min — twice — so it never reached its own
// test gate. Silence is the stall guard's job (600s); this bounds total work.
func runTimeout() time.Duration {
	return time.Duration(envInt("RIG_WORKER_RUN_TIMEOUT", 3000)) * time.Second
}

// ClientCallTimeout is the wall-clock limit rig asks the CALLING client to apply
// to one worker tool call — written into the generated .mcp.json as the worker
// server's `timeout`. It sits above runTimeout so the engine always kills its own
// round first, and it does double duty on Claude Code v2.1.203+: a per-server
// timeout of at least 1s is also a floor on the idle timeout, so a round that is
// working quietly is no longer aborted at the 30-minute stdio idle window.
func ClientCallTimeout() time.Duration {
	return runTimeout() + 5*time.Minute
}

// exploreTimeout bounds a single explore call. It is MUCH tighter than
// runTimeout because explore is only Stage 0: it must leave the rest of the
// caller's turn budget for triage + implement + verify. Measured failure mode:
// with explore free to run its full runTimeout (3600s) it consumed the entire
// 1800s harness watchdog on a real sympy repo, so the pipeline never reached
// implement. On expiry the explore loop finalizes a best-effort, coverage-
// incomplete report (triage then correctly forces delegate) rather than losing
// the run. Override via RIG_EXPLORE_TIMEOUT (seconds).
func exploreTimeout() time.Duration {
	return time.Duration(envInt("RIG_EXPLORE_TIMEOUT", 600)) * time.Second
}

// rpcRequest / rpcResponse are the minimal JSON-RPC 2.0 shapes we handle. id is
// json.RawMessage because JSON-RPC allows string OR number ids, and notifications
// omit it (nil).
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Server is the stdio MCP server exposing worker.implement.
type Server struct {
	engine *Engine
	out    *bufio.Writer

	// mu guards repo. The stdio loop is sequential today, but the drill is read
	// by a different call than the one that sets it, so the pairing is stated.
	mu sync.Mutex
	// repo is where implement last ran. It is the drill's default target: under
	// the product wiring the server's own working directory is rig's checkout,
	// not the repo under work, so defaulting to the cwd sent every unqualified
	// drill to the wrong tree (B6 run 2).
	repo string
}

// noteRepo records the tree the work is happening in, so a later drill that
// arrives without a repo argument still lands on it.
func (s *Server) noteRepo(repo string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repo = repo
}

// drillTarget resolves the default drill target: the repo implement ran in, and
// only failing that the process working directory.
func (s *Server) drillTarget() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.repo != "" {
		return s.repo
	}
	cwd, _ := os.Getwd()
	return cwd
}

// Serve runs the MCP stdio loop over r/w until r is closed (EOF). It is the body
// of the `rig-move-llm worker` subcommand. cfg is the resolved worker config.
func Serve(cfg config.Config, r io.Reader, w io.Writer) error {
	s := &Server{engine: NewEngine(cfg), out: bufio.NewWriter(w)}
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			s.handleLine(line)
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func (s *Server) handleLine(line []byte) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		logStderr("mcp: bad json: %v", err)
		return
	}
	// Notifications (no id) get no response.
	isNotification := len(req.ID) == 0

	switch req.Method {
	case "initialize":
		s.reply(req.ID, s.onInitialize(req.Params))
	case "notifications/initialized", "initialized":
		// no-op notification
	case "ping":
		s.reply(req.ID, map[string]any{})
	case "tools/list":
		s.reply(req.ID, map[string]any{"tools": toolList()})
	case "tools/call":
		res, rerr := s.onToolsCall(req.Params)
		if rerr != nil {
			s.replyErr(req.ID, rerr.Code, rerr.Message)
			return
		}
		s.reply(req.ID, res)
	default:
		if !isNotification {
			s.replyErr(req.ID, -32601, "method not found: "+req.Method)
		}
	}
}

func (s *Server) onInitialize(params json.RawMessage) map[string]any {
	proto := mcpProtocolVersion
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(params, &p) == nil && p.ProtocolVersion != "" {
		proto = p.ProtocolVersion // echo the client's requested revision
	}
	return map[string]any{
		"protocolVersion": proto,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": "rig-move-llm-worker", "version": "1"},
	}
}

// toolList is the tools/list payload: implement (Stage 2), explore (Stage 0),
// triage (the Gate A intake declaration), and show_change (the drill).
func toolList() []map[string]any {
	return []map[string]any{{
		"name":        "implement",
		"description": "Resolve a coding task by running an agentic loop on the local worker model: it reads/edits files in the repo and runs tests, then returns a summary, the diff, and the last test output. Delegate all code changes here.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task":     map[string]any{"type": "string", "description": "The task / bug to resolve, with enough detail to act on."},
				"repo":     map[string]any{"type": "string", "description": "Absolute path to the repo checkout. Defaults to the server's working directory."},
				"gate_dir": map[string]any{"type": "string", "description": "Optional path to the frozen .gate/ contract; the worker will not modify it."},
			},
			"required": []string{"task"},
		},
	}, {
		"name":        "explore",
		"description": "Stage 0: the free worker explores the repo for a task and returns a distilled, machine-verified context — relevant files, grounded candidate edit sites (path:line + verbatim snippet, verified against disk), entrypoints/repro, and declared blind spots. It runs to completion in this one call (rig loops internally over large repos), so call it ONCE, before reading the repo yourself; then spot-check 2-3 citations and call triage.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task": map[string]any{"type": "string", "description": "The task to ground, with enough detail to search on."},
				"repo": map[string]any{"type": "string", "description": "Absolute path to the repo checkout. Defaults to the server's working directory."},
			},
			"required": []string{"task"},
		},
	}, {
		"name":        "triage",
		"description": "Declare the intake decision after explore: solo (single obvious small edit at a verified site — you edit it yourself) or delegate (multi-file / needs investigation / uncertain — hand to implement). A deterministic consistency check against the Stage-0 evidence may override solo to delegate. If unsure, declare delegate.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"decision": map[string]any{"type": "string", "enum": []string{"solo", "delegate"}},
				"reason":   map[string]any{"type": "string", "description": "One line: why this decision, citing the Stage-0 evidence."},
			},
			"required": []string{"decision", "reason"},
		},
	}, {
		"name":        "show_change",
		"description": "Show the raw bytes behind one entry of a tiered return: the actual git diff hunks covering a line range, or the full test log. When implement returns a manifest of changes instead of the diff itself, this is how you review one — call it with that entry's `file`, `line` and `end_line`. It returns git's own output verbatim, so read it as you would the diff. Prefer this over reading the changed file directly: the file shows the post-change state only, this shows what actually changed.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file":       map[string]any{"type": "string", "description": "Repo-relative path from the manifest entry."},
				"start_line": map[string]any{"type": "integer", "description": "First line of the range, post-change numbering (a manifest entry's `line`)."},
				"end_line":   map[string]any{"type": "integer", "description": "Last line of the range (a manifest entry's `end_line`). Defaults to start_line."},
				"kind":       map[string]any{"type": "string", "enum": []string{"diff", "test_log"}, "description": "\"diff\" (default) drills the working-tree change; \"test_log\" drills the parked raw test output."},
				"repo":       map[string]any{"type": "string", "description": "Absolute path to the repo checkout. Defaults to the server's working directory."},
			},
			"required": []string{"file", "start_line"},
		},
	}}
}

func (s *Server) onToolsCall(params json.RawMessage) (map[string]any, *rpcError) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return nil, &rpcError{Code: -32602, Message: "bad params: " + err.Error()}
	}
	switch call.Name {
	case "implement":
		// falls through to the loop below
	case "explore":
		return s.onExplore(call.Arguments), nil
	case "triage":
		return s.onTriage(call.Arguments), nil
	case "show_change":
		return s.onShowChange(call.Arguments), nil
	default:
		return nil, &rpcError{Code: -32602, Message: "unknown tool: " + call.Name}
	}

	var args struct {
		Task    string `json:"task"`
		Repo    string `json:"repo"`
		GateDir string `json:"gate_dir"`
	}
	_ = json.Unmarshal(call.Arguments, &args)
	if strings.TrimSpace(args.Task) == "" {
		return toolText(`{"error":"task is required"}`, true), nil
	}
	if args.Repo == "" {
		args.Repo, _ = os.Getwd()
	}
	if args.GateDir == "" {
		if cand := args.Repo + "/.gate"; dirExists(cand) {
			args.GateDir = cand
		}
	}

	s.noteRepo(args.Repo)

	ctx, cancel := context.WithTimeout(context.Background(), runTimeout())
	defer cancel()
	logStderr("worker.implement repo=%s gate=%s", args.Repo, args.GateDir)
	res := s.engine.Implement(ctx, args.Repo, args.Task, args.GateDir)

	// A round the engine flagged unproductive must not cost MAIN a delegation
	// slot (the budget exists to stop runaway retries, not to charge for rounds
	// that produced nothing). The refund happens here — the process that computed
	// the flag — against the same state dir the hook's budget reads; the cap that
	// keeps the loop bounded lives in gatestate.RefundRound.
	if res.Unproductive {
		r, ok := gatestate.RefundRound(s.engine.stateDir())
		logStderr("worker.implement unproductive justification=%q refund=%v effective=%d refunded=%d",
			res.UnproductiveJustification, ok, r.Effective(), r.Refunded)
	}

	// The default return is the plain C0 payload: the B6 gate failed the tiered
	// return on both axes, and the cc engine's winning numbers were all measured
	// against this exact surface, byte for byte.
	if !returnTieringOn() {
		body, _ := json.MarshalIndent(res, "", "  ")
		logStderr("worker.implement done stopped=%s iters=%d in=%d out=%d files=%v",
			res.Stopped, res.Iterations, res.InputTokens, res.OutputTokens, res.FilesChanged)
		return toolText(string(body), res.Failed()), nil
	}

	// The return contract (R9): the diff and the test log are the two things that
	// get re-cached on every later MAIN turn, so they are tiered here — on the rig
	// side of the boundary — before anything is serialized. The worker never sees
	// the threshold (it cannot shape its diff to beat it) and MAIN never has to
	// read the diff in order to decide whether to read the diff.
	payload := TierResult(res, args.Repo, returnThreshold())
	body, _ := json.MarshalIndent(payload, "", "  ")
	logStderr("%s", implementSummary(res, payload))
	return toolText(string(body), res.Failed()), nil
}

// onExplore runs the Stage-0 explore loop and returns the gated report.
func (s *Server) onExplore(arguments json.RawMessage) map[string]any {
	var args struct {
		Task string `json:"task"`
		Repo string `json:"repo"`
	}
	_ = json.Unmarshal(arguments, &args)
	if strings.TrimSpace(args.Task) == "" {
		return toolText(`{"error":"task is required"}`, true)
	}
	if args.Repo == "" {
		args.Repo, _ = os.Getwd()
	}
	ctx, cancel := context.WithTimeout(context.Background(), exploreTimeout())
	defer cancel()
	logStderr("worker.explore repo=%s", args.Repo)
	res := s.engine.Explore(ctx, args.Repo, args.Task)
	body, _ := json.MarshalIndent(res, "", "  ")
	logStderr("worker.explore done stopped=%s iters=%d anchors=%d read=%d sites=%d rejected=%d",
		res.Stopped, res.Iterations, len(res.Anchors), len(res.AnchorsRead), len(res.CandidateEditSites), len(res.RejectedSites))
	return toolText(string(body), res.Stopped == "error")
}

// onTriage records MAIN's intake declaration (Gate A) and returns the effective
// decision after the consistency backstop.
func (s *Server) onTriage(arguments json.RawMessage) map[string]any {
	var args struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	_ = json.Unmarshal(arguments, &args)
	out := s.engine.Triage(args.Decision, args.Reason, s.engine.cfg.GateMode)
	body, _ := json.MarshalIndent(out, "", "  ")
	logStderr("worker.triage declared=%s effective=%s overridden=%v", out.Declared, out.Effective, out.Overridden)
	return toolText(string(body), false)
}

// drillTimeout bounds one show_change. It only shells out to `git diff` on one
// path, so it is short by design: a drill that hangs stalls a review turn.
func drillTimeout() time.Duration {
	return time.Duration(envInt("RIG_DRILL_TIMEOUT", 30)) * time.Second
}

// onShowChange serves the drill (R9 §4). The body is returned as plain text
// rather than wrapped in JSON: it is a diff, MAIN reads diffs natively, and JSON
// escaping would both obscure it and inflate the very cost the tier exists to
// cut. The bookkeeping rides on one header line instead.
func (s *Server) onShowChange(arguments json.RawMessage) map[string]any {
	var args struct {
		File      string `json:"file"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
		Kind      string `json:"kind"`
		Repo      string `json:"repo"`
	}
	_ = json.Unmarshal(arguments, &args)
	if strings.TrimSpace(args.File) == "" {
		return toolText("show_change: file is required", true)
	}
	if args.Repo == "" {
		args.Repo = s.drillTarget()
	}

	ctx, cancel := context.WithTimeout(context.Background(), drillTimeout())
	defer cancel()
	res, err := ShowChange(ctx, args.Repo, args.File, args.StartLine, args.EndLine, args.Kind)
	if err != nil {
		logStderr("worker.show_change refused file=%s: %v", args.File, err)
		return toolText("show_change: "+err.Error(), true)
	}

	calls, total := DrilledSoFar()
	logStderr("worker.show_change file=%s:%d-%d kind=%s hunks=%d bytes=%d drilled_total=%d/%d",
		res.File, res.StartLine, res.EndLine, res.Kind, res.Hunks, len(res.Body), calls, total)

	head := fmt.Sprintf("%s:%d-%d (%s, %d B", res.File, res.StartLine, res.EndLine, res.Kind, len(res.Body))
	if res.Kind == drillKindDiff {
		head += fmt.Sprintf(", %d hunk(s), whole — hunks are never clipped at the range", res.Hunks)
	}
	return toolText(head+")\n"+res.Body, false)
}

// toolText wraps a text payload in the MCP tools/call result envelope.
func toolText(text string, isErr bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	}
}

func (s *Server) reply(id json.RawMessage, result interface{}) {
	s.write(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) replyErr(id json.RawMessage, code int, msg string) {
	s.write(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (s *Server) write(resp rpcResponse) {
	b, err := json.Marshal(resp)
	if err != nil {
		logStderr("mcp: marshal response: %v", err)
		return
	}
	s.out.Write(b)
	s.out.WriteByte('\n')
	s.out.Flush()
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func logStderr(format string, args ...any) {
	line := fmt.Sprintf("[rig-worker] "+format+"\n", args...)
	fmt.Fprint(os.Stderr, line)
	// A stdio MCP server's stderr belongs to whoever launched it — under Claude
	// Code it disappears into CC's own logs, out of reach of the measurement
	// harness. RIG_WORKER_LOG mirrors the same lines into a file the harness owns.
	if path := strings.TrimSpace(os.Getenv("RIG_WORKER_LOG")); path != "" {
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			_, _ = f.WriteString(line)
			_ = f.Close()
		}
	}
}

// implementSummary is the one line a measurement run reads back to tune the two
// numbers B3 left as guesses: RIG_RETURN_THRESHOLD, and the ×2 rule that drops
// the manifest from per-symbol to per-file. Tuning either needs what the gate
// decided (tier, granularity) *and* what it cost (manifest_tokens beside
// diff_tokens) on the same return.
func implementSummary(res Result, tiered TieredResult) string {
	manifestTokens := 0
	if tiered.Tier != "full" {
		// What MAIN actually receives, measured on the payload itself.
		body, _ := json.MarshalIndent(tiered, "", "  ")
		manifestTokens = estimateTokens(string(body))
	}
	return fmt.Sprintf(
		"worker.implement done stopped=%s iters=%d in=%d out=%d files=%v tier=%s granularity=%s diff_tokens=%d manifest_tokens=%d threshold_tokens=%d changes=%d",
		res.Stopped, res.Iterations, res.InputTokens, res.OutputTokens, res.FilesChanged,
		tiered.Tier, tiered.Granularity, tiered.DiffTokens, manifestTokens,
		tiered.ThresholdTokens, len(tiered.Changes))
}
