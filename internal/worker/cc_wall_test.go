package worker

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/config"
)

// gateUse / gateResult are the two stream events that bracket one gate command.
// They are written out longhand because the bracketing — tool_use opens the
// span, the matching tool_result closes it — IS what is under test.
func gateUse(id, cmd string) string {
	return `echo '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"` + id +
		`","name":"Bash","input":{"command":"` + cmd + `"}}]}}'`
}

func gateResult(id string) string {
	return `echo '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + id +
		`","content":"# pass 12"}]}}'`
}

// The accounting the wall guard is built on (#57): time inside a gate command is
// credited, overlapping gates are credited once (they are wall-clock, not CPU),
// and the credit is capped so a worker that never leaves its gate still dies.
func TestCCActivityCreditsGateTime(t *testing.T) {
	t.Run("an open span makes working time stand still", func(t *testing.T) {
		act := newCCActivity()
		act.gateBegin("g1")
		time.Sleep(60 * time.Millisecond)
		// While the gate runs, gateTime grows exactly as fast as elapsed, so the
		// wall guard's budget cannot advance — this is why it cannot fire mid-gate.
		if w := act.working(time.Minute); w > 20*time.Millisecond {
			t.Fatalf("working time = %s during an open gate span, want ~0", w)
		}
	})

	t.Run("a closed span stays credited", func(t *testing.T) {
		act := newCCActivity()
		act.gateBegin("g1")
		time.Sleep(60 * time.Millisecond)
		act.gateEnd("g1")
		if g := act.gateTime(); g < 50*time.Millisecond {
			t.Fatalf("gateTime = %s, want the whole closed span", g)
		}
		time.Sleep(40 * time.Millisecond)
		// Post-gate time is charged normally; the earlier span is not re-charged.
		if w := act.working(time.Minute); w < 30*time.Millisecond || w > 60*time.Millisecond {
			t.Fatalf("working time = %s, want only the post-gate time", w)
		}
	})

	t.Run("overlapping gates are credited once", func(t *testing.T) {
		act := newCCActivity()
		act.gateBegin("g1")
		time.Sleep(30 * time.Millisecond)
		act.gateBegin("g2")
		time.Sleep(30 * time.Millisecond)
		act.gateEnd("g1")
		// g2 is still open, so the span must NOT have closed yet.
		time.Sleep(30 * time.Millisecond)
		act.gateEnd("g2")
		if g := act.gateTime(); g > 200*time.Millisecond {
			t.Fatalf("gateTime = %s — overlapping gates were double-counted", g)
		}
		if g := act.gateTime(); g < 80*time.Millisecond {
			t.Fatalf("gateTime = %s — the span closed early, while g2 was still running", g)
		}
	})

	t.Run("the credit is capped", func(t *testing.T) {
		act := newCCActivity()
		act.gateBegin("g1")
		time.Sleep(80 * time.Millisecond)
		// Only 10ms of the ~80ms span may be excused, so working time keeps
		// growing — which is what stops an endless gate from buying endless wall.
		if w := act.working(10 * time.Millisecond); w < 50*time.Millisecond {
			t.Fatalf("working time = %s with a 10ms credit, want the uncredited remainder", w)
		}
	})

	t.Run("an unknown tool_result does not close a span", func(t *testing.T) {
		act := newCCActivity()
		act.gateBegin("g1")
		act.gateEnd("not-a-gate")
		time.Sleep(40 * time.Millisecond)
		if w := act.working(time.Minute); w > 15*time.Millisecond {
			t.Fatalf("working time = %s — a foreign tool_result closed the gate span", w)
		}
	})
}

// The measured failure #57 was filed against: round 3 of the tj/commander.js
// task was killed at the 50-minute wall in the MIDDLE of `node --test`, so a
// round whose work was already on disk was thrown away and the task needed two
// more rounds. A worker that spends the whole wall inside its gate and then
// finishes must now survive.
func TestWallGuardDoesNotFireWhileTheGateRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake worker is a shell script")
	}
	repo := t.TempDir()
	bin := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/sh\n" +
		gateUse("g1", "npm test") + "\n" +
		"sleep 3\n" + // the gate is running: silent, and far past the wall
		gateResult("g1") + "\n" +
		`echo '{"type":"result","subtype":"success","result":"fixed it","num_turns":3}'` + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RIG_WORKER_RUN_TIMEOUT", "1")   // wall: 1s of WORKING time
	t.Setenv("RIG_WORKER_GATE_CREDIT", "600") // plenty of credit for the gate

	e := NewEngine(config.Config{})
	var res Result
	ctx, cancel := context.WithTimeout(context.Background(), WallCeiling())
	defer cancel()
	if !e.runCCOnce(ctx, bin, "http://127.0.0.1:1/v1", repo, "task", &res, &ccProof{}, true) {
		t.Fatalf("a worker waiting on its own gate must not be killed: stopped=%q err=%q", res.Stopped, res.Err)
	}
	if res.Stopped != "done" {
		t.Fatalf("stopped = %q, want done (err=%q)", res.Stopped, res.Err)
	}
	// The gate's own output must survive too — a round killed mid-gate used to
	// return a truncated test log as its verdict.
	if !strings.Contains(res.LastTest, "# pass 12") {
		t.Errorf("the gate output was lost: LastTest=%q", res.LastTest)
	}
}

