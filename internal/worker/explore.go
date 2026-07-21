package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/gatestate"
	"github.com/Cheevatech/rig-move-llm/pkg/translate"
)

// Stage 0 of the explore-first flow: the free worker reads the repo and returns
// a distilled, GROUNDED context for MAIN. The worker is not trusted — four
// deterministic gates verify its report before it reaches MAIN:
//
//	0a grounding      — every candidate edit site carries path:line + a verbatim
//	                    snippet; rig re-reads the file and matches it against disk.
//	0b coverage       — rig pre-computes a ranked anchor set (ComputeAnchors) and
//	                    counts the worker's real read_file calls; the report is
//	                    bounced until the top anchors were actually read.
//	0c schema         — the report must be strict JSON with all mandatory keys
//	                    (open_questions / unread_but_flagged force the worker to
//	                    declare its own blind spots).
//	0d comprehension  — deterministic minimum: each cited span must contain a real
//	                    identifier, and code-shaped terms from the rationale must
//	                    appear near the cited span. (MAIN is additionally steered
//	                    to spot-check 2–3 citations itself — cheap because targeted.)

// Explore is resumable, and rig OWNS the resume loop. A real repo's files are far
// larger than one bounded read pass, so exploration is chunked into internal
// ROUNDS — but the round loop runs entirely inside this one MCP call, driven by
// rig, NOT by the calling agent. Each round seeds a fresh worker conversation
// with the accumulated progress digest (files already read, sites found, anchors
// still to read), runs a bounded read pass, gate-checks it, and merges the result;
// rig keeps starting rounds until coverage is complete or the round ceiling is hit.
//
// This keeps the contract minimal on both sides: the worker is a plain
// OpenAI-compatible endpoint that only answers Chat Completions (nothing special
// required of it), and the calling agent (Claude Code or any harness) makes ONE
// explore call and gets a finished, gate-verified report — it never has to loop.
// Progress is still persisted per round for crash recovery / observability.
const (
	// roundIters bounds ONE round's agentic read pass, keeping the worker's context
	// window bounded (a round reads a handful of files, then checkpoints into the
	// progress digest and the next round starts fresh). Override RIG_EXPLORE_MAX_ITERS.
	roundIters = 14
	// maxExploreRounds caps how many internal rounds rig runs before returning a
	// best-effort report (guards against an endless explore loop).
	maxExploreRounds = 6
	// requiredAnchorCount is how many top-ranked anchors the coverage gate insists
	// the worker reads (accumulated across rounds).
	requiredAnchorCount = 5
	maxAnchorSet        = 8
	// maxGateBounces bounds how many times a failing report is sent back within one
	// round.
	maxGateBounces = 2
	// pauseMargin is how many iterations before a round's cap we tell the worker to
	// checkpoint: emit what it has so the round always yields a report to merge.
	pauseMargin = 2
)

// EditSite is one grounded candidate edit location in the Stage-0 report.
type EditSite struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Snippet   string `json:"snippet"`
	Rationale string `json:"rationale"`
}

// exploreReport is the strict JSON contract (gate 0c) the worker must emit.
type exploreReport struct {
	RelevantFiles      []string   `json:"relevant_files"`
	CandidateEditSites []EditSite `json:"candidate_edit_sites"`
	EntrypointsOrRepro []string   `json:"entrypoints_or_repro"`
	OpenQuestions      []string   `json:"open_questions"`
	UnreadButFlagged   []string   `json:"unread_but_flagged"`
}

// ExploreResult is what the explore tool returns to MAIN: the verified report
// plus gate outcomes and loop accounting.
type ExploreResult struct {
	exploreReport
	Anchors            []string `json:"anchors"`      // rig-computed candidate files (ranked)
	AnchorsRead        []string `json:"anchors_read"` // which of them the worker actually read
	CoverageIncomplete bool     `json:"coverage_incomplete,omitempty"`
	RejectedSites      []string `json:"rejected_sites,omitempty"` // "path:line — reason" (failed 0a/0d)
	Iterations         int      `json:"iterations"`               // total across all internal rounds
	Rounds             int      `json:"rounds"`                   // how many internal read passes rig ran
	InputTokens        int      `json:"input_tokens"`
	OutputTokens       int      `json:"output_tokens"`
	Stopped            string   `json:"stopped"` // "done" | "max_rounds" | "error"
	Err                string   `json:"error,omitempty"`
	Advice             string   `json:"advice,omitempty"`
}

