package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/gatestate"
)

func budgetState(t *testing.T, max int) *State {
	t.Helper()
	dir := t.TempDir()
	return &State{
		Enabled:   true,
		GatePaths: filepath.Join(dir, "gate_paths"),
		LogPath:   filepath.Join(dir, "log"),
		StateDir:  dir,
		MaxRounds: max,
	}
}

const implementCall = `{"tool_name":"mcp__worker__implement","tool_input":{}}`

// The runaway shape from #18: a round fails, MAIN delegates again, forever. The
// budget lets a genuinely iterative task through (3 rounds was the measured cost
// of a hard one) and stops the round after it.
func TestRoundBudget_DeniesTheRoundAfterTheBudget(t *testing.T) {
	s := budgetState(t, 3)

	for i := 1; i <= 3; i++ {
		if denied, out := preDecision(t, s, implementCall); denied {
			t.Fatalf("round %d must be allowed within a budget of 3; out=%q", i, out)
		}
	}

	denied, out := preDecision(t, s, implementCall)
	if !denied {
		t.Fatal("the 4th delegation must be denied")
	}
	for _, want := range []string{"budget for this turn is spent", "STOP now and report to the human", "resets on their next message"} {
		if !strings.Contains(out, want) {
			t.Errorf("deny reason missing %q; out=%q", want, out)
		}
	}
}

// The escape hatch is the human's next message — no new knob. UserPromptSubmit
// calls ClearTurn, which must reopen the budget.
func TestRoundBudget_ClearTurnReopensTheBudget(t *testing.T) {
	s := budgetState(t, 1)

	if denied, _ := preDecision(t, s, implementCall); denied {
		t.Fatal("the first delegation must be allowed")
	}
	if denied, _ := preDecision(t, s, implementCall); !denied {
		t.Fatal("the second delegation must be denied under a budget of 1")
	}

	gatestate.ClearTurn(s.StateDir)

	if denied, out := preDecision(t, s, implementCall); denied {
		t.Fatalf("a new turn must reopen the budget; out=%q", out)
	}
}

// MAIN must be able to tell "out of budget" apart from any other refusal,
// and must know the knob that raises it — so the denial message names the
// round, the cap, and the environment variable.
func TestRoundBudget_DenialMessageNamesRoundCapAndOverride(t *testing.T) {
	s := budgetState(t, 3)

	// Spend the budget.
	for i := 1; i <= 3; i++ {
		if denied, _ := preDecision(t, s, implementCall); denied {
			t.Fatalf("round %d must be allowed within a budget of 3", i)
		}
	}

	// The 4th call is denied — inspect the reason.
	denied, out := preDecision(t, s, implementCall)
	if !denied {
		t.Fatal("the 4th delegation must be denied")
	}

	for _, want := range []string{
		"delegate call 4",         // the round number of the refused call
		"the limit is 3",          // the cap
		"RIG_MAX_DELEGATE_ROUNDS", // the override knob
	} {
		if !strings.Contains(out, want) {
			t.Errorf("deny reason missing %q; out=%q", want, out)
		}
	}
}

// Exercised at the default cap of 3 (not 1) to prove the reset is a true
// reset, not an off-by-one carry.
func TestRoundBudget_CounterDoesNotLeakAcrossTurns(t *testing.T) {
	s := budgetState(t, 3)

	// Spend all 3 rounds.
	for i := 1; i <= 3; i++ {
		if denied, _ := preDecision(t, s, implementCall); denied {
			t.Fatalf("round %d must be allowed", i)
		}
	}
	// The 4th is denied.
	if denied, _ := preDecision(t, s, implementCall); !denied {
		t.Fatal("the 4th delegation must be denied")
	}

	// Clear the turn — simulates the human's next message.
	gatestate.ClearTurn(s.StateDir)

	// A full fresh budget of 3 must be available.
	for i := 1; i <= 3; i++ {
		if denied, out := preDecision(t, s, implementCall); denied {
			t.Fatalf("new turn round %d must be allowed; out=%q", i, out)
		}
	}
	// And the 4th of the new turn must be denied again.
	if denied, _ := preDecision(t, s, implementCall); !denied {
		t.Fatal("the 4th delegation of the new turn must be denied")
	}
}

