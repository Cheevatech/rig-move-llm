package hook

import (
	"strings"
	"testing"
)

// #24, as measured: the cc engine runs the worker as its own `claude -p`
// session, so its hook payloads carry no agent_id. With RIG_AGENT_ID stripped
// from the child environment too, the hook read the free worker leg as the paid
// MAIN leg and denied every tool it needs — a round burned the full wall guard
// with iters=0, an empty diff, and rig's own deny text sitting in its last_test.
//
// The stamp is what makes the worker not-MAIN by construction.
func TestStampedWorkerMayActuallyWork(t *testing.T) {
	t.Setenv("RIG_AGENT_ID", "cc-worker")
	s := &State{Enabled: true, MaxRounds: 1}

	for _, call := range []string{
		`{"tool_name":"Bash","tool_input":{"command":"pytest -q"}}`,
		`{"tool_name":"Edit","tool_input":{"file_path":"/repo/app.py","new_string":"fix"}}`,
		`{"tool_name":"Write","tool_input":{"file_path":"/repo/new.py","content":"x"}}`,
		`{"tool_name":"Task","tool_input":{}}`,
	} {
		if denied, out := preDecision(t, s, call); denied {
			t.Errorf("the worker was denied %s — it cannot do its job:\n%s", call, out)
		}
	}
}

// The same calls from the unstamped paid leg stay denied: the fix must not open
// the gate for MAIN.
func TestUnstampedMainStaysDenied(t *testing.T) {
	s := &State{Enabled: true}
	for _, call := range []string{
		`{"tool_name":"Bash","tool_input":{"command":"pytest -q"}}`,
		`{"tool_name":"Edit","tool_input":{"file_path":"/repo/app.py","new_string":"fix"}}`,
	} {
		denied, out := preDecision(t, s, call)
		if !denied {
			t.Errorf("MAIN must stay denied for %s", call)
		}
		if denied && !strings.Contains(out, "plan/delegate/review only") {
			t.Errorf("unexpected deny reason for MAIN: %s", out)
		}
	}
}

// The worker's own delegate-shaped calls must not consume MAIN's per-turn
// delegation budget (#18): the budget meters the paid leg's decisions, and a
// stamped worker is not that leg.
func TestWorkerDoesNotSpendTheDelegationBudget(t *testing.T) {
	s := budgetState(t, 1)
	t.Setenv("RIG_AGENT_ID", "cc-worker")

	for i := 0; i < 5; i++ {
		if denied, out := preDecision(t, s, implementCall); denied {
			t.Fatalf("worker call %d denied; out=%q", i, out)
		}
	}

	// MAIN's own budget is untouched by everything the worker did.
	t.Setenv("RIG_AGENT_ID", "")
	if denied, out := preDecision(t, s, implementCall); denied {
		t.Fatalf("MAIN lost its first round to the worker's calls; out=%q", out)
	}
}
