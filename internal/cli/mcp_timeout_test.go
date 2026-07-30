package cli

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/worker"
)

// #33: without an explicit per-call timeout, a stdio server that answers only at
// the END of a long round — which is what the worker does — is aborted by Claude
// Code's 30-minute idle window while it is still working. The per-server timeout
// is both the honest wall and the floor on that idle window.
func TestWorkerMCPEntryCarriesAClientCallTimeout(t *testing.T) {
	for _, npx := range []bool{false, true} {
		entry := workerMCPEntry(npx)

		raw, ok := entry["timeout"]
		if !ok {
			t.Fatalf("npx=%v: worker entry has no timeout — the client falls back to the ~28h default and the 30m stdio idle abort", npx)
		}
		ms, ok := raw.(int)
		if !ok {
			t.Fatalf("npx=%v: timeout is %T, want int milliseconds", npx, raw)
		}
		if got, want := time.Duration(ms)*time.Millisecond, worker.ClientCallTimeout(); got != want {
			t.Errorf("npx=%v: timeout = %v, want %v (the value rig's own guards are ordered against)", npx, got, want)
		}
		// Below 1000ms Claude Code ignores the field entirely.
		if ms < 1000 {
			t.Errorf("npx=%v: timeout %dms is below the 1000ms floor and would be ignored", npx, ms)
		}
	}
}

// The whole point of the ordering is that rig kills its own round first and gets
// to return a diagnosis plus the partial diff (#31), instead of MAIN seeing a
// bare client-side abort (#18).
func TestClientCallTimeoutSitsAboveTheEnginesOwnWall(t *testing.T) {
	t.Setenv("RIG_WORKER_RUN_TIMEOUT", "")

	if client := worker.ClientCallTimeout(); client <= 3000*time.Second {
		t.Errorf("client timeout %v must exceed the engine wall guard (3000s default)", client)
	}
}

// The generated file must parse as the config Claude Code reads, with the
// timeout on the worker server itself.
func TestRenderMCPEmitsTheTimeout(t *testing.T) {
	var got struct {
		MCPServers map[string]struct {
			Command string `json:"command"`
			Timeout int    `json:"timeout"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(renderMCP(false)), &got); err != nil {
		t.Fatalf("generated .mcp.json does not parse: %v", err)
	}
	w, ok := got.MCPServers["worker"]
	if !ok {
		t.Fatal("no worker server in the generated .mcp.json")
	}
	if w.Timeout != int(worker.ClientCallTimeout()/time.Millisecond) {
		t.Errorf("worker timeout = %d ms, want %d ms", w.Timeout, int(worker.ClientCallTimeout()/time.Millisecond))
	}
}