// A denied round must not have side effects: freezing a contract or closing the
// repair window on a call that never ran would corrupt the state MAIN reasons
// from.
func TestRoundBudget_DeniedRoundHasNoSideEffects(t *testing.T) {
	s := budgetState(t, 1)
	repo := filepath.Join(t.TempDir(), "repo")
	gate := filepath.Join(repo, ".gate")
	if err := os.MkdirAll(gate, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gate, "repro.py"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if denied, _ := preDecision(t, s, implementCall); denied {
		t.Fatal("the first delegation must be allowed")
	}

	// Authored after the budget was spent: the denied call must leave it alone.
	s.appendGatePath(gate)
	if err := gatestate.WriteRepair(s.StateDir, gatestate.Repair{EditsLeft: 2, OpenedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	if denied, _ := preDecision(t, s, implementCall); !denied {
		t.Fatal("the second delegation must be denied")
	}
	if isDir(filepath.Join(repo, ".gate.frozen")) {
		t.Error("a denied delegation must not freeze the gate contract")
	}
	if _, open := gatestate.ReadRepair(s.StateDir); !open {
		t.Error("a denied delegation must not close the open repair window")
	}
}

// Fail-open: the budget is a guard rail, never a reason MAIN cannot work. With
// it disabled (0) or with no state dir, delegation behaves exactly as before.
func TestRoundBudget_FailsOpen(t *testing.T) {
	t.Run("disabled by zero", func(t *testing.T) {
		s := budgetState(t, 0)
		for i := 0; i < 5; i++ {
			if denied, out := preDecision(t, s, implementCall); denied {
				t.Fatalf("delegation %d denied with the budget disabled; out=%q", i, out)
			}
		}
	})
	t.Run("no state dir", func(t *testing.T) {
		s := budgetState(t, 1)
		s.StateDir = ""
		for i := 0; i < 5; i++ {
			if denied, out := preDecision(t, s, implementCall); denied {
				t.Fatalf("delegation %d denied without a state dir; out=%q", i, out)
			}
		}
	})
}

// Only implement is budgeted: the free Stage-0 tools must stay unmetered, or the
// budget would push MAIN back into reading the repo itself.
func TestRoundBudget_OnlyMetersImplement(t *testing.T) {
	s := budgetState(t, 1)
	for i := 0; i < 5; i++ {
		if denied, out := preDecision(t, s, `{"tool_name":"mcp__worker__explore","tool_input":{}}`); denied {
			t.Fatalf("explore call %d denied; out=%q", i, out)
		}
	}
	if denied, _ := preDecision(t, s, implementCall); denied {
		t.Fatal("implement must still have its full budget after explore calls")
	}
}

// An unproductive round is refunded: after the engine gives the slot back, the
// full productive budget is still available. (The refund itself is wired at the
// worker MCP server; here the budget math is what's under test.)
func TestRoundBudget_UnproductiveRoundDoesNotConsumeASlot(t *testing.T) {
	s := budgetState(t, 3)

	// Round 1 runs and comes back unproductive → refunded.
	if denied, _ := preDecision(t, s, implementCall); denied {
		t.Fatal("round 1 must be allowed")
	}
	if _, ok := gatestate.RefundRound(s.StateDir); !ok {
		t.Fatal("refund must be granted for the first unproductive round")
	}

	// A full budget of 3 productive rounds must still fit.
	for i := 1; i <= 3; i++ {
		if denied, out := preDecision(t, s, implementCall); denied {
			t.Fatalf("productive round %d after a refund must be allowed; out=%q", i, out)
		}
	}
	if denied, _ := preDecision(t, s, implementCall); !denied {
		t.Fatal("the round after the refunded budget must still be denied")
	}
}

// A caller that only ever produces unproductive rounds is STILL stopped: the
// refund cap bounds the loop at MaxRounds + MaxUnproductiveRefunds calls.
func TestRoundBudget_OnlyUnproductiveRoundsIsStillStopped(t *testing.T) {
	s := budgetState(t, 3)

	allowed := 0
	for i := 1; i <= 10; i++ {
		denied, _ := preDecision(t, s, implementCall)
		if denied {
			break
		}
		allowed++
		// Every round comes back unproductive; past the cap the refund is refused.
		gatestate.RefundRound(s.StateDir)
	}

	want := s.MaxRounds + gatestate.MaxUnproductiveRefunds
	if allowed != want {
		t.Fatalf("allowed %d all-unproductive rounds, want exactly MaxRounds+cap = %d", allowed, want)
	}
}

// The denial message must state the TRUE budget under the refund rule: the
// effective call number, that zero calls remain, and the refund accounting.
func TestRoundBudget_DenialMessageStatesTrueRemaining(t *testing.T) {
	s := budgetState(t, 3)

	// One refunded round, then a full productive budget, then the denied call.
	if denied, _ := preDecision(t, s, implementCall); denied {
		t.Fatal("round 1 must be allowed")
	}
	gatestate.RefundRound(s.StateDir)
	for i := 1; i <= 3; i++ {
		if denied, _ := preDecision(t, s, implementCall); denied {
			t.Fatalf("productive round %d must be allowed", i)
		}
	}
	denied, out := preDecision(t, s, implementCall)
	if !denied {
		t.Fatal("the call past the refunded budget must be denied")
	}
	for _, want := range []string{
		"delegate call 4", // effective: 5 paid - 1 refunded
		"the limit is 3",
		"ZERO delegate calls remain",
		"refunded up to 2 per turn",
		"1 of yours were",
		"RIG_MAX_DELEGATE_ROUNDS",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("deny reason missing %q; out=%q", want, out)
		}
	}
}
