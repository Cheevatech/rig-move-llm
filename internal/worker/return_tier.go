package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Pointer is one entry of the tiered-return manifest: it names *what* changed
// without carrying any of the change itself. The raw substrate stays in the
// target repo's git working tree, so a pointer is lossless in the only sense
// that matters here — MAIN can drill back to the bytes through it (R4 §3, R9 §3).
type Pointer struct {
	File string `json:"file"`
	Line int    `json:"line"` // first changed line, post-change numbering
	// EndLine is the last changed line of the same symbol, so a pointer names a
	// range the drill tool can fetch, not just a starting point.
	EndLine   int    `json:"end_line"`
	Symbol    string `json:"symbol,omitempty"`
	Signature string `json:"signature,omitempty"`
	Kind      string `json:"kind"` // add | mod | del
	Added     int    `json:"added"`
	Removed   int    `json:"removed"`
}

// Pointers turns a unified `git diff` of repo into the manifest's pointer list.
//
// Resolution is per changed *line*, not per hunk: git's own hunk header names
// the last decl textually above the hunk without looking at indentation, so it
// reports the enclosing class for a method edit and goes stale across def
// boundaries (measured in B1). A single hunk routinely spans several symbols.
//
// Accuracy is best-effort by design. Drill fetches by file plus line range, not
// by symbol name, so a mislabelled symbol is a bad caption, not a broken fetch —
// and when the scanner cannot resolve a line it emits a bare file:line pointer
// rather than a guess.
func Pointers(repo, diff string) []Pointer {
	var out []Pointer
	src := newSourceCache(repo)

	for _, f := range parseDiffFiles(diff) {
		if f.deleted {
			out = append(out, Pointer{File: f.path, Line: 1, EndLine: 1, Kind: "del", Removed: f.removed})
			continue
		}
		lines := src.get(f.path)

		// Group changed lines by the symbol enclosing them; a symbol touched by
		// two hunks is one pointer.
		type group struct {
			Pointer
			declLine int
		}
		var order []string
		groups := map[string]*group{}
		for _, ch := range f.changes {
			sym, sig, decl := resolveSymbol(lines, ch.line)
			key := sym + "\x00" + strconv.Itoa(decl)
			g := groups[key]
			if g == nil {
				g = &group{Pointer: Pointer{
					File: f.path, Line: ch.line, Symbol: sym, Signature: sig, Kind: "mod",
				}, declLine: decl}
				groups[key] = g
				order = append(order, key)
			}
			if ch.line < g.Line || g.Line == 0 {
				g.Line = ch.line
			}
			if ch.line > g.EndLine {
				g.EndLine = ch.line
			}
			if ch.added {
				g.Added++
			} else {
				g.Removed++
			}
		}

		for _, key := range order {
			g := groups[key]
			switch {
			case f.isNew || (g.declLine > 0 && f.addedLines[g.declLine]):
				g.Kind = "add" // the declaration itself is new
			case g.Added == 0 && g.Removed > 0:
				g.Kind = "del"
			}
			out = append(out, g.Pointer)
		}
	}
	return out
}

// --- the threshold gate + manifest (R9 §1,2,3,7) --------------------------

// defaultReturnThreshold is the diff size, in estimated tokens, at which the
// return path stops shipping the diff body to MAIN and ships the manifest
// instead. R7 measured the cost of *not* gating: a verbose return re-cached on
// every later MAIN turn made sympy 42% MORE expensive than solving it solo, while
// small returns (flask, pytest) came out 30-39% cheaper. So the gate wants to sit
// above an ordinary small fix — a few hundred tokens of diff — and below the
// multi-file returns that caused the regression. 2000 tokens (~8KB of diff) is
// that seam; B6 tunes it against the real streams.
const defaultReturnThreshold = 2000

// returnThreshold resolves the gate's budget (RIG_RETURN_THRESHOLD), in the same
// mould as RIG_WORKER_CTX_LIMIT.
func returnThreshold() int { return envInt("RIG_RETURN_THRESHOLD", defaultReturnThreshold) }

// returnTieringOn reports whether the tiered return is applied at all
// (RIG_RETURN_TIERING=1). The default is OFF: the B6 gate failed the tiered
// return on both the cost and the safety axis, and every number that justifies
// the cc engine was measured against the plain C0 payload — so the product
// ships that exact surface, and tiering stays an experiment-only opt-in.
func returnTieringOn() bool { return envInt("RIG_RETURN_TIERING", 0) > 0 }

