package worker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/pkg/translate"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// --- fixtures -------------------------------------------------------------

// bigDiffRepo builds a REAL repo whose working tree carries a diff far larger
// than any sane return threshold (the sympy-scale case R7 measured at +42%), and
// returns the repo plus `git diff` verbatim. Generated from git, never hand-written.
func bigDiffRepo(t *testing.T) (repo, diff string) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")

	// 12 modules × 8 methods each, then rewrite one line in every method: a wide
	// diff over many symbols, like a real refactor-shaped worker run.
	write := func(mod string, body func(*strings.Builder)) {
		var b strings.Builder
		body(&b)
		if err := os.WriteFile(filepath.Join(dir, mod), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 16; i++ {
		mod := fmt.Sprintf("mod%02d.py", i)
		write(mod, func(b *strings.Builder) {
			fmt.Fprintf(b, "class Shape%02d:\n", i)
			for m := 0; m < 8; m++ {
				fmt.Fprintf(b, "    def method%02d(self, value):\n", m)
				fmt.Fprintf(b, "        scaled = value * %d  # original\n", m+1)
				fmt.Fprintf(b, "        return scaled\n\n")
			}
		})
	}
	git("add", "-A")
	git("commit", "-qm", "base")

	for i := 0; i < 16; i++ {
		mod := fmt.Sprintf("mod%02d.py", i)
		write(mod, func(b *strings.Builder) {
			fmt.Fprintf(b, "class Shape%02d:\n", i)
			for m := 0; m < 8; m++ {
				fmt.Fprintf(b, "    def method%02d(self, value):\n", m)
				fmt.Fprintf(b, "        scaled = value * %d  # rewritten by the worker with a much longer trailing comment\n", m+1)
				fmt.Fprintf(b, "        return scaled\n\n")
			}
		})
	}
	out, err := exec.Command("git", "-C", dir, "diff").CombinedOutput()
	if err != nil {
		t.Fatalf("git diff: %v\n%s", err, out)
	}
	if len(out) < 20000 {
		t.Fatalf("fixture diff too small to exercise the gate: %d bytes", len(out))
	}
	return dir, string(out)
}

// deepDiffRepo is the other shape a real return takes: a big diff CONCENTRATED in
// a few symbols (the sympy case — one function rewritten at length), where a
// per-symbol manifest is a genuine squeeze.
func deepDiffRepo(t *testing.T) (repo, diff string) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")

	body := func(tag string) string {
		var b strings.Builder
		b.WriteString("class Point2D:\n")
		for _, m := range []string{"scale", "translate", "rotate"} {
			fmt.Fprintf(&b, "    def %s(self, factor):\n", m)
			for i := 0; i < 60; i++ {
				fmt.Fprintf(&b, "        step%02d = self._apply(factor, %d)  # %s pass over the coordinate pair\n", i, i, tag)
			}
			b.WriteString("        return self\n\n")
		}
		return b.String()
	}
	if err := os.WriteFile(filepath.Join(dir, "point2d.py"), []byte(body("original")), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-qm", "base")
	if err := os.WriteFile(filepath.Join(dir, "point2d.py"), []byte(body("rewritten")), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", dir, "diff").CombinedOutput()
	if err != nil {
		t.Fatalf("git diff: %v\n%s", err, out)
	}
	return dir, string(out)
}

// pointerRepoCopy is the pointer fixture's working tree in a scratch dir, so a
// run that parks its test log for drilling writes into a temp repo instead of
// this repo's testdata.
func pointerRepoCopy(t *testing.T) (repo, diff string) {
	t.Helper()
	src, diff := loadPointerFixture(t)
	repo = t.TempDir()
	for _, f := range []string{"point.py", "helpers.py"} {
		b, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo, diff
}

const greenLog = `============================= test session starts ==============================
collected 4 items

tests/test_point.py ....                                                 [100%]

============================== 4 passed in 0.42s ===============================
[exit 0]`

const redLog = `============================= test session starts ==============================
collected 4 items

tests/test_point.py ..F.                                                 [100%]

=================================== FAILURES ===================================
_____________________________ test_scale_negative ______________________________

    def test_scale_negative():
        p = Point2D(1, 2)
>       assert p.scale(-1) == (-1, -2)
E       assert (1, 2) == (-1, -2)
E         At index 0 diff: 1 != -1

tests/test_point.py:31: AssertionError
=========================== short test summary info ============================
FAILED tests/test_point.py::test_scale_negative - assert (1, 2) == (-1, -2)
========================= 1 failed, 3 passed in 0.51s ==========================
[exit 1]`

// --- the threshold gate (R9 §2) -------------------------------------------

// Under the threshold nothing changes about the diff leg: MAIN gets the whole
// diff, exactly as before the gate existed.
func TestTierResultUnderThresholdReturnsFullDiff(t *testing.T) {
	repo, diff := loadPointerFixture(t)
	res := Result{Summary: "fixed scale", Diff: diff, FilesChanged: []string{"point.py"}, Stopped: "done"}

	got := TierResult(res, repo, 100000)

	if got.Tier != "full" {
		t.Fatalf("tier = %q, want full", got.Tier)
	}
	if got.Diff != diff {
		t.Errorf("full tier must carry the diff verbatim (got %d bytes, want %d)", len(got.Diff), len(diff))
	}
	if len(got.Changes) != 0 {
		t.Errorf("full tier should not also pay for a manifest, got %d changes", len(got.Changes))
	}
}

// At or over the threshold the diff body never reaches MAIN — only the manifest.
func TestTierResultOverThresholdReturnsManifestOnly(t *testing.T) {
	repo, diff := bigDiffRepo(t)
	res := Result{Summary: "rewrote every method", Diff: diff, Stopped: "done", LastTest: greenLog}

	got := TierResult(res, repo, 500)

	if got.Tier != "manifest" {
		t.Fatalf("tier = %q, want manifest", got.Tier)
	}
	if got.Diff != "" {
		t.Fatalf("manifest tier leaked %d bytes of diff body", len(got.Diff))
	}
	// 128 one-line edits across 16 files: per-symbol entries would cost more than
	// the diff, so the manifest degrades resolution — but never coverage.
	if got.Granularity != "file" {
		t.Errorf("granularity = %q, want file for a wide diff", got.Granularity)
	}
	seen := map[string]bool{}
	for _, c := range got.Changes {
		seen[c.File] = true
	}
	for i := 0; i < 16; i++ {
		if f := fmt.Sprintf("mod%02d.py", i); !seen[f] {
			t.Errorf("%s changed but is missing from the manifest — undrillable", f)
		}
	}
	if got.DiffTokens < got.ThresholdTokens {
		t.Errorf("gate tripped with diff_tokens=%d below threshold=%d", got.DiffTokens, got.ThresholdTokens)
	}

	// The mechanism being bought: what MAIN re-caches must be a fraction of the
	// diff it replaces (the sympy 46k → manifest squeeze).
	body, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if manifest, full := estimateTokens(string(body)), estimateTokens(diff); manifest*2 >= full {
		t.Errorf("manifest costs %d tokens vs %d for the raw diff — no squeeze", manifest, full)
	}

	// And it must carry no line of the change itself.
	for _, needle := range []string{"rewritten by the worker", "+        scaled = value", "@@ -"} {
		if strings.Contains(string(body), needle) {
			t.Errorf("manifest payload leaked diff body %q", needle)
		}
	}
}

// The manifest is only lossless if it says how to get the raw bytes back, in the
// exact argument shape the drill tool accepts (contract locked with B4).
func TestManifestDrillArgsMatchShowChangeContract(t *testing.T) {
	repo, diff := bigDiffRepo(t)
	res := Result{Diff: diff, Stopped: "done", LastTest: greenLog}

	got := TierResult(res, repo, 500)

	// Every entry must be callable as-is: its own fields are the drill arguments.
	for _, c := range got.Changes {
		if c.File == "" || c.Line <= 0 || c.EndLine < c.Line {
			t.Errorf("change is not drillable: %+v", c)
		}
	}
	// And the manifest must say so, naming the tool and its exact argument set.
	for _, want := range []string{"show_change", "start_line", "end_line", `kind: "diff"`, `kind: "test_log"`} {
		if !strings.Contains(got.DrillWith, want) {
			t.Errorf("drill_with does not mention %q: %q", want, got.DrillWith)
		}
	}
}

// --- verify tiering (R9 §7) ----------------------------------------------

// Green is one line. The uncapped test log (Result.LastTest, the second leak B1
// found) must not ride along at either tier.
func TestVerifyTierGreenIsOneLine(t *testing.T) {
	repo, diff := pointerRepoCopy(t)
	res := Result{Diff: diff, LastTest: greenLog, Stopped: "done"}

	got := TierResult(res, repo, 100000) // full-diff tier: verify still tiers
	if got.Verify == nil {
		t.Fatal("no verify tier")
	}
	if got.Verify.Status != "pass" {
		t.Errorf("status = %q, want pass", got.Verify.Status)
	}
	if strings.Contains(got.Verify.Summary, "\n") {
		t.Errorf("green verify must be one line, got %q", got.Verify.Summary)
	}
	if !strings.Contains(got.Verify.Summary, "4 passed") {
		t.Errorf("green summary should carry the counts, got %q", got.Verify.Summary)
	}
	body, _ := json.Marshal(got)
	if strings.Contains(string(body), "test session starts") {
		t.Error("payload leaked the raw test log")
	}
}

// Red keeps what review needs to act — which test failed and the assertion — and
// nothing else; the full log is drillable.
func TestVerifyTierRedKeepsFailingTestAndAssertion(t *testing.T) {
	repo, diff := pointerRepoCopy(t)
	res := Result{Diff: diff, LastTest: redLog, Stopped: "done"}

	got := TierResult(res, repo, 100000)
	if got.Verify == nil || got.Verify.Status != "fail" {
		t.Fatalf("verify = %+v, want status fail", got.Verify)
	}
	if len(got.Verify.Failures) != 1 || !strings.Contains(got.Verify.Failures[0], "test_scale_negative") {
		t.Errorf("failures = %v, want the failing test id", got.Verify.Failures)
	}
	if len(got.Verify.Assertions) == 0 || !strings.Contains(got.Verify.Assertions[0], "assert (1, 2) == (-1, -2)") {
		t.Errorf("assertions = %v, want the assertion pointer", got.Verify.Assertions)
	}
	body, _ := json.Marshal(got)
	if strings.Contains(string(body), "test session starts") {
		t.Error("payload leaked the raw test log")
	}
	if got.Verify.LogBytes != len(redLog) {
		t.Errorf("log_bytes = %d, want %d", got.Verify.LogBytes, len(redLog))
	}
}

// The full log has to be fetchable through the same drill contract, so it must be
// parked where show_change(file, start_line, end_line, kind=test_log) can read it.
func TestVerifyTierParksLogForDrill(t *testing.T) {
	repo, diff := loadPointerFixture(t)
	tmp := t.TempDir()
	for _, f := range []string{"point.py", "helpers.py"} {
		b, err := os.ReadFile(filepath.Join(repo, f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmp, f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res := Result{Diff: diff, LastTest: redLog, Stopped: "done"}

	got := TierResult(res, tmp, 100000)
	d := got.Verify.Drill
	if d == nil {
		t.Fatal("red verify must offer a log drill")
	}
	if d.Tool != "show_change" || d.Kind != "test_log" {
		t.Fatalf("log drill = %+v, want show_change/test_log", d)
	}
	parked, err := os.ReadFile(filepath.Join(tmp, d.File))
	if err != nil {
		t.Fatalf("drill target unreadable: %v", err)
	}
	if string(parked) != redLog {
		t.Error("parked log is not the raw log verbatim")
	}
	if lines := strings.Count(redLog, "\n") + 1; d.EndLine != lines || d.StartLine != 1 {
		t.Errorf("log drill range = %d-%d, want 1-%d", d.StartLine, d.EndLine, lines)
	}
}

// A run with no test output at all is a fact review must see, not a silent gap.
func TestVerifyTierMissingWhenNothingRan(t *testing.T) {
	repo, diff := pointerRepoCopy(t)
	got := TierResult(Result{Diff: diff, Stopped: "done"}, repo, 100000)
	if got.Verify == nil || got.Verify.Status != "missing" {
		t.Fatalf("verify = %+v, want status missing", got.Verify)
	}
}

// --- intent (R9 §3) ------------------------------------------------------

// The worker's prose is a claim under a hard ceiling, not a report to be trusted
// at length.
func TestIntentIsCappedNotTrusted(t *testing.T) {
	repo, diff := pointerRepoCopy(t)
	res := Result{Summary: strings.Repeat("the worker explains itself at length. ", 200), Diff: diff, LastTest: greenLog}

	got := TierResult(res, repo, 100000)
	if got.Summary == "" {
		t.Fatal("intent line dropped entirely")
	}
	if len(got.Summary) > maxIntentBytes+len(intentTruncMark) {
		t.Errorf("intent line is %d bytes, ceiling is %d", len(got.Summary), maxIntentBytes)
	}
}

// Per-symbol intent is adopted only for symbols the deterministic layer already
// found: a claim cannot invent a change that is not in the diff.
func TestPerSymbolIntentCannotInventSymbols(t *testing.T) {
	repo, diff := deepDiffRepo(t)
	res := Result{
		Summary: `Done. {"intents":{"Point2D.scale":"added the missing scale method",` +
			`"Nonexistent.ghost":"claimed a change that never happened"}}`,
		Diff: diff, LastTest: greenLog,
	}

	got := TierResult(res, repo, 500) // manifest tier
	if got.Tier != "manifest" {
		t.Fatalf("tier = %q, want manifest", got.Tier)
	}
	if got.Granularity != "symbol" {
		t.Fatalf("granularity = %q, want symbol for a diff concentrated in few symbols", got.Granularity)
	}
	var scaled bool
	for _, c := range got.Changes {
		if c.Symbol == "Point2D.scale" {
			scaled = true
			if !strings.Contains(c.Intent, "missing scale method") {
				t.Errorf("intent for Point2D.scale = %q, want the worker's claim", c.Intent)
			}
		}
		if strings.Contains(c.Intent, "never happened") {
			t.Errorf("invented symbol intent survived on %+v", c)
		}
	}
	if !scaled {
		t.Fatal("Point2D.scale missing from the manifest")
	}
	body, _ := json.Marshal(got)
	if strings.Contains(string(body), "Nonexistent.ghost") {
		t.Error("payload carried an invented symbol")
	}
}

// R11's prose-free lock: with intent suppressed the return artifact is pointers
// and verify facts only — no worker-authored prose anywhere.
func TestIntentSuppressible(t *testing.T) {
	repo, diff := pointerRepoCopy(t)
	t.Setenv("RIG_RETURN_NO_INTENT", "1")
	res := Result{Summary: "a paragraph of worker prose", Diff: diff, LastTest: greenLog}

	got := TierResult(res, repo, 10)
	body, _ := json.Marshal(got)
	if strings.Contains(string(body), "worker prose") {
		t.Error("suppressed intent still reached the payload")
	}
	for _, c := range got.Changes {
		if c.Intent != "" {
			t.Errorf("suppressed intent survived per-symbol: %+v", c)
		}
	}
	if len(got.Changes) == 0 {
		t.Error("suppression must keep the deterministic pointer layer")
	}
}

// --- metadata carried through (B5 binds to Result's shape) ----------------

func TestTierResultCarriesRunMetadata(t *testing.T) {
	repo, diff := pointerRepoCopy(t)
	res := Result{
		Summary: "s", Diff: diff, FilesChanged: []string{"point.py", "helpers.py"},
		Iterations: 7, InputTokens: 1234, OutputTokens: 567, Stopped: "max_iters",
		HitIterationCap: true, Checkpoints: 2, Err: "boom", LastTest: greenLog,
	}
	got := TierResult(res, repo, 10)
	switch {
	case got.Iterations != 7 || got.InputTokens != 1234 || got.OutputTokens != 567:
		t.Errorf("usage counters lost: %+v", got)
	case got.Stopped != "max_iters" || !got.HitIterationCap || got.Checkpoints != 2:
		t.Errorf("stop state lost: %+v", got)
	case got.Err != "boom":
		t.Errorf("error lost: %+v", got)
	case len(got.FilesChanged) != 2:
		t.Errorf("files_changed lost: %+v", got)
	}
}

// The estimator only has to be monotonic and free (there is no tokenizer in this
// binary), which is all the gate needs to be deterministic.
func TestEstimateTokensMonotonic(t *testing.T) {
	prev := -1
	for _, n := range []int{0, 1, 4, 40, 4000, 46000} {
		got := estimateTokens(strings.Repeat("x", n))
		if got < prev {
			t.Fatalf("estimate not monotonic at n=%d: %d after %d", n, got, prev)
		}
		prev = got
	}
	if estimateTokens(strings.Repeat("x", 4000)) < 500 {
		t.Errorf("estimate is implausibly low: %d", estimateTokens(strings.Repeat("x", 4000)))
	}
}

// The threshold is a rig-side knob, in the RIG_WORKER_CTX_LIMIT mould.
func TestReturnThresholdKnob(t *testing.T) {
	if returnThreshold() != defaultReturnThreshold {
		t.Errorf("default threshold = %d, want %d", returnThreshold(), defaultReturnThreshold)
	}
	t.Setenv("RIG_RETURN_THRESHOLD", "321")
	if returnThreshold() != 321 {
		t.Errorf("env override ignored, got %d", returnThreshold())
	}
}

// --- the gate is wired at the tool boundary -------------------------------

// The whole point of putting the gate at mcp.go's implement return is that
// nothing reaches MAIN un-tiered. This drives the real MCP surface end to end:
// a worker run whose test log is enormous must come back summarized, with the
// raw log parked for drilling rather than serialized into the reply.
func TestMCPImplementReturnsTieredPayload(t *testing.T) {
	repo := gitRepo(t)
	noise := strings.Repeat("collecting … a very long pytest log line that MAIN would re-cache forever\n", 400)
	be := fakeBackend(t, []translate.OpenAIResponse{
		toolCallResp("c1", "write_file", `{"path":"app.py","content":"def f():\n    return 2\n"}`),
		toolCallResp("c2", "run_bash", `{"command":"sh runtests.sh"}`),
		finalResp("Changed f() to return 2."),
	})
	defer be.Close()
	if err := os.WriteFile(filepath.Join(repo, "noise.txt"), []byte(noise), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "runtests.sh"),
		[]byte("#!/bin/sh\ncat noise.txt\necho '1 passed in 0.1s'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RIG_WORKER_API_BASE", be.URL)
	t.Setenv("RIG_RETURN_TIERING", "1")
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"implement",` +
		`"arguments":{"task":"make f return 2","repo":` + quote(repo) + `}}}` + "\n")
	var out bytes.Buffer
	if err := Serve(config.Config{WorkerAPIBase: be.URL, WorkerModel: "test"}, in, &out); err != nil {
		t.Fatal(err)
	}

	text := toolResultText(t, out.String())
	if strings.Contains(text, "re-cache forever") {
		t.Errorf("the raw test log reached MAIN through the tool reply (%d bytes)", len(text))
	}
	var payload TieredResult
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("tool reply is not a tiered payload: %v\n%s", err, text)
	}
	if payload.Tier == "" {
		t.Error("reply carries no tier — the gate is not wired in")
	}
	if payload.Verify == nil || payload.Verify.Status != "pass" {
		t.Fatalf("verify tier = %+v, want pass", payload.Verify)
	}
	if payload.Verify.Drill == nil {
		t.Fatal("no drill offered for the parked log")
	}
	parked, err := os.ReadFile(filepath.Join(repo, payload.Verify.Drill.File))
	if err != nil || !strings.Contains(string(parked), "re-cache forever") {
		t.Errorf("parked log missing or wrong: %v", err)
	}
}

// With the switch unset, the implement reply must be the plain C0 Result —
// no tiering vocabulary at all — because that is the surface every winning B5
// number was measured against (map10 P1).
func TestMCPImplementDefaultIsC0Shaped(t *testing.T) {
	repo := gitRepo(t)
	be := fakeBackend(t, []translate.OpenAIResponse{
		toolCallResp("c1", "write_file", `{"path":"app.py","content":"def f():\n    return 2\n"}`),
		finalResp("Changed f() to return 2."),
	})
	defer be.Close()

	t.Setenv("RIG_WORKER_API_BASE", be.URL)
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"implement",` +
		`"arguments":{"task":"make f return 2","repo":` + quote(repo) + `}}}` + "\n")
	var out bytes.Buffer
	if err := Serve(config.Config{WorkerAPIBase: be.URL, WorkerModel: "test"}, in, &out); err != nil {
		t.Fatal(err)
	}

	text := toolResultText(t, out.String())
	var raw map[string]any
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		t.Fatalf("tool reply is not JSON: %v\n%s", err, text)
	}
	for _, k := range []string{"tier", "threshold_tokens", "changes", "verify"} {
		if _, there := raw[k]; there {
			t.Errorf("default reply carries tiering key %q — the gate must be opt-in", k)
		}
	}
	if _, there := raw["diff"]; !there {
		t.Error("default reply is missing the plain diff field")
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// toolResultText unwraps the MCP tools/call envelope of the last response line.
func toolResultText(t *testing.T, raw string) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *rpcError `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &resp); err != nil {
		t.Fatalf("bad rpc response: %v\n%s", err, raw)
	}
	if resp.Error != nil {
		t.Fatalf("rpc error: %+v", resp.Error)
	}
	if len(resp.Result.Content) == 0 {
		t.Fatalf("empty tool result: %s", raw)
	}
	return resp.Result.Content[0].Text
}
