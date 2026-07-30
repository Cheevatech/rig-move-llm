package cli

import (
	"os"
	"testing"
)

// TestMain isolates the test suite from the ambient environment.
//
// The suite may be run from inside a rig worker session where RIG_AGENT_ID is
// stamped (e.g. "cc-worker"). The inWorkerSession() helper in user_prompt.go
// and the effectiveAgentID() fallback in hook.go both read
// os.Getenv("RIG_AGENT_ID"). If the ambient value leaks in, every test that
// simulates MAIN (no agent_id) sees "this is a worker session" and the
// session-start and user-prompt hooks return early without doing their work.
//
// Tests that deliberately want the marker set it with t.Setenv, which still
// works and restores afterwards.
func TestMain(m *testing.M) {
	os.Unsetenv("RIG_AGENT_ID")
	os.Exit(m.Run())
}