// noIntent reports whether worker prose is suppressed entirely
// (RIG_RETURN_NO_INTENT=1), leaving the return artifact deterministic: pointers
// and verify facts only. R11 wants this lockable at the source.
func noIntent() bool { return envInt("RIG_RETURN_NO_INTENT", 0) > 0 }

// Hard ceilings on everything the worker authors. Prose is a claim to be checked
// by drilling, so it is budgeted like a caption, not a report.
const (
	maxIntentBytes       = 400
	maxSymbolIntentBytes = 160
	maxVerifyLineBytes   = 240
	maxVerifyFailures    = 5
	maxVerifyAssertions  = 3
	intentTruncMark      = "…"
)

// drillTool is the one primitive on the MCP surface that returns a body, scoped
// to a pointer (R9 §4). The manifest names it with the exact arguments it takes,
// so the way back to the raw bytes travels with the summary. Contract shared with
// B4: {file, start_line, end_line, kind?} where kind is "diff" or "test_log".
const drillTool = "show_change"

// testLogName is where the raw test log is parked so the drill tool can serve it
// from the same {file, start_line, end_line} contract as a code hunk. It lives in
// the project dir rig already owns inside a checkout, and is untracked, so it
// never shows up in the diff it describes.
const testLogName = ".rig-move-llm/last_test.log"