const exploreSystemPrompt = `You are a repo-exploration worker. Your ONLY job is to READ the repository and
report grounded evidence for the task — you must NOT edit anything.
You have three read-only tools: read_file, grep_repo, list_dir.
Requirements:
1. Read every file in the ANCHOR LIST you are given (use read_file; large files may be read in line ranges).
2. Follow the evidence: grep for the task's identifiers, read what matters.
3. Reply with ONLY a JSON object (no prose, no markdown fences) with EXACTLY these keys:
   {"relevant_files": [..], "candidate_edit_sites": [{"path": .., "line": <int>, "snippet": "<verbatim line(s) copied from the file>", "rationale": ".."}],
    "entrypoints_or_repro": [..], "open_questions": [..], "unread_but_flagged": [..]}
   - snippet MUST be copied verbatim from the file (it is machine-verified against disk; paraphrasing = rejection).
   - open_questions: what you could not determine. unread_but_flagged: files likely relevant that you did not read.
   Both keys are mandatory (empty arrays if none) — declaring blind spots is part of the job.
Paths are relative to the repo root. Do not ask questions — explore, then report.
Exploration runs in rounds: if you are told your budget is nearly up, emit the JSON with what you have
so far (put files you still need to read into unread_but_flagged) — you will be given another round to
continue reading them, with your progress preserved. Do not rush a wrong answer to finish in one pass.`

// Explore runs Stage 0 to completion in ONE MCP call: rig drives an internal
// round loop against the worker endpoint until coverage is complete or the round
// ceiling is hit, then returns the gate-verified report. The caller makes a single
// call — the resume control lives here, not in the calling agent.
func (e *Engine) Explore(ctx context.Context, repo, task string) ExploreResult {
	res := ExploreResult{Stopped: "error"}
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		res.Err = "bad repo path: " + err.Error()
		return res
	}
	if fi, err := os.Stat(absRepo); err != nil || !fi.IsDir() {
		res.Err = "repo is not a directory: " + absRepo
		return res
	}

	anchors := ComputeAnchors(ctx, absRepo, task, maxAnchorSet)
	required := map[string]bool{}
	var anchorLines []string
	for i, a := range anchors {
		res.Anchors = append(res.Anchors, a.Path)
		mark := ""
		if i < requiredAnchorCount {
			required[a.Path] = true
			mark = " (MUST read)"
		}
		anchorLines = append(anchorLines, fmt.Sprintf("- %s%s (matches: %s)", a.Path, mark, strings.Join(a.Via, ", ")))
	}

	// Resume any progress a prior (crashed/interrupted) call left for this exact
	// task+repo; normally this starts empty and rig fills it round by round.
	prog, _ := e.loadProgress(absRepo, task)
	read := map[string]bool{}
	for _, f := range prog.ReadFiles {
		read[f] = true
	}

	var report *exploreReport
	coverageDone := len(unreadRequired(required, read)) == 0

	// rig OWNS the round loop: keep exploring until covered or the ceiling is hit.
	for round := prog.Round + 1; round <= maxExploreRounds; round++ {
		res.Rounds = round
		if err := ctx.Err(); err != nil {
			// Out of time budget — stop looping and finalize best-effort below.
			break
		}
		rep, iters, inTok, outTok, rejected, rerr := e.exploreRound(ctx, absRepo, task, anchorLines, required, prog, round, read)
		res.Iterations += iters
		res.InputTokens += inTok
		res.OutputTokens += outTok
		if rejected != nil {
			res.RejectedSites = rejected
		}
		if rerr != "" {
			// A hard round error (endpoint down / unparseable after retries). Keep any
			// progress so far and finalize best-effort rather than losing the run.
			res.Err = rerr
			break
		}

		prog = mergeProgress(prog, task, absRepo, read, rep)
		prog.Round = round
		report = rep
		coverageDone = len(unreadRequired(required, read)) == 0

		if rep != nil && coverageDone {
			break // fully covered + a verified report → done
		}
		// Not finished — persist progress (crash recovery) and run another round.
		e.saveProgress(prog)
	}

	for p := range read {
		if contains(res.Anchors, p) {
			res.AnchorsRead = append(res.AnchorsRead, p)
		}
	}
	sort.Strings(res.AnchorsRead)
	res.exploreReport = progressReport(prog)

	switch {
	case report != nil && coverageDone && res.Err == "":
		res.Stopped = "done"
		res.Advice = "Evidence above is machine-verified against disk (grounding/coverage/schema gates). " +
			"Spot-check 2-3 citations, then declare intake triage via mcp__worker__triage: \"solo\" (single " +
			"obvious small edit at a verified site) or \"delegate\" (multi-file / needs investigation / uncertain). If unsure, delegate."
		e.clearProgress()
	default:
		// Ceiling hit, out of time, or an endpoint error — hand back the best-effort
		// accumulated report, flagged incomplete so triage forces delegate.
		res.Stopped = "max_rounds"
		res.CoverageIncomplete = true
		if unread := unreadRequired(required, read); len(unread) > 0 {
			res.UnreadButFlagged = union(res.UnreadButFlagged, unread)
		}
		res.Advice = "Explore did not fully cover the repo (round ceiling / time / endpoint). Triage via " +
			"mcp__worker__triage; given the incomplete evidence, prefer \"delegate\"."
		e.clearProgress()
	}

	e.persistExplore(absRepo, res)
	e.writeExploreDebug(absRepo, res)
	return res
}

