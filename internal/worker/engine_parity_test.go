package worker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/internal/gatestate"
	"github.com/Cheevatech/rig-move-llm/pkg/translate"
)

// The Result contract is one contract served by two engines, and nothing
// structural forces a feature onto both: #48 shipped with its check wired on
// the 3-tool loop only, dead on the engine the product actually runs, and every
// test was green. These tests close that seam the only way that generalizes —
// by driving BOTH engines through the same scenarios and comparing the
// contract-level facts of their Results directly. A feature added to one engine
// fails here until it exists on the other.

// parityFacts is the projection of a Result that must not depend on which
// engine produced it: the machine-readable facts a caller keys decisions on.
type parityFacts struct {
	Stopped       string
	HasDiff       bool
	GateSource    string
	GateVerdict   string
	WorkerVerdict string
	Unproductive  bool
	Justification string
}

func factsOf(res Result) parityFacts {
	return parityFacts{
		Stopped:       res.Stopped,
		HasDiff:       strings.TrimSpace(res.Diff) != "",
		GateSource:    res.GateSource,
		GateVerdict:   res.GateVerdict,
		WorkerVerdict: res.WorkerVerdict,
		Unproductive:  res.Unproductive,
		Justification: res.UnproductiveJustification,
	}
}

// loopGoRepo is ccTestGoRepo's shape driven through the loop engine's fake
// backend instead of the cc fake bin: same repo layout, different engine.
func runLoopScenario(t *testing.T, repo string, script []translate.OpenAIResponse) Result {
	t.Helper()
	be := fakeBackend(t, script)
	defer be.Close()
	// The engine picks the loop when RIG_WORKER_ENGINE is any non-"cc" value.
	t.Setenv("RIG_WORKER_ENGINE", "loop")
	cfg := config.Config{WorkerAPIBase: be.URL, WorkerModel: "test"}
	return NewEngine(cfg).Implement(context.Background(), repo, "the task", "")
}

func runCCScenario(t *testing.T, repo, stream string, editsRepo bool) Result {
	t.Helper()
	var bin string
	if editsRepo {
		bin, _ = ccFakeBin(t, t.TempDir(), stream)
	} else {
		bin = ccFakeBinNoEdit(t, t.TempDir(), stream)
	}
	ccEnv(t, bin)
	return NewEngine(config.Config{}).Implement(context.Background(), repo, "the task", "")
}

// bigUsageTool is a scripted loop response with enough token spend that a
// no-change round crosses the unproductive thresholds.
func bigUsageTool(id string) translate.OpenAIResponse {
	r := toolCallResp(id, "read_file", `{"path":"file.txt"}`)
	r.Usage = &translate.OpenAIUsage{PromptTokens: 4000, CompletionTokens: 100}
	return r
}

// Scenario 1: a round that edits a file in a Go-shaped repo. Both engines must
// return the engine's own gate measurement in the same fields.
func TestParity_DiffBearingRoundIsEngineGated(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("needs a shell for the fake cc bin")
	}

	loopRes := runLoopScenario(t, ccTestGoRepo(t), []translate.OpenAIResponse{
		toolCallResp("c1", "write_file", `{"path":"file.txt","content":"edited\n"}`),
		finalResp("Edited the file."),
	})
	ccRes := runCCScenario(t, ccTestGoRepo(t), ccHappyStream, true)

	for name, res := range map[string]Result{"loop": loopRes, "cc": ccRes} {
		if res.Stopped != "done" {
			t.Fatalf("%s: stopped=%q err=%q", name, res.Stopped, res.Err)
		}
	}

	lf, cf := factsOf(loopRes), factsOf(ccRes)
	if !lf.HasDiff || !cf.HasDiff {
		t.Fatalf("both rounds must carry a diff: loop=%v cc=%v", lf.HasDiff, cf.HasDiff)
	}
	if lf.GateSource != "engine" || cf.GateSource != "engine" {
		t.Errorf("gate_source parity: loop=%q cc=%q, want engine on both", lf.GateSource, cf.GateSource)
	}
	if lf.GateVerdict != "pass" || cf.GateVerdict != "pass" {
		t.Errorf("gate_verdict parity: loop=%q cc=%q, want pass on both", lf.GateVerdict, cf.GateVerdict)
	}
	if lf.WorkerVerdict == "" || cf.WorkerVerdict == "" {
		t.Errorf("worker_verdict must be derived on both: loop=%q cc=%q", lf.WorkerVerdict, cf.WorkerVerdict)
	}
	if lf.Unproductive || cf.Unproductive {
		t.Errorf("a diff-bearing round is never unproductive: loop=%v cc=%v", lf.Unproductive, cf.Unproductive)
	}
}