// DrillRef is a ready-to-call drill invocation. Its presence is what makes a
// tiered return lossless in the only sense R9 claims: the raw substrate is still
// reachable, it just isn't resident.
type DrillRef struct {
	Tool string `json:"tool"`
	// Repo makes the ref self-contained. The drill's default target is the repo
	// implement ran in, but an explicit path is what keeps a review from ever
	// depending on that default being right (B6 run 2).
	Repo      string `json:"repo,omitempty"`
	File      string `json:"file"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Kind      string `json:"kind"` // diff | test_log
}

// Change is one manifest entry: the deterministic pointer plus the worker's claim
// about it (optional, capped, suppressible). Its own file/line/end_line ARE the
// drill arguments — see drillGuidance — so the entry pays nothing to repeat them.
type Change struct {
	Pointer
	Intent string `json:"intent,omitempty"`
	// Symbols counts the symbols folded into a file-granularity entry (see
	// TieredResult.Granularity); zero at symbol granularity.
	Symbols int `json:"symbols,omitempty"`
}

// VerifyTier is the test result at summary resolution. The raw log is the third
// bloat source R9 §7 names — pytest output runs to tens of thousands of tokens,
// and Result.LastTest carries it uncapped — so it is tiered at BOTH tiers, not
// just when the diff trips the gate.
type VerifyTier struct {
	Status string `json:"status"` // pass | fail | missing
	// Command is the gate this status came from. It is published because the
	// classifier that picks the gate out of the worker's bash calls is a
	// heuristic: naming the command lets review judge it instead of trust it.
	Command    string    `json:"command,omitempty"`
	Summary    string    `json:"summary"`
	Failures   []string  `json:"failures,omitempty"`
	Assertions []string  `json:"assertions,omitempty"`
	LogBytes   int       `json:"log_bytes,omitempty"`
	Drill      *DrillRef `json:"drill,omitempty"`
}

// TieredResult is what the implement tool serializes back to MAIN. It sits on top
// of Result rather than replacing it — Result stays the worker loop's own shape
// (B5 swaps the loop underneath it).
type TieredResult struct {
	Tier string `json:"tier"` // full | manifest
	// Repo is the checkout the pointers refer to, published so every drill can
	// be explicit about its target instead of inheriting a default.
	Repo            string   `json:"repo,omitempty"`
	Summary         string   `json:"summary,omitempty"`
	FilesChanged    []string `json:"files_changed,omitempty"`
	Diff            string   `json:"diff,omitempty"` // tier=full only
	DiffTokens      int      `json:"diff_tokens"`
	ThresholdTokens int      `json:"threshold_tokens"`
	Changes         []Change `json:"changes,omitempty"` // tier=manifest only
	// Granularity says what one entry in Changes stands for: "symbol" normally,
	// "file" when per-symbol entries cost more than the diff they replace.
	Granularity      string      `json:"granularity,omitempty"`
	Verify           *VerifyTier `json:"verify,omitempty"`
	DrillWith        string      `json:"drill_with,omitempty"`
	Iterations       int         `json:"iterations"`
	InputTokens      int         `json:"input_tokens"`
	OutputTokens     int         `json:"output_tokens"`
	Stopped          string      `json:"stopped"`
	HitIterationCap  bool        `json:"hit_iteration_cap,omitempty"`
	Checkpoints      int         `json:"checkpoints,omitempty"`
	Err              string      `json:"error,omitempty"`
	GateSource       string      `json:"gate_source,omitempty"`
	GateVerdict      string      `json:"gate_verdict,omitempty"`
	EngineGateCmd    string      `json:"engine_gate_cmd,omitempty"`
	EngineGateOutput string      `json:"engine_gate_output,omitempty"`
	WorkerVerdict    string      `json:"worker_verdict,omitempty"`
}

// drillGuidance tells MAIN what the manifest is and what to do with it: review
// from the pointers, drill the ones that look wrong. It is deliberately blunt
// about the intent lines being claims (R9 §3 / R3: a worker's account of its own
// work is a hypothesis, and trust comes from the gate, not from the prose).
const drillGuidance = "The diff exceeded the return budget, so it was left in the repo's git working tree and " +
	"summarized as pointers. Each `intent` is the worker's UNVERIFIED claim about its own change. Review from " +
	"the pointers, then read the raw hunk behind any change that looks wrong, surprising, or unexplained by " +
	"calling `" + drillTool + "` with {repo: <this result's `repo`>, file, start_line: <that change's `line`>, " +
	"end_line: <its `end_line`>, kind: \"diff\"}. ALWAYS pass `repo`: it is the checkout these pointers were " +
	"taken from, and a drill against any other tree cannot see this change. The full test output comes back the " +
	"same way, from the `drill` arguments under `verify` (kind: \"test_log\")."

// TierResult applies the return contract to one implement run: a deterministic
// size gate on the diff (R9 §2), a pointer manifest when it trips (R9 §3), and
// verify tiering always (R9 §7).
//
// The gate lives here, on the rig side of the MCP boundary, for two reasons R9
// insists on: the worker never learns the threshold so it cannot shape its diff
// to game it, and MAIN never has to see the diff in order to decide whether to
// see the diff.
func TierResult(res Result, repo string, thresholdTokens int) TieredResult {
	out := TieredResult{
		Summary:          intentLine(res.Summary),
		FilesChanged:     res.FilesChanged,
		DiffTokens:       estimateTokens(res.Diff),
		ThresholdTokens:  thresholdTokens,
		Iterations:       res.Iterations,
		InputTokens:      res.InputTokens,
		OutputTokens:     res.OutputTokens,
		Stopped:          res.Stopped,
		HitIterationCap:  res.HitIterationCap,
		Checkpoints:      res.Checkpoints,
		Err:              res.Err,
		GateSource:       res.GateSource,
		GateVerdict:      res.GateVerdict,
		EngineGateCmd:    res.EngineGateCmd,
		EngineGateOutput: res.EngineGateOutput,
		WorkerVerdict:    res.WorkerVerdict,
	}
	out.Repo = repo
	out.Verify = tierVerify(res.LastTest, res.LastTestCmd, repo)

	if thresholdTokens <= 0 || out.DiffTokens < thresholdTokens {
		out.Tier = "full"
		out.Diff = res.Diff
		return out
	}

	out.Tier = "manifest"
	intents := symbolIntents(res.Summary)
	for _, p := range Pointers(repo, res.Diff) {
		if p.EndLine < p.Line {
			p.EndLine = p.Line
		}
		c := Change{Pointer: p}
		// A per-symbol claim is adopted only for a symbol the deterministic layer
		// actually found: the worker can caption its changes, not invent them.
		if p.Symbol != "" {
			if s, ok := intents[p.Symbol]; ok {
				c.Intent = capBytes(s, maxSymbolIntentBytes)
			}
		}
		out.Changes = append(out.Changes, c)
	}
	out.Granularity = "symbol"

	// A manifest is only worth building if it is smaller than the diff it stands
	// in for, and a WIDE change breaks that: 128 one-line edits across 16 files
	// produce more pointer entries than diff (measured on a generated fixture).
	// Collapse to one entry per file rather than dropping entries — coverage is
	// what keeps review honest (invariant R1: every change stays drillable), so
	// the thing to give up under budget pressure is resolution, not reach. The
	// bar is a real squeeze, not merely "smaller": a manifest that costs half the
	// diff has not bought enough to be worth reviewing blind.
	if estimateTokens(manifestJSON(out.Changes))*2 >= out.DiffTokens {
		out.Changes = rollupByFile(out.Changes)
		out.Granularity = "file"
	}
	out.DrillWith = drillGuidance
	return out
}

// manifestJSON renders the manifest the way it will be billed, so the size guard
// measures the real cost and not an estimate of an estimate.
func manifestJSON(changes []Change) string {
	b, err := json.MarshalIndent(changes, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

// rollupByFile folds every symbol pointer of a file into one file-wide pointer:
// the union of the changed line range, the summed counts, and how many symbols it
// covers. Intent lines are dropped — a claim that no longer names one symbol is
// not a claim worth billing.
func rollupByFile(changes []Change) []Change {
	var order []string
	byFile := map[string]*Change{}
	for _, c := range changes {
		f := byFile[c.File]
		if f == nil {
			roll := Change{Pointer: Pointer{File: c.File, Line: c.Line, EndLine: c.EndLine, Kind: c.Kind}}
			byFile[c.File] = &roll
			order = append(order, c.File)
			f = &roll
		}
		if c.Line < f.Line {
			f.Line = c.Line
		}
		if c.EndLine > f.EndLine {
			f.EndLine = c.EndLine
		}
		if c.Kind != f.Kind {
			f.Kind = "mod" // a file with mixed add/mod/del edits is simply modified
		}
		f.Added += c.Added
		f.Removed += c.Removed
		if c.Symbol != "" {
			f.Symbols++
		}
	}
	out := make([]Change, 0, len(order))
	for _, f := range order {
		out = append(out, *byFile[f])
	}
	return out
}

// estimateTokens is a free, monotonic stand-in for a tokenizer (this binary ships
// with none, and pulling one in for a size gate would cost more than it saves).
// Four bytes per token is the usual rule of thumb for code; the gate only needs
// the ordering to be right, not the count.
func estimateTokens(s string) int { return (len(s) + 3) / 4 }

// intentLine caps the worker's own account of the run, and drops it entirely when
// prose is locked off.
func intentLine(summary string) string {
	if noIntent() {
		return ""
	}
	s := summary
	// Strip a machine-readable intents block so it is not billed twice (it is
	// re-emitted per symbol in the manifest).
	if raw := extractJSON(s); raw != "" && strings.Contains(raw, "\"intents\"") {
		s = strings.Replace(s, raw, "", 1)
	}
	return capBytes(strings.TrimSpace(s), maxIntentBytes)
}

// symbolIntents pulls an optional {"intents": {"Symbol": "one line"}} block out of
// the worker's reply, reusing the same tolerant extractor the explore report gate
// uses. Workers that emit no such block simply get no per-symbol captions.
func symbolIntents(summary string) map[string]string {
	if noIntent() {
		return nil
	}
	raw := extractJSON(summary)
	if raw == "" {
		return nil
	}
	var envelope struct {
		Intents map[string]string `json:"intents"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return nil
	}
	return envelope.Intents
}