// exploreRound runs one bounded read pass, seeded with the accumulated progress.
// It returns the gate-checked report for this round (nil if the worker never
// produced a valid one), iteration/token counts, any grounding rejections, and a
// non-empty error string only on a hard failure (endpoint unreachable / schema
// unrecoverable). read is mutated in place with the files this round touched.
func (e *Engine) exploreRound(ctx context.Context, absRepo, task string, anchorLines []string, required map[string]bool, prog exploreProgress, round int, read map[string]bool) (report *exploreReport, iters, inTok, outTok int, rejected []string, errStr string) {
	user := "TASK:\n" + task + "\n\nANCHOR LIST (deterministically derived from the task; read every MUST entry):\n"
	if len(anchorLines) > 0 {
		user += strings.Join(anchorLines, "\n")
	} else {
		user += "(no anchors found — locate the relevant files yourself via grep_repo/list_dir)"
	}
	if round > 1 {
		user += fmt.Sprintf("\n\nCONTINUING (round %d). Already read: %s.", round, strings.Join(prog.ReadFiles, ", "))
		if unread := unreadRequired(required, read); len(unread) > 0 {
			user += " Still MUST read: " + strings.Join(unread, ", ") + "."
		}
		if len(prog.Sites) > 0 {
			user += fmt.Sprintf(" Candidate sites found so far: %d (re-include them in your report).", len(prog.Sites))
		}
	}

	msgs := []translate.OpenAIMessage{
		{Role: "system", Content: exploreSystemPrompt},
		{Role: "user", Content: user},
	}

	bounces := 0
	checkpointed := false
	callIters := envInt("RIG_EXPLORE_MAX_ITERS", roundIters)
	limit := ctxLimit()

	for i := 0; i < callIters; i++ {
		iters = i + 1
		if err := ctx.Err(); err != nil {
			return report, iters, inTok, outTok, rejected, ""
		}
		resp, usage, err := e.chat(ctx, msgs, exploreToolSchema())
		if err != nil {
			return nil, iters, inTok, outTok, rejected, "chat: " + err.Error()
		}
		inTok += usage[0]
		outTok += usage[1]
		// Primary round-end trigger is now the real context size (usage[0] =
		// prompt_tokens this round has accumulated); the iteration cap below is the
		// secondary safety net. A single big-file read can blow the context long
		// before the iteration count runs out, so token is the honest signal.
		budgetTripped := overCtxBudget(usage[0], limit)
		if len(resp.Choices) == 0 {
			return nil, iters, inTok, outTok, rejected, "worker returned no choices"
		}
		m := resp.Choices[0].Message
		msgs = append(msgs, translate.OpenAIMessage{Role: "assistant", Content: m.Content, ToolCalls: m.ToolCalls})

		if len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				out := e.execExploreTool(ctx, absRepo, tc, read)
				msgs = append(msgs, translate.OpenAIMessage{Role: "tool", ToolCallID: tc.ID, Content: truncate(out, defaultMaxOutputB)})
			}
			// Near this round's budget: ask the worker to checkpoint so the round
			// always yields a report to merge; unread files carry to the next round.
			// Trigger on the token budget (primary) OR the iteration cap (secondary).
			if !checkpointed && (budgetTripped || i >= callIters-pauseMargin) {
				checkpointed = true
				msgs = append(msgs, translate.OpenAIMessage{Role: "user", Content: "CHECKPOINT — this round's budget is nearly up. Emit the JSON report with what you have so far; " +
					"list files you still need to read in unread_but_flagged. You will get another round to continue."})
			}
			continue
		}

		// Final answer: run the gates. Fixable failures bounce back within this round.
		rep, problems := parseReport(m.Content) // gate 0c
		if rep == nil {
			if bounces < maxGateBounces {
				bounces++
				msgs = append(msgs, translate.OpenAIMessage{Role: "user", Content: "GATE FAILED (schema): " + strings.Join(problems, "; ") +
					"\nReply again with ONLY the JSON object in the exact schema."})
				continue
			}
			return nil, iters, inTok, outTok, rejected, "schema gate failed after retries: " + strings.Join(problems, "; ")
		}

		coverageDone := len(unreadRequired(required, read)) == 0
		if !coverageDone && !checkpointed && bounces < maxGateBounces {
			bounces++
			msgs = append(msgs, translate.OpenAIMessage{Role: "user", Content: "GATE (coverage): you have not read these MUST anchors: " +
				strings.Join(unreadRequired(required, read), ", ") + "\nRead them with read_file, then re-emit the JSON."})
			continue
		}

		verified, rej := verifySites(absRepo, rep.CandidateEditSites, task) // gates 0a + 0d
		if len(rej) > 0 && len(verified) == 0 && bounces < maxGateBounces && !checkpointed {
			bounces++
			msgs = append(msgs, translate.OpenAIMessage{Role: "user", Content: "GATE FAILED (grounding): every cited snippet failed verification against disk:\n- " +
				strings.Join(rej, "\n- ") + "\nCopy snippets VERBATIM from the files (re-read them if needed) and re-emit the JSON."})
			continue
		}
		rep.CandidateEditSites = verified
		rejected = rej
		report = rep
		break
	}
	return report, iters, inTok, outTok, rejected, ""
}

