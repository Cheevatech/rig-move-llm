// V6 proof-of-flip: red->green evidence read from the worker's own stream
// events, one worker-side retry, UNPROVEN marker — never a bounce to MAIN.
package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/config"
)

func proofEvent(id, cmd string) string {
	return `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"` + id +
		`","name":"Bash","input":{"command":"` + cmd + `"}}]}}` + "\n"
}

func proofResult(id, text string, isErr bool) string {
	e := ""
	if isErr {
		e = `"is_error":true,`
	}
	return `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + id +
		`",` + e + `"content":"` + text + `"}]}}` + "\n"
}

func parseProof(t *testing.T, stream string) *ccProof {
	t.Helper()
	proof := &ccProof{}
	var res Result
	e := NewEngine(config.Config{})
	e.parseCCStream(strings.NewReader(stream), nil, &res, proof, nil)
	return proof
}

func TestCCProofOutcome(t *testing.T) {
	for _, tc := range []struct {
		txt   string
		isErr bool
		want  string
	}{
		{"1 failed, 0 passed in 0.1s", false, "red"},
		{"Traceback (most recent call last): AssertionError", false, "red"},
		{"1 passed in 0.1s", true, "red"}, // non-zero exit wins over the text
		{"1 passed in 0.1s", false, "green"},
		{"5 passed, 2 warnings in 0.3s", false, "green"},
		{"collecting ...", false, ""},
		{"ERROR rig_proof_test.py - ModuleNotFoundError: No module named 'x'", false, "red"},
		{"ImportError: cannot import name 'x'", false, "red"},
		{"FileNotFoundError: [Errno 2] No such file or directory: 'new.py'", false, "red"},
		{"3 passed in 0.1s\nERRORS during collection\nModuleNotFoundError: No module named 'x'", false, "red"}, // mixed run: red wins over " passed"
	} {
		if got := ccProofOutcome(tc.txt, tc.isErr); got != tc.want {
			t.Errorf("ccProofOutcome(%q, %v) = %q, want %q", tc.txt, tc.isErr, got, tc.want)
		}
	}
}

func TestCCProofFlipRedThenGreen(t *testing.T) {
	p := parseProof(t,
		proofEvent("a", "python -m pytest -q rig_proof_test.py")+
			proofResult("a", "1 failed in 0.01s", true)+
			proofEvent("b", "python  -m  pytest -q rig_proof_test.py")+ // spacing normalized away
			proofResult("b", "1 passed in 0.01s", false))
	if !p.Flip {
		t.Fatal("red->green with the same normalized command must count as a flip")
	}
}

func TestCCProofNoRedIsNotAFlip(t *testing.T) {
	p := parseProof(t,
		proofEvent("a", "python -m pytest -q rig_proof_test.py")+
			proofResult("a", "1 passed in 0.01s", false))
	if p.Flip {
		t.Fatal("green without a prior red proves nothing")
	}
}

func TestCCProofRedAfterGreenIsNotAFlip(t *testing.T) {
	p := parseProof(t,
		proofEvent("a", "python -m pytest -q rig_proof_test.py")+
			proofResult("a", "1 passed in 0.01s", false)+
			proofEvent("b", "python -m pytest -q rig_proof_test.py")+
			proofResult("b", "1 failed in 0.01s", true))
	if p.Flip {
		t.Fatal("green then red is the wrong order — no flip")
	}
}

func TestCCProofCommandMismatchIsNotAFlip(t *testing.T) {
	p := parseProof(t,
		proofEvent("a", "python -m pytest -q rig_proof_test.py::test_a")+
			proofResult("a", "1 failed in 0.01s", true)+
			proofEvent("b", "python -m pytest -q rig_proof_test.py::test_b")+
			proofResult("b", "1 passed in 0.01s", false))
	if p.Flip {
		t.Fatal("red and green from different commands must not pair into a flip")
	}
}