// tierVerify reduces a raw test log to what review acts on, and parks the log so
// the rest stays fetchable. Green is one line; red keeps the failing test ids and
// the assertion text, which is what tells MAIN whether the fix is real.
func tierVerify(log, cmd, repo string) *VerifyTier {
	if strings.TrimSpace(log) == "" {
		return &VerifyTier{
			Status:  "missing",
			Summary: "no test command was run by the worker — nothing verifies this change",
		}
	}
	v := &VerifyTier{Status: exitStatus(log), Command: cmd, LogBytes: len(log)}
	lines := strings.Split(log, "\n")

	for _, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "FAILED "), strings.HasPrefix(t, "ERROR "):
			if len(v.Failures) < maxVerifyFailures {
				v.Failures = append(v.Failures, capBytes(t, maxVerifyLineBytes))
			}
		case t == "E" || strings.HasPrefix(t, "E "), strings.HasPrefix(t, "E\t"):
			if len(v.Assertions) < maxVerifyAssertions {
				v.Assertions = append(v.Assertions, capBytes(strings.TrimSpace(t[1:]), maxVerifyLineBytes))
			}
		}
	}
	v.Summary = verifySummaryLine(lines, v.Status)

	// The raw log is not in git, so parking it is what lets one drill contract
	// serve both a code hunk and the log behind a verify summary.
	if rel, n, err := parkTestLog(repo, log); err == nil {
		v.Drill = &DrillRef{Tool: drillTool, Repo: repo, File: rel, StartLine: 1, EndLine: n, Kind: "test_log"}
	}
	return v
}

