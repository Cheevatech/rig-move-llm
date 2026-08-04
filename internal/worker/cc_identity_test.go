package worker

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/internal/hook"
)

// workerCalls are tool calls only a worker may make: if the hook reads the
// caller as MAIN it denies all of them (the #24 shape — a round that burns its
// whole wall with iters=0 and an empty diff).
var workerCalls = []string{
	`{"tool_name":"Bash","tool_input":{"command":"pytest -q"}}`,
	`{"tool_name":"Edit","tool_input":{"file_path":"/repo/app.py","new_string":"fix"}}`,
}

// The seam #24 broke runs across two packages: the cc engine identifies its
// subprocess, and the hook reads that identity to tell the free worker leg from
// the paid MAIN leg. Each side passing its own unit tests is exactly the state
// that shipped the bug, so this asserts the handshake itself — for the identity
// that does not leak (a registered session id) and for the env stamp that is
// still the fallback when registration fails.
func TestSessionIDSatisfiesTheHook(t *testing.T) {
	dir := t.TempDir()
	e := &Engine{cfg: config.Config{DataDir: dir}}
	t.Setenv("RIG_STATE_DIR", dir)

	ws, ok := e.newWorkerSession()
	if !ok {
		t.Fatal("could not register a worker session — the engine would fall back to the leaking env stamp")
	}
	defer ws.close()

	s := &hook.State{Enabled: true, StateDir: dir}
	for _, call := range withSession(t, workerCalls, ws.id) {
		var out bytes.Buffer
		if err := s.PreTool(strings.NewReader(call), &out); err != nil {
			t.Fatalf("PreTool: %v", err)
		}
		if strings.Contains(out.String(), `"deny"`) {
			t.Errorf("the registered worker session was denied %s:\n%s", call, out.String())
		}
	}

	// The identity must not outlive the round: once the engine closes the
	// session, that id is MAIN again.
	ws.close()
	for _, call := range withSession(t, workerCalls[:1], ws.id) {
		var out bytes.Buffer
		if err := s.PreTool(strings.NewReader(call), &out); err != nil {
			t.Fatalf("PreTool: %v", err)
		}
		if !strings.Contains(out.String(), `"deny"`) {
			t.Errorf("a closed session still passes as a worker — a later MAIN session handed the same id inherits worker privileges:\n%s", out.String())
		}
	}
}

// #42: the worker's identity must not reach the processes the worker launches.
// A registered session gets no RIG_AGENT_ID in its child environment, so the
// user's own `go test` — which inherits that environment — sees no rig identity.
func TestRegisteredSessionDoesNotStampTheEnvironment(t *testing.T) {
	env := ccChildEnv("http://worker-leg", false)
	for _, kv := range env {
		if strings.HasPrefix(kv, "RIG_AGENT_ID=") {
			t.Fatalf("a registered worker session still exports %s — it inherits into every process the worker starts (#42)", kv)
		}
	}
}

// The fallback is the #24 insurance: when nothing could be registered the round
// must still run, even at the price of the leak.
func TestFallbackStampSatisfiesTheHook(t *testing.T) {
	var stamp string
	for _, kv := range ccChildEnv("http://worker-leg", true) {
		if v, ok := strings.CutPrefix(kv, "RIG_AGENT_ID="); ok {
			stamp = v
		}
	}
	if stamp == "" {
		t.Fatal("the fallback child environment carries no RIG_AGENT_ID — the hook will read the worker as MAIN and deny its tools")
	}
	t.Setenv("RIG_AGENT_ID", stamp)

	s := &hook.State{Enabled: true}
	for _, call := range workerCalls {
		var out bytes.Buffer
		if err := s.PreTool(strings.NewReader(call), &out); err != nil {
			t.Fatalf("PreTool: %v", err)
		}
		if strings.Contains(out.String(), `"deny"`) {
			t.Errorf("the stamped worker was denied %s:\n%s", call, out.String())
		}
	}
}

// A session id read off a payload is untrusted input that becomes a file path.
func TestRegistryRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &hook.State{Enabled: true, StateDir: filepath.Join(dir, "worker_sessions")}
	call := `{"tool_name":"Bash","session_id":"../outside","tool_input":{"command":"pytest -q"}}`
	var out bytes.Buffer
	if err := s.PreTool(strings.NewReader(call), &out); err != nil {
		t.Fatalf("PreTool: %v", err)
	}
	if !strings.Contains(out.String(), `"deny"`) {
		t.Errorf("a traversing session id was accepted as a worker identity:\n%s", out.String())
	}
}

// withSession stamps a session id onto each call payload.
func withSession(t *testing.T, calls []string, id string) []string {
	t.Helper()
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		var m map[string]any
		if err := json.Unmarshal([]byte(c), &m); err != nil {
			t.Fatalf("payload %s: %v", c, err)
		}
		m["session_id"] = id
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	return out
}