// The other half of the same guard: time NOT spent in a gate is still charged,
// so a worker that is merely churning dies on schedule and says so.
func TestWallGuardStillKillsANonGateRound(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake worker is a shell script")
	}
	repo := t.TempDir()
	bin := filepath.Join(t.TempDir(), "fake-claude")
	// Reading files forever: streaming (so the stall guard stays quiet) but not
	// a gate, so every second counts against the wall.
	script := "#!/bin/sh\n" +
		"while true; do\n" +
		`  echo '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"r1","name":"Read","input":{"file_path":"README.md"}}]}}'` + "\n" +
		"  sleep 0.2\n" +
		"done\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RIG_WORKER_RUN_TIMEOUT", "1")
	t.Setenv("RIG_WORKER_GATE_CREDIT", "600")

	e := NewEngine(config.Config{})
	var res Result
	ctx, cancel := context.WithTimeout(context.Background(), WallCeiling())
	defer cancel()
	start := time.Now()
	if e.runCCOnce(ctx, bin, "http://127.0.0.1:1/v1", repo, "task", &res, &ccProof{}, true) {
		t.Fatal("a killed worker produces no result event; sawResult must be false")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("wall guard did not fire: call took %s", elapsed)
	}
	if res.Stopped != "timeout" {
		t.Fatalf("stopped = %q, want timeout (err=%q)", res.Stopped, res.Err)
	}
	for _, want := range []string{"wall guard", "working time", "Read README.md"} {
		if !strings.Contains(res.Err, want) {
			t.Errorf("diagnosis missing %q:\n%s", want, res.Err)
		}
	}
}

// The credit is a credit, not an exemption: a worker that never leaves its gate
// buys itself exactly one credit's worth of extra wall and is then killed by the
// ceiling. Without this bound, #57's fix would have removed the wall entirely.
func TestGateCreditIsBoundedByTheCeiling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake worker is a shell script")
	}
	repo := t.TempDir()
	bin := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/sh\n" +
		gateUse("g1", "npm test") + "\n" +
		"sleep 300\n" // a gate that never returns
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RIG_WORKER_RUN_TIMEOUT", "1")
	t.Setenv("RIG_WORKER_GATE_CREDIT", "1") // ceiling = 2s

	e := NewEngine(config.Config{})
	var res Result
	ctx, cancel := context.WithTimeout(context.Background(), WallCeiling())
	defer cancel()
	start := time.Now()
	if e.runCCOnce(ctx, bin, "http://127.0.0.1:1/v1", repo, "task", &res, &ccProof{}, true) {
		t.Fatal("the worker never returned a result event; sawResult must be false")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("nothing bounded the endless gate: call took %s", elapsed)
	}
	if res.Stopped != "timeout" {
		t.Fatalf("stopped = %q, want timeout (err=%q)", res.Stopped, res.Err)
	}
	// Whichever of the two bounds spoke first, the diagnosis must say the round
	// was sitting in a gate — that is the fact MAIN needs to act on.
	if !strings.Contains(res.Err, "gate") {
		t.Errorf("diagnosis does not mention the gate the worker was stuck in:\n%s", res.Err)
	}
}

// Regression on the ladder the fix widened: the ceiling sits between the wall
// and the client, so the engine is still the layer that kills its own round.
func TestWallCeilingSitsInTheLadder(t *testing.T) {
	stall, wall, ceiling, client := ccStallTimeout(), runTimeout(), WallCeiling(), ClientCallTimeout()
	if !(stall < wall && wall <= ceiling && ceiling < client) {
		t.Fatalf("ladder broken: stall %s < wall %s <= ceiling %s < client %s", stall, wall, ceiling, client)
	}
	if ceiling != wall+gateCredit() {
		t.Fatalf("ceiling %s must be wall %s + gate credit %s", ceiling, wall, gateCredit())
	}
}