func TestCCProofIgnoresNonProofCommands(t *testing.T) {
	p := parseProof(t,
		proofEvent("a", "python -m pytest -q tests/test_x.py")+
			proofResult("a", "1 failed in 0.01s", true)+
			proofEvent("b", "python -m pytest -q tests/test_x.py")+
			proofResult("b", "1 passed in 0.01s", false))
	if p.Flip {
		t.Fatal("only the canonical rig_proof_test.py counts as proof")
	}
}

func TestCCProofCompleteRequiresFileGone(t *testing.T) {
	repo := t.TempDir()
	p := &ccProof{Flip: true}
	if !ccProofComplete(repo, p) {
		t.Fatal("flip + no leftover file should be complete")
	}
	if err := os.WriteFile(filepath.Join(repo, ccProofFile), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ccProofComplete(repo, p) {
		t.Fatal("a leftover proof file must invalidate the proof")
	}
	if ccProofComplete(repo, &ccProof{}) {
		t.Fatal("no flip is never complete")
	}
}

// ccFakeBinCounting is ccFakeBin plus an invocation counter, for retry tests.
func ccFakeBinCounting(t *testing.T, stream string) (bin string, calls func() int) {
	t.Helper()
	outDir := t.TempDir()
	streamFile := filepath.Join(outDir, "stream.jsonl")
	if err := os.WriteFile(streamFile, []byte(stream), 0o644); err != nil {
		t.Fatal(err)
	}
	countFile := filepath.Join(outDir, "calls.txt")
	bin = filepath.Join(outDir, "fake-claude")
	script := "#!/bin/bash\n" +
		"echo call >> '" + countFile + "'\n" +
		"cat '" + streamFile + "'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, func() int {
		b, _ := os.ReadFile(countFile)
		return strings.Count(string(b), "call")
	}
}

func TestCCDoneWithoutProofRetriesOnceThenUnproven(t *testing.T) {
	repo := ccTestRepo(t)
	bin, calls := ccFakeBinCounting(t, ccNoProofStream)
	ccEnv(t, bin)

	res := NewEngine(config.Config{}).Implement(context.Background(), repo, "fix the bug", "")

	if got := calls(); got != 2 {
		t.Errorf("want exactly one worker-side retry (2 spawns), got %d", got)
	}
	if res.Stopped != "done" {
		t.Errorf("stopped=%q — UNPROVEN must label, not block", res.Stopped)
	}
	if !strings.Contains(res.Summary, "UNPROVEN") {
		t.Errorf("summary lacks the UNPROVEN marker: %q", res.Summary)
	}
	// The retry doubles the meter: both spawns are paid rounds on the worker leg.
	if res.InputTokens != 222 || res.OutputTokens != 44 || res.Iterations != 8 {
		t.Errorf("retry usage not accumulated: in=%d out=%d iters=%d",
			res.InputTokens, res.OutputTokens, res.Iterations)
	}
}

func TestCCProofHappyPathNoRetryNoMarker(t *testing.T) {
	repo := ccTestRepo(t)
	bin, calls := ccFakeBinCounting(t, ccHappyStream)
	ccEnv(t, bin)

	res := NewEngine(config.Config{}).Implement(context.Background(), repo, "fix the bug", "")

	if got := calls(); got != 1 {
		t.Errorf("proof-complete run must not retry, got %d spawns", got)
	}
	if strings.Contains(res.Summary, "UNPROVEN") {
		t.Errorf("proof-complete run must not be labelled UNPROVEN: %q", res.Summary)
	}
	if res.Stopped != "done" || res.Err != "" {
		t.Errorf("stopped=%q err=%q", res.Stopped, res.Err)
	}
}