// exitStatus reads the run_bash exit trailer, which is rig's own marker and so is
// more trustworthy than pattern-matching a test runner's output.
func exitStatus(log string) string {
	i := strings.LastIndex(log, "[exit ")
	killed := strings.Contains(log, "[killed:") || strings.Contains(log, "[run error:")
	switch {
	case killed:
		return "fail"
	case i < 0:
		return "unknown"
	case strings.HasPrefix(log[i:], "[exit 0]"):
		return "pass"
	default:
		return "fail"
	}
}

// verifySummaryLine picks the runner's own tally line — pytest's "N passed" /
// "N failed" banner — falling back to the last non-empty line. Banner padding
// ('=' rules) is stripped so the line costs what it says.
func verifySummaryLine(lines []string, status string) string {
	want := "passed"
	if status != "pass" {
		want = "failed"
	}
	var last string
	pick := ""
	for _, l := range lines {
		t := strings.Trim(strings.TrimSpace(l), "= ")
		if t == "" || strings.HasPrefix(t, "[exit") {
			continue
		}
		last = t
		if strings.Contains(t, want) && (strings.Contains(t, " in ") || strings.Contains(t, "passed")) {
			pick = t // later tallies win: the final banner is the authoritative one
		}
	}
	if pick == "" {
		pick = last
	}
	return capBytes(pick, maxVerifyLineBytes)
}

// parkTestLog writes the raw log inside repo and returns its repo-relative path
// plus its line count, which are exactly the drill arguments for it.
func parkTestLog(repo, log string) (string, int, error) {
	abs, err := safeJoin(repo, testLogName)
	if err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", 0, err
	}
	if err := os.WriteFile(abs, []byte(log), 0o644); err != nil {
		return "", 0, err
	}
	return testLogName, strings.Count(log, "\n") + 1, nil
}

func capBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + intentTruncMark
}

// --- unified diff parsing -------------------------------------------------

type diffChange struct {
	line  int // post-change line number ("-" lines take the position they left)
	added bool
}

type diffFile struct {
	path       string
	isNew      bool
	deleted    bool
	inHunk     bool
	removed    int
	changes    []diffChange
	addedLines map[int]bool
}

func parseDiffFiles(diff string) []*diffFile {
	var files []*diffFile
	var cur *diffFile
	newLine := 0

	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			cur = &diffFile{path: gitDiffPath(l), addedLines: map[int]bool{}}
			files = append(files, cur)
			newLine = 0
		case cur == nil:
			// preamble noise
		case strings.HasPrefix(l, "new file mode"):
			cur.isNew = true
		case strings.HasPrefix(l, "deleted file mode"), l == "+++ /dev/null":
			cur.deleted = true
		case strings.HasPrefix(l, "@@"):
			cur.inHunk = true
			newLine = hunkNewStart(l)
		case strings.HasPrefix(l, "\\"):
			// "\ No newline at end of file"
		case strings.HasPrefix(l, "-") && cur.inHunk:
			// Counted even for a deleted file, whose new side has no lines at all.
			cur.removed++
			if newLine > 0 {
				cur.changes = append(cur.changes, diffChange{line: newLine})
			}
		case newLine == 0:
			// header noise, or a hunk with no new side
		case strings.HasPrefix(l, "+"):
			cur.changes = append(cur.changes, diffChange{line: newLine, added: true})
			cur.addedLines[newLine] = true
			newLine++
		default: // context line (leading space, or the empty tail line)
			newLine++
		}
	}
	return files
}

