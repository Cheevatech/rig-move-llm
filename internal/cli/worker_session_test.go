package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/gatestate"
)

// #27, as measured: the cc worker runs as its own `claude -p` session in the same
// project, so it fires the lifecycle hooks too. cmdUserPrompt cleared MAIN's
// per-turn state unconditionally, so every successful delegation reset the
// counter the delegation was supposed to be counted in — the #18 budget could
// only ever fire when two dispatches landed between two worker sessions.
func TestUserPromptHookLeavesMainStateAloneInAWorkerSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "state")
	t.Setenv("RIG_STATE_DIR", dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// MAIN's turn is under way: it has delegated once and a repair window is open.
	if n := gatestate.BumpRound(dir); n != 1 {
		t.Fatalf("BumpRound = %d, want 1", n)
	}
	if err := gatestate.WriteTriage(dir, gatestate.Triage{Decision: "delegate", At: time.Now()}); err != nil {
		t.Fatal(err)
	}

	// The worker's session starts and fires the hook.
	t.Setenv("RIG_AGENT_ID", "cc-worker")
	var out bytes.Buffer
	if rc := cmdUserPrompt(strings.NewReader("{}"), &out); rc != 0 {
		t.Fatalf("hook rc=%d", rc)
	}

	if r, ok := gatestate.ReadRounds(dir); !ok || r.Count != 1 {
		t.Errorf("the worker reset MAIN's delegation counter: %+v (ok=%v)", r, ok)
	}
	if _, ok := gatestate.ReadTriage(dir); !ok {
		t.Error("the worker cleared MAIN's triage decision")
	}
}

// MAIN's own invocation must still reset the turn — that is the whole point of
// the hook, and the human's next message is the delegation budget's escape hatch.
func TestUserPromptHookStillClearsForMain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "state")
	t.Setenv("RIG_STATE_DIR", dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gatestate.BumpRound(dir)
	t.Setenv("RIG_AGENT_ID", "")

	var out bytes.Buffer
	if rc := cmdUserPrompt(strings.NewReader("{}"), &out); rc != 0 {
		t.Fatalf("hook rc=%d", rc)
	}

	if r, ok := gatestate.ReadRounds(dir); ok || r.Count != 0 {
		t.Errorf("MAIN's new message did not reset the turn: %+v (ok=%v)", r, ok)
	}
}

// A worker session must not reshape the user's workspace either: materializing a
// project scope is a MAIN-leg decision.
func TestSessionStartCreatesNoScopeInAWorkerSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	proj := filepath.Join(home, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", proj)
	// A global scope exists, so the hook would normally follow the user in here.
	global := filepath.Join(home, ".rig-move-llm")
	if err := os.MkdirAll(global, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(global, "config.env"), []byte("ENABLED=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RIG_AGENT_ID", "cc-worker")
	var out bytes.Buffer
	if rc := cmdSessionStart(strings.NewReader("{}"), &out); rc != 0 {
		t.Fatalf("hook rc=%d", rc)
	}

	if _, err := os.Stat(filepath.Join(proj, ".rig-move-llm")); err == nil {
		t.Error("a worker session materialized a project scope")
	}
}
