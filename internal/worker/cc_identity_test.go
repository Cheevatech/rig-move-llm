package worker

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/hook"
)

// The seam #24 broke runs across two packages: the cc engine stamps the child
// environment, and the hook reads that stamp to tell the free worker leg from
// the paid MAIN leg. Each side passing its own unit tests is exactly the state
// that shipped the bug, so this asserts the handshake itself: whatever
// ccChildEnv produces must be enough for the hook to let the worker work.
func TestChildEnvStampSatisfiesTheHook(t *testing.T) {
	var stamp string
	for _, kv := range ccChildEnv("http://worker-leg") {
		if v, ok := strings.CutPrefix(kv, "RIG_AGENT_ID="); ok {
			stamp = v
		}
	}
	if stamp == "" {
		t.Fatal("the child environment carries no RIG_AGENT_ID — the hook will read the worker as MAIN and deny its tools")
	}
	t.Setenv("RIG_AGENT_ID", stamp)

	s := &hook.State{Enabled: true}
	for _, call := range []string{
		`{"tool_name":"Bash","tool_input":{"command":"pytest -q"}}`,
		`{"tool_name":"Edit","tool_input":{"file_path":"/repo/app.py","new_string":"fix"}}`,
	} {
		var out bytes.Buffer
		if err := s.PreTool(strings.NewReader(call), &out); err != nil {
			t.Fatalf("PreTool: %v", err)
		}
		if strings.Contains(out.String(), `"deny"`) {
			t.Errorf("the stamped worker was denied %s:\n%s", call, out.String())
		}
	}
}
