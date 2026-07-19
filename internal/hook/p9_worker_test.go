package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain_TaskDeniedSteersToWorker asserts the Option-2 pivot: MAIN can no
// longer spawn a subagent — it must call the worker MCP tool instead.
func TestMain_TaskDeniedSteersToWorker(t *testing.T) {
	s := &State{}
	// Even a well-formed synchronous Task (previously allowed) is now denied for MAIN.
	task := `{"tool_name":"Task","tool_input":{"run_in_background":false}}`
	denied, out := preDecision(t, s, task)
	if !denied {
		t.Fatalf("MAIN Task should be denied under Option 2; out=%q", out)
	}
	if !strings.Contains(out, "mcp__worker__implement") {
		t.Errorf("deny reason should steer to the worker tool; out=%q", out)
	}
}

// TestMain_WorkerToolAllowed_FreezesGate asserts MAIN may call the worker tool,
// and that doing so is the freeze point for any authored .gate contract.
func TestMain_WorkerToolAllowed_FreezesGate(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	gate := filepath.Join(repo, ".gate")
	if err := os.MkdirAll(gate, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gate, "repro.py"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &State{GatePaths: filepath.Join(dir, "gate_paths"), LogPath: filepath.Join(dir, "log")}
	s.appendGatePath(gate)

	call := `{"tool_name":"mcp__worker__implement","tool_input":{}}`
	if denied, out := preDecision(t, s, call); denied {
		t.Fatalf("MAIN worker tool call denied; out=%q", out)
	}
	if !isDir(filepath.Join(repo, ".gate.frozen")) {
		t.Error("worker tool call did not freeze the .gate contract")
	}
}

// TestMain_OtherMCPStillDenied confirms only the worker server is opened up;
// arbitrary MCP servers remain denied for MAIN (the shared-MCP tier is unchanged).
func TestMain_OtherMCPStillDenied(t *testing.T) {
	s := &State{}
	if denied, _ := preDecision(t, s, `{"tool_name":"mcp__random__do"}`); !denied {
		t.Error("a non-allowlisted MCP tool should stay denied for MAIN")
	}
}

// TestPostTool_GatesOnWorkerReturn confirms the deterministic gate fires when the
// worker MCP tool returns (not only on Task/Agent).
func TestPostTool_GatesOnWorkerReturn(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".gate.frozen"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A trivial runner that always reports GREEN.
	runner := filepath.Join(dir, "run_gate.sh")
	if err := os.WriteFile(runner, []byte("#!/usr/bin/env bash\necho 'GREEN|contract passed'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &State{
		GatePaths:  filepath.Join(dir, "gate_paths"),
		LogPath:    filepath.Join(dir, "log"),
		GateRunner: runner,
	}
	s.appendGatePath(filepath.Join(repo, ".gate"))

	var b strings.Builder
	payload := `{"tool_name":"mcp__worker__implement","tool_input":{}}`
	if err := s.PostTool(strings.NewReader(payload), &b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "GATE VERDICT: GREEN") {
		t.Errorf("PostTool did not gate the worker return; out=%q", b.String())
	}
}
