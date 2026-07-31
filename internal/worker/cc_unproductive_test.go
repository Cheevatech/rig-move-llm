package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/config"
)

// ccFakeBinNoEdit is ccFakeBin without the edit: these tests are about the
// round that leaves NOTHING on disk, so the stand-in worker must not write.
func ccFakeBinNoEdit(t *testing.T, dir, stream string) string {
	t.Helper()
	streamFile := filepath.Join(t.TempDir(), "stream.jsonl")
	if err := os.WriteFile(streamFile, []byte(stream), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "fake-claude")
	script := "#!/bin/bash\ncat '" + streamFile + "'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// The unproductive check has to run on the cc engine, not only on the 3-tool
// loop: Implement() returns to implementCC before it reaches its own call, and
// the cc engine is the one the product runs. These tests drive the cc engine.

// ccSpunNoWriteStream: many turns, real spend, no filesystem write at all, and
// nothing left on disk.
const ccSpunNoWriteStream = `{"type":"system","subtype":"init","session_id":"s"}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./..."}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}
{"type":"result","subtype":"success","result":"Looked everywhere, changed nothing.","num_turns":9,"usage":{"input_tokens":400000,"output_tokens":4000},"total_cost_usd":0}
`

// ccWroteThenNothingStream: the worker used a file-writing tool, but no work
// survives in the diff.
const ccWroteThenNothingStream = `{"type":"system","subtype":"init","session_id":"s"}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"w1","name":"Write","input":{"file_path":"/tmp/x","content":"x"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"w1","content":"ok"}]}}
{"type":"result","subtype":"success","result":"Tried and backed it out.","num_turns":2,"usage":{"input_tokens":100,"output_tokens":10},"total_cost_usd":0}
`

// ccQuickConclusionStream: the legitimate "nothing to change here" answer —
// short, cheap, no writes. It must NOT be labelled unproductive.
const ccQuickConclusionStream = `{"type":"system","subtype":"init","session_id":"s"}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"grep -rn foo ."}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"no matches"}]}}
{"type":"result","subtype":"success","result":"Nothing to change here.","num_turns":2,"usage":{"input_tokens":900,"output_tokens":40},"total_cost_usd":0}
`

func TestCCUnproductiveHighSpendNoChange(t *testing.T) {
	repo := ccTestRepo(t)
	bin := ccFakeBinNoEdit(t, t.TempDir(), ccSpunNoWriteStream)
	ccEnv(t, bin)

	res := NewEngine(config.Config{}).Implement(context.Background(), repo, "do the thing", "")

	if res.FilesTouched {
		t.Errorf("files_touched = true, want false (the worker only ran Bash)")
	}
	if !res.Unproductive {
		t.Fatalf("unproductive = false, want true (iterations=%d tokens=%d diff=%q)",
			res.Iterations, res.InputTokens+res.OutputTokens, res.Diff)
	}
	if res.UnproductiveJustification != "high-spend no-change" {
		t.Errorf("justification = %q, want high-spend no-change", res.UnproductiveJustification)
	}
}

func TestCCUnproductiveTouchedFilesNoChange(t *testing.T) {
	repo := ccTestRepo(t)
	bin := ccFakeBinNoEdit(t, t.TempDir(), ccWroteThenNothingStream)
	ccEnv(t, bin)

	res := NewEngine(config.Config{}).Implement(context.Background(), repo, "do the thing", "")

	if !res.FilesTouched {
		t.Fatalf("files_touched = false, want true (the worker called Write)")
	}
	if !res.Unproductive || res.UnproductiveJustification != "touched-files no-change" {
		t.Errorf("unproductive=%v justification=%q, want true/touched-files no-change",
			res.Unproductive, res.UnproductiveJustification)
	}
}

func TestCCQuickNothingToChangeIsNotUnproductive(t *testing.T) {
	repo := ccTestRepo(t)
	bin := ccFakeBinNoEdit(t, t.TempDir(), ccQuickConclusionStream)
	ccEnv(t, bin)

	res := NewEngine(config.Config{}).Implement(context.Background(), repo, "do the thing", "")

	if res.Unproductive {
		t.Errorf("unproductive = true for a short, cheap, no-write round: justification=%q",
			res.UnproductiveJustification)
	}
}

func TestCCWritesFilesExcludesBash(t *testing.T) {
	for _, n := range []string{"Write", "Edit", "MultiEdit", "NotebookEdit"} {
		if !ccWritesFiles(n) {
			t.Errorf("ccWritesFiles(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"Bash", "Read", "Grep", ""} {
		if ccWritesFiles(n) {
			t.Errorf("ccWritesFiles(%q) = true, want false", n)
		}
	}
}