// Scenario 2: the quick, cheap "nothing to change here" answer. Neither engine
// may label it unproductive, and neither has anything to gate.
func TestParity_QuickNothingToChange(t *testing.T) {
	loopRes := runLoopScenario(t, ccTestGoRepo(t), []translate.OpenAIResponse{
		finalResp("Nothing to change here."),
	})
	ccRes := runCCScenario(t, ccTestGoRepo(t), ccQuickConclusionStream, false)

	lf, cf := factsOf(loopRes), factsOf(ccRes)
	want := parityFacts{Stopped: "done", WorkerVerdict: ""}
	for name, f := range map[string]parityFacts{"loop": lf, "cc": cf} {
		if f != want {
			t.Errorf("%s: facts = %+v, want %+v", name, f, want)
		}
	}
}

// Scenario 3: a round that spins — real iteration and token spend, no writes,
// nothing on disk. Both engines must flag it with the same justification.
func TestParity_HighSpendNoChangeIsUnproductive(t *testing.T) {
	script := []translate.OpenAIResponse{
		bigUsageTool("c1"), bigUsageTool("c2"), bigUsageTool("c3"), bigUsageTool("c4"),
		finalResp("Looked everywhere, changed nothing."),
	}
	loopRes := runLoopScenario(t, ccTestGoRepo(t), script)
	ccRes := runCCScenario(t, ccTestGoRepo(t), ccSpunNoWriteStream, false)

	lf, cf := factsOf(loopRes), factsOf(ccRes)
	for name, f := range map[string]parityFacts{"loop": lf, "cc": cf} {
		if f.Stopped != "done" || f.HasDiff {
			t.Fatalf("%s: scenario broken: %+v", name, f)
		}
		if !f.Unproductive || f.Justification != "high-spend no-change" {
			t.Errorf("%s: unproductive=%v justification=%q, want true/high-spend no-change", name, f.Unproductive, f.Justification)
		}
	}
}

// Guard on the guard: the loop scenario above must actually cross the spend
// thresholds, or scenario 3 would pass vacuously if the thresholds moved.
func TestParity_LoopHighSpendScenarioCrossesThresholds(t *testing.T) {
	res := runLoopScenario(t, ccTestGoRepo(t), []translate.OpenAIResponse{
		bigUsageTool("c1"), bigUsageTool("c2"), bigUsageTool("c3"), bigUsageTool("c4"),
		finalResp("Looked everywhere, changed nothing."),
	})
	if res.Iterations < unproductiveIterThreshold {
		t.Fatalf("iterations=%d below threshold %d", res.Iterations, unproductiveIterThreshold)
	}
	if spend := res.InputTokens + res.OutputTokens; spend <= unproductiveSpendThreshold {
		t.Fatalf("spend=%d not above threshold %d", spend, unproductiveSpendThreshold)
	}
}

// The repo helper must stay a real Go shape, or every gate expectation above
// silently degrades to the NOT RUN branch.
func TestParity_RepoHelperIsGoShaped(t *testing.T) {
	repo := ccTestGoRepo(t)
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
		t.Fatalf("ccTestGoRepo lost its go.mod: %v", err)
	}
}

// End-to-end over the MCP seam: an unproductive round arriving through the
// worker server must refund the delegation slot in the SAME state dir the
// hook's budget reads. This is the wiring #48 left as a known gap.
func TestMCPImplementRefundsUnproductiveRound(t *testing.T) {
	repo := ccTestGoRepo(t)
	be := fakeBackend(t, []translate.OpenAIResponse{
		bigUsageTool("c1"), bigUsageTool("c2"), bigUsageTool("c3"), bigUsageTool("c4"),
		finalResp("Looked everywhere, changed nothing."),
	})
	defer be.Close()

	state := t.TempDir()
	t.Setenv("RIG_STATE_DIR", state)
	t.Setenv("RIG_WORKER_ENGINE", "loop")
	// The hook charged this delegation before the tool ran.
	gatestate.BumpRound(state)

	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"implement",` +
		`"arguments":{"task":"the task","repo":` + quote(repo) + `}}}` + "\n")
	var out bytes.Buffer
	if err := Serve(config.Config{WorkerAPIBase: be.URL, WorkerModel: "test"}, in, &out); err != nil {
		t.Fatal(err)
	}

	r, ok := gatestate.ReadRounds(state)
	if !ok || r.Refunded != 1 || r.Effective() != 0 {
		t.Fatalf("unproductive round must refund its slot: rounds=%+v ok=%v", r, ok)
	}

	// And a productive round must NOT refund: same server, a round with a diff.
	be2 := fakeBackend(t, []translate.OpenAIResponse{
		toolCallResp("c1", "write_file", `{"path":"file.txt","content":"edited\n"}`),
		finalResp("Edited the file."),
	})
	defer be2.Close()
	gatestate.BumpRound(state)
	in2 := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"implement",` +
		`"arguments":{"task":"the task","repo":` + quote(repo) + `}}}` + "\n")
	var out2 bytes.Buffer
	if err := Serve(config.Config{WorkerAPIBase: be2.URL, WorkerModel: "test"}, in2, &out2); err != nil {
		t.Fatal(err)
	}
	r, _ = gatestate.ReadRounds(state)
	if r.Refunded != 1 {
		t.Fatalf("a productive round must not refund: rounds=%+v", r)
	}
}
