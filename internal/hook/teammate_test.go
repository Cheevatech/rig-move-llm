package hook

import (
	"strings"
	"testing"
)

// preDecision runs the PreTool hook against a payload and reports whether it was
// denied (non-empty stdout carrying a deny decision).
func preDecision(t *testing.T, s *State, payloadJSON string) (denied bool, out string) {
	t.Helper()
	var b strings.Builder
	if err := s.PreTool(strings.NewReader(payloadJSON), &b); err != nil {
		t.Fatalf("PreTool: %v", err)
	}
	out = b.String()
	return strings.Contains(out, `"permissionDecision":"deny"`), out
}

func TestTeammateRigAgentIDAllowsWorkerTools(t *testing.T) {
	s := &State{Enabled: true} // no log/gate paths needed

	// A terminal-backend teammate's Bash carries no agent_id in the payload; its
	// identity lives in RIG_AGENT_ID (stamped by teammate-exec). It must be allowed.
	t.Setenv("RIG_AGENT_ID", "asmokey-dde64b737cb4fe85")
	teammateBash := `{"tool_name":"Bash","tool_input":{"command":"go test ./..."}}`
	if denied, out := preDecision(t, s, teammateBash); denied {
		t.Fatalf("teammate Bash denied with RIG_AGENT_ID set; out=%q", out)
	}
	teammateEdit := `{"tool_name":"Edit","tool_input":{"file_path":"/repo/main.go"}}`
	if denied, out := preDecision(t, s, teammateEdit); denied {
		t.Fatalf("teammate Edit denied with RIG_AGENT_ID set; out=%q", out)
	}
}

func TestFloor1_LeadWithoutMarkerStillDenied(t *testing.T) {
	s := &State{Enabled: true}

	// Floor #1: the lead (MAIN) process is launched without RIG_AGENT_ID and its
	// payloads have no agent_id. It must remain plan/delegate/review-only — no
	// leak of the teammate marker can flip its posture.
	// (RIG_AGENT_ID intentionally NOT set here.)
	leadBash := `{"tool_name":"Bash","tool_input":{"command":"rm -rf /"}}`
	if denied, _ := preDecision(t, s, leadBash); !denied {
		t.Fatal("MAIN Bash was allowed without a worker marker — floor #1 breached")
	}
	leadEdit := `{"tool_name":"Edit","tool_input":{"file_path":"/repo/main.go"}}`
	if denied, _ := preDecision(t, s, leadEdit); !denied {
		t.Fatal("MAIN Edit was allowed without a worker marker — floor #1 breached")
	}
}

func TestInProcessTeammateAgentIDInPayloadAllowed(t *testing.T) {
	s := &State{Enabled: true}

	// The default in-process backend: the teammate's agent_id is in the payload
	// (no RIG_AGENT_ID env). Must be allowed — this is the zero-config path.
	payload := `{"agent_id":"asmokey-dde64b737cb4fe85","tool_name":"Bash","tool_input":{"command":"echo hi"}}`
	if denied, out := preDecision(t, s, payload); denied {
		t.Fatalf("in-process teammate (payload agent_id) denied; out=%q", out)
	}
}