func TestCCLeftoverProofFileIsRemovedAndUnproven(t *testing.T) {
	repo := ccTestRepo(t)
	// Proof events are present, but the fake worker leaves rig_proof_test.py on
	// disk every run — the engine must refuse the proof AND clean the file up.
	bin, calls := ccFakeBinCounting(t, ccHappyStream)
	script, _ := os.ReadFile(bin)
	withLitter := strings.Replace(string(script), "echo call",
		"touch rig_proof_test.py\necho call", 1)
	if err := os.WriteFile(bin, []byte(withLitter), 0o755); err != nil {
		t.Fatal(err)
	}
	ccEnv(t, bin)

	res := NewEngine(config.Config{}).Implement(context.Background(), repo, "fix the bug", "")

	if got := calls(); got != 2 {
		t.Errorf("leftover file should trigger the one retry, got %d spawns", got)
	}
	if !strings.Contains(res.Summary, "UNPROVEN") {
		t.Errorf("leftover proof file must yield UNPROVEN: %q", res.Summary)
	}
	if _, err := os.Stat(filepath.Join(repo, ccProofFile)); !os.IsNotExist(err) {
		t.Error("engine must remove the leftover proof file from the tree")
	}
	if strings.Contains(res.Diff, ccProofFile) {
		t.Errorf("proof file leaked into the diff: %v", res.FilesChanged)
	}
}

func TestCCProofGreenfieldFlip(t *testing.T) {
	cmd := "./.venv/bin/python -m pytest -q rig_proof_test.py"
	p := parseProof(t,
		proofEvent("a", cmd)+
			proofResult("a", "ERROR rig_proof_test.py - ModuleNotFoundError: No module named 'newmod'", true)+
			proofEvent("b", cmd)+
			proofResult("b", "1 passed in 0.02s", false))
	if !p.Flip {
		t.Fatal("greenfield task must be able to prove red->green: the missing-module failure IS the red state")
	}
	repo := t.TempDir() // no proof file on disk
	if !ccProofComplete(repo, p) {
		t.Fatal("greenfield flip with no leftover proof file should be complete")
	}
}

func TestCCProofGreenfieldRedWithoutIsError(t *testing.T) {
	cmd := "./.venv/bin/python -m pytest -q rig_proof_test.py"
	p := parseProof(t,
		proofEvent("a", cmd)+
			proofResult("a", "ERROR rig_proof_test.py - ImportError: cannot import name 'thing'", false)+
			proofEvent("b", cmd)+
			proofResult("b", "1 passed in 0.02s", false))
	if !p.Flip {
		t.Fatal("textual greenfield patterns alone must be enough for CC versions that do not set is_error")
	}
}

func TestCCProofNeverPairsAcrossSessions(t *testing.T) {
	cmd := "./.venv/bin/python -m pytest -q rig_proof_test.py"
	// Session one: only the red
	p1 := parseProof(t,
		proofEvent("a", cmd)+
			proofResult("a", "ERROR rig_proof_test.py - ModuleNotFoundError: No module named 'newmod'", true))
	if p1.Flip {
		t.Fatal("lone red must not produce a flip")
	}
	// Session two: only the green
	p2 := parseProof(t,
		proofEvent("b", cmd)+
			proofResult("b", "1 passed in 0.02s", false))
	if p2.Flip {
		t.Fatal("a stale red from an earlier spawn must never legitimize a later lone green")
	}
}

func TestCCProofContractText(t *testing.T) {
	// ccSystemPrompt must contain the greenfield ordering instructions
	for _, substr := range []string{"BEFORE your first edit", "GREENFIELD", "ModuleNotFoundError"} {
		if !strings.Contains(ccSystemPrompt, substr) {
			t.Errorf("ccSystemPrompt missing required substring: %q", substr)
		}
	}
	// The retry instruction must contain stash commands, but NOT destructive reset patterns
	retryInstr := ccProofRetryInstrFor(ccStashTag())
	for _, substr := range []string{"git stash push -u", "git stash pop"} {
		if !strings.Contains(retryInstr, substr) {
			t.Errorf("retry instruction missing required substring: %q", substr)
		}
	}
	// "git checkout -- ." appears only in a parenthetical contrast ("a plain `git checkout -- .` cannot
	// revert an untracked new file"), not as the actual instruction — so we don't assert its absence.
	// What matters is it does NOT carry the other destructive pattern:
	if strings.Contains(retryInstr, "git diff > /tmp/rig_fix.patch") {
		t.Error("retry instruction must NOT contain: \"git diff > /tmp/rig_fix.patch\"")
	}
}