// writeExploreDebug dumps the full last explore result to <stateDir>/explore_last.json
// for observability — the MCP tool's stderr is not captured by Claude Code, so this
// is how a post-mortem sees what the worker actually returned (stopped, err, sites).
func (e *Engine) writeExploreDebug(repo string, res ExploreResult) {
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(e.stateDir(), "explore_last.json"), b, 0o644)
}

// persistExplore writes the digest the triage consistency backstop reads.
func (e *Engine) persistExplore(repo string, res ExploreResult) {
	files := map[string]bool{}
	for _, s := range res.CandidateEditSites {
		files[s.Path] = true
	}
	editFiles := make([]string, 0, len(files))
	for f := range files {
		editFiles = append(editFiles, f)
	}
	sort.Strings(editFiles)
	_ = gatestate.WriteExplore(e.stateDir(), gatestate.Explore{
		Repo:               repo,
		RelevantFiles:      res.RelevantFiles,
		EditSiteFiles:      editFiles,
		NSites:             len(res.CandidateEditSites),
		NOpenQuestions:     len(res.OpenQuestions),
		CoverageIncomplete: res.CoverageIncomplete,
		At:                 time.Now(),
	})
}

// stateDir mirrors the hook's state resolution (RIG_STATE_DIR override, else the
// scope data dir) so both sides read the same files.
func (e *Engine) stateDir() string {
	if d := os.Getenv("RIG_STATE_DIR"); d != "" {
		return d
	}
	return e.cfg.DataDir
}

// --- gate 0c: schema ---

