package hook

import "testing"

// TestDisabledPassesThrough asserts the master switch: with Enabled=false the
// PreTool hook denies nothing (Claude Code runs normally). This is the mode a
// user gets by skipping the worker in setup.
func TestDisabledPassesThrough(t *testing.T) {
	s := &State{Enabled: false}
	for _, payload := range []string{
		`{"tool_name":"Bash","tool_input":{}}`,
		`{"tool_name":"Edit","tool_input":{"file_path":"/x/main.go"}}`,
		`{"tool_name":"Task","tool_input":{}}`,
		`{"tool_name":"mcp__random__do"}`,
	} {
		if denied, out := preDecision(t, s, payload); denied {
			t.Errorf("disabled hook must pass through %s; got deny out=%q", payload, out)
		}
	}
}