// gitDiffPath pulls the post-change path out of a `diff --git a/x b/x` header.
func gitDiffPath(header string) string {
	rest := strings.TrimPrefix(header, "diff --git ")
	i := strings.Index(rest, " b/")
	if i < 0 {
		return strings.TrimPrefix(rest, "a/")
	}
	return strings.TrimPrefix(rest[i+1:], "b/")
}

// hunkNewStart reads C out of "@@ -a,b +c,d @@ …". Returns 0 when unparseable
// or when the new side is empty (a deleted file), which suppresses resolution.
func hunkNewStart(l string) int {
	i := strings.Index(l, "+")
	if i < 0 {
		return 0
	}
	num := l[i+1:]
	if j := strings.IndexAny(num, ", @"); j >= 0 {
		num = num[:j]
	}
	n, err := strconv.Atoi(num)
	if err != nil {
		return 0
	}
	return n
}

// --- the scanner ----------------------------------------------------------

// resolveSymbol is enclosingSymbol plus the line the innermost decl sits on,
// which Pointers needs to tell a brand-new symbol from an edited one.
func resolveSymbol(lines []string, line int) (symbol, signature string, declLine int) {
	if len(lines) == 0 || line < 1 || line > len(lines) {
		return "", "", 0
	}
	// Baseline: the indentation of the block the line lives in. Blank lines
	// carry none, so borrow it from the nearest code line above.
	i := line - 1
	for i >= 0 && strings.TrimSpace(lines[i]) == "" {
		i--
	}
	if i < 0 {
		return "", "", 0
	}

	limit := indentOf(lines[i])
	var names []string
	if name, ok := declName(lines[i]); ok {
		names = append(names, name)
		signature = strings.TrimSpace(lines[i])
		declLine = i + 1
		if limit == 0 {
			return name, signature, declLine
		}
		i--
	}

	// Walk up collecting each decl that opens a strictly shallower block: in
	// Python indentation *is* the block structure, which is exactly what git's
	// hunk header ignores.
	for ; i >= 0 && limit > 0; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		ind := indentOf(lines[i])
		if ind >= limit {
			continue
		}
		name, ok := declName(lines[i])
		if !ok {
			continue
		}
		names = append(names, name)
		if signature == "" {
			signature = strings.TrimSpace(lines[i])
			declLine = i + 1
		}
		limit = ind
	}

	for l, r := 0, len(names)-1; l < r; l, r = l+1, r-1 {
		names[l], names[r] = names[r], names[l]
	}
	return strings.Join(names, "."), signature, declLine
}

// enclosingSymbol returns the dotted path of the Python symbol containing the
// given 1-based line, plus that symbol's declaration line, or ("", "") when the
// line sits at module level or cannot be resolved.
func enclosingSymbol(lines []string, line int) (symbol, signature string) {
	s, sig, _ := resolveSymbol(lines, line)
	return s, sig
}

func indentOf(l string) int {
	n := 0
	for _, r := range l {
		switch r {
		case ' ':
			n++
		case '\t':
			n += 4
		default:
			return n
		}
	}
	return n
}

// declName reports the name a `def` / `async def` / `class` line declares.
func declName(l string) (string, bool) {
	t := strings.TrimSpace(l)
	t = strings.TrimPrefix(t, "async ")
	for _, kw := range []string{"def ", "class "} {
		if !strings.HasPrefix(t, kw) {
			continue
		}
		name := strings.TrimSpace(t[len(kw):])
		if i := strings.IndexAny(name, "(: \t"); i >= 0 {
			name = name[:i]
		}
		if name == "" {
			return "", false
		}
		return name, true
	}
	return "", false
}

// --- source access --------------------------------------------------------

// sourceCache reads post-change files off disk once each. Only Python resolves
// today; every other extension degrades to a bare file:line pointer (multi-lang
// is deferred behind the R13 gate).
type sourceCache struct {
	repo  string
	files map[string][]string
}

func newSourceCache(repo string) *sourceCache {
	return &sourceCache{repo: repo, files: map[string][]string{}}
}

func (c *sourceCache) get(rel string) []string {
	if lines, ok := c.files[rel]; ok {
		return lines
	}
	var lines []string
	if strings.EqualFold(filepath.Ext(rel), ".py") {
		if abs, err := safeJoin(c.repo, rel); err == nil {
			if b, err := os.ReadFile(abs); err == nil {
				lines = strings.Split(string(b), "\n")
			}
		}
	}
	c.files[rel] = lines
	return lines
}
