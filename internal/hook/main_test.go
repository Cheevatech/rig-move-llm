package hook

import (
	"os"
	"testing"
)

// TestMain isolates the test suite from the ambient environment.
//
// The suite may be run from inside a rig worker session where RIG_AGENT_ID is
// stamped (e.g. "cc-worker"). The hook's effectiveAgentID() falls back to
// os.Getenv("RIG_AGENT_ID") when the payload has no agent_id. If the ambient
// value leaks in, every test that simulates MAIN (no agent_id, expecting a deny)
// sees a non-empty agent_id and gets an allow — the entire guard is invisible.
//
// Tests that deliberately want the marker (e.g. TestTeammateRigAgentIDAllowsWorkerTools)
// set it with t.Setenv, which still works and restores afterwards.
func TestMain(m *testing.M) {
	os.Unsetenv("RIG_AGENT_ID")
	os.Exit(m.Run())
}