// parseReport extracts and validates the strict JSON report. nil + problems on
// failure.
func parseReport(content string) (*exploreReport, []string) {
	raw := extractJSON(content)
	if raw == "" {
		return nil, []string{"no JSON object found in the reply"}
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &keys); err != nil {
		return nil, []string{"invalid JSON: " + err.Error()}
	}
	var problems []string
	for _, k := range []string{"relevant_files", "candidate_edit_sites", "entrypoints_or_repro", "open_questions", "unread_but_flagged"} {
		if _, ok := keys[k]; !ok {
			problems = append(problems, "missing mandatory key "+k)
		}
	}
	if len(problems) > 0 {
		return nil, problems
	}
	var rep exploreReport
	if err := json.Unmarshal([]byte(raw), &rep); err != nil {
		return nil, []string{"schema mismatch: " + err.Error()}
	}
	for i, s := range rep.CandidateEditSites {
		if s.Path == "" || s.Line <= 0 || strings.TrimSpace(s.Snippet) == "" {
			problems = append(problems, fmt.Sprintf("candidate_edit_sites[%d] needs non-empty path, line>0, snippet", i))
		}
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return &rep, nil
}

// extractJSON returns the first balanced {...} object in s (fences tolerated).
func extractJSON(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// --- gates 0a + 0d: grounding + comprehension min-check ---

// verifySites re-reads every cited file and keeps only sites whose snippet
// matches disk (whitespace-normalized) within lineTolerance of the claimed line
// and passes the comprehension min-check. Rejections are returned as
// human-readable reasons.
const lineTolerance = 15

func verifySites(repo string, sites []EditSite, task string) (kept []EditSite, rejected []string) {
	fileCache := map[string][]string{}
	for _, s := range sites {
		reason := verifyOne(repo, s, task, fileCache)
		if reason == "" {
			kept = append(kept, s)
		} else {
			rejected = append(rejected, fmt.Sprintf("%s:%d — %s", s.Path, s.Line, reason))
		}
	}
	return kept, rejected
}

func verifyOne(repo string, s EditSite, task string, cache map[string][]string) string {
	lines, ok := cache[s.Path]
	if !ok {
		p, err := safeJoin(repo, s.Path)
		if err != nil {
			return "bad path: " + err.Error()
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return "cannot read file: " + err.Error()
		}
		lines = strings.Split(string(data), "\n")
		cache[s.Path] = lines
	}

	snip := normalizeWS(s.Snippet)
	if len(snip) < 8 {
		return "snippet too short to verify (copy the full line verbatim)"
	}
	snipLines := nonEmptyLines(s.Snippet)
	window := len(snipLines)
	if window == 0 {
		return "empty snippet"
	}
	// gate 0a: find the snippet on disk (whitespace-normalized sliding window).
	bestLine := -1
	for i := 0; i+window <= len(lines); i++ {
		cand := normalizeWS(strings.Join(lines[i:i+window], "\n"))
		if cand == snip || strings.Contains(cand, snip) {
			if bestLine == -1 || abs(i+1-s.Line) < abs(bestLine-s.Line) {
				bestLine = i + 1
			}
		}
	}
	if bestLine == -1 {
		return "snippet not found in file (must be verbatim)"
	}
	if abs(bestLine-s.Line) > lineTolerance {
		return fmt.Sprintf("snippet found at line %d, not near claimed line %d", bestLine, s.Line)
	}
	// gate 0d minimum: the cited span must contain a real identifier, and
	// code-shaped terms in the rationale must appear near the span.
	if len(identRe.FindAllString(s.Snippet, 1)) == 0 {
		return "cited span contains no identifier"
	}
	if terms := codeTerms(s.Rationale); len(terms) > 0 {
		lo := max0(bestLine - 30)
		hi := bestLine + window + 30
		if hi > len(lines) {
			hi = len(lines)
		}
		ctxText := strings.Join(lines[lo:hi], "\n") + "\n" + s.Snippet
		found := false
		for _, t := range terms {
			if strings.Contains(ctxText, t) {
				found = true
				break
			}
		}
		if !found {
			return "rationale names code terms (" + strings.Join(terms, ", ") + ") absent near the cited span"
		}
	}
	return ""
}

// codeTerms extracts code-shaped identifiers (snake_case / mixed case) from
// free text — the rationale terms gate 0d cross-checks.
func codeTerms(text string) []string {
	var out []string
	for _, tok := range identRe.FindAllString(text, -1) {
		lower := strings.ToLower(tok)
		if stopwords[lower] {
			continue
		}
		if strings.Contains(tok, "_") || (tok != lower && tok != strings.ToUpper(tok) && !isTitleWord(tok)) {
			out = append(out, tok)
			if len(out) >= 5 {
				break
			}
		}
	}
	return out
}

func normalizeWS(s string) string { return strings.Join(strings.Fields(s), " ") }

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// --- gate 0b: coverage ---

func unreadRequired(required, read map[string]bool) []string {
	var out []string
	for p := range required {
		if !read[p] {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// --- read-only tools for the explore loop ---

func exploreToolSchema() []translate.OpenAITool {
	return []translate.OpenAITool{
		{Type: "function", Function: translate.OpenAIToolFunction{
			Name:        "read_file",
			Description: "Read a UTF-8 text file from the repo (optionally a line range for large files). path is relative to the repo root.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"start_line":{"type":"integer"},"end_line":{"type":"integer"}},"required":["path"]}`),
		}},
		{Type: "function", Function: translate.OpenAIToolFunction{
			Name:        "grep_repo",
			Description: "Search the repo for a fixed string; returns matching lines as path:line:text.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string","description":"optional subdirectory or file to restrict the search"}},"required":["pattern"]}`),
		}},
		{Type: "function", Function: translate.OpenAIToolFunction{
			Name:        "list_dir",
			Description: "List the entries of a directory in the repo. path is relative to the repo root ('.' for the root).",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}},
	}
}

// execExploreTool runs one read-only tool call, recording read_file paths for
// the coverage gate. Tool failures return as text so the model can react.
func (e *Engine) execExploreTool(ctx context.Context, repo string, tc translate.OpenAIToolCall, read map[string]bool) string {
	switch tc.Function.Name {
	case "read_file":
		var a struct {
			Path      string `json:"path"`
			StartLine int    `json:"start_line"`
			EndLine   int    `json:"end_line"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &a); err != nil {
			return "error: bad arguments: " + err.Error()
		}
		p, err := safeJoin(repo, a.Path)
		if err != nil {
			return "error: " + err.Error()
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return "error: " + err.Error()
		}
		rel, _ := filepath.Rel(repo, p)
		read[filepath.ToSlash(rel)] = true
		lines := strings.Split(string(data), "\n")
		lo, hi := 1, len(lines)
		if a.StartLine > 0 {
			lo = a.StartLine
		}
		if a.EndLine > 0 && a.EndLine < hi {
			hi = a.EndLine
		}
		if lo > hi || lo > len(lines) {
			return fmt.Sprintf("error: line range %d-%d outside file (%d lines)", lo, hi, len(lines))
		}
		var b strings.Builder
		for i := lo; i <= hi; i++ {
			fmt.Fprintf(&b, "%d\t%s\n", i, lines[i-1])
		}
		return b.String()

	case "grep_repo":
		var a struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &a); err != nil {
			return "error: bad arguments: " + err.Error()
		}
		if strings.TrimSpace(a.Pattern) == "" {
			return "error: empty pattern"
		}
		args := []string{"grep", "-n", "-I", "-F", "--untracked", a.Pattern}
		if a.Path != "" {
			if _, err := safeJoin(repo, a.Path); err != nil {
				return "error: " + err.Error()
			}
			args = append(args, "--", a.Path)
		}
		out := gitOut(ctx, repo, args...)
		if strings.TrimSpace(out) == "" {
			return "(no matches)"
		}
		return out

	case "list_dir":
		var a struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &a); err != nil {
			return "error: bad arguments: " + err.Error()
		}
		if a.Path == "" {
			a.Path = "."
		}
		p := repo
		if a.Path != "." {
			var err error
			p, err = safeJoin(repo, a.Path)
			if err != nil {
				return "error: " + err.Error()
			}
		}
		ents, err := os.ReadDir(p)
		if err != nil {
			return "error: " + err.Error()
		}
		var b strings.Builder
		for _, e := range ents {
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			b.WriteString(name + "\n")
		}
		return b.String()

	default:
		return "error: unknown tool " + tc.Function.Name
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
