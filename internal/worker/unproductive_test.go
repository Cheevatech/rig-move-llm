package worker

import "testing"

func TestDetermineUnproductive_highSpend(t *testing.T) {
	// Round ends on its own, no files changed, HIGH iteration/token spend.
	res := Result{
		Stopped:      "done",
		Diff:         "",
		FilesChanged: nil,
		Iterations:   16,
		InputTokens:  416000,
		OutputTokens: 5000,
	}
	determineUnproductive(&res, false)
	if !res.Unproductive {
		t.Errorf("want unproductive for high-spend no-change round")
	}
	if res.UnproductiveJustification != "high-spend no-change" {
		t.Errorf("justification = %q, want high-spend no-change", res.UnproductiveJustification)
	}
}

func TestDetermineUnproductive_quickConclusion(t *testing.T) {
	// Short round (low iterations/tokens) concluding nothing-to-change.
	res := Result{
		Stopped:      "done",
		Diff:         "",
		FilesChanged: nil,
		Iterations:   2,
		InputTokens:  3000,
		OutputTokens: 200,
	}
	determineUnproductive(&res, false)
	if res.Unproductive {
		t.Errorf("want NOT unproductive for quick no-change round, got justification=%q", res.UnproductiveJustification)
	}
}

func TestDetermineUnproductive_withDiff(t *testing.T) {
	// Round with a non-empty diff is unchanged.
	res := Result{
		Stopped:      "done",
		Diff:         "diff --git a/foo.py b/foo.py\n+",
		FilesChanged: []string{"foo.py"},
		Iterations:   16,
		InputTokens:  416000,
		OutputTokens: 5000,
	}
	determineUnproductive(&res, false)
	if res.Unproductive {
		t.Error("want NOT unproductive for round with a diff")
	}
}

func TestDetermineUnproductive_killedRound(t *testing.T) {
	// Existing "stopped" semantics unchanged: timeout rounds are not flagged.
	res := Result{
		Stopped:      "timeout",
		Diff:         "",
		FilesChanged: nil,
		Iterations:   16,
		InputTokens:  416000,
		OutputTokens: 5000,
	}
	determineUnproductive(&res, false)
	if res.Unproductive {
		t.Error("want NOT unproductive for killed round")
	}
	if res.Stopped != "timeout" {
		t.Errorf("stopped = %q, want timeout unchanged", res.Stopped)
	}
}

func TestDetermineUnproductive_touchedFiles(t *testing.T) {
	// Round that touched the filesystem but left nothing surviving.
	res := Result{
		Stopped:      "done",
		Diff:         "",
		FilesChanged: nil,
		Iterations:   2,
		InputTokens:  3000,
		OutputTokens: 200,
	}
	determineUnproductive(&res, true)
	if !res.Unproductive {
		t.Error("want unproductive for touched-files no-change round")
	}
	if res.UnproductiveJustification != "touched-files no-change" {
		t.Errorf("justification = %q, want touched-files no-change", res.UnproductiveJustification)
	}
}

func TestDetermineUnproductive_maxIters(t *testing.T) {
	// max_iters rounds are not flagged (not stopped on their own).
	res := Result{
		Stopped:         "max_iters",
		Diff:            "",
		FilesChanged:    nil,
		Iterations:      10,
		InputTokens:     416000,
		OutputTokens:    5000,
		HitIterationCap: true,
	}
	determineUnproductive(&res, false)
	if res.Unproductive {
		t.Error("want NOT unproductive for max_iters round")
	}
}

func TestDetermineUnproductive_error(t *testing.T) {
	// Error rounds are not flagged.
	res := Result{
		Stopped:      "error",
		Diff:         "",
		FilesChanged: nil,
		Iterations:   1,
		InputTokens:  500,
		OutputTokens: 100,
		Err:          "worker endpoint unreachable",
	}
	determineUnproductive(&res, false)
	if res.Unproductive {
		t.Error("want NOT unproductive for error round")
	}
}

func TestDetermineUnproductive_boundaryIterations(t *testing.T) {
	// At the iteration threshold but low spend — not unproductive.
	res := Result{
		Stopped:      "done",
		Diff:         "",
		FilesChanged: nil,
		Iterations:   unproductiveIterThreshold,
		InputTokens:  1000,
		OutputTokens: 100,
	}
	determineUnproductive(&res, false)
	if res.Unproductive {
		t.Error("want NOT unproductive: at iter threshold but low spend")
	}
}

func TestDetermineUnproductive_boundarySpend(t *testing.T) {
	// High iterations but at the spend threshold — not unproductive (both must be high).
	res := Result{
		Stopped:      "done",
		Diff:         "",
		FilesChanged: nil,
		Iterations:   10,
		InputTokens:  unproductiveSpendThreshold,
		OutputTokens: 0,
	}
	determineUnproductive(&res, false)
	if res.Unproductive {
		t.Error("want NOT unproductive: at spend threshold, not above")
	}
}

func TestDetermineUnproductive_whitespacedDiff(t *testing.T) {
	// A diff that is whitespace-only is treated as empty.
	res := Result{
		Stopped:      "done",
		Diff:         "   \n  \t  ",
		FilesChanged: nil,
		Iterations:   16,
		InputTokens:  416000,
		OutputTokens: 5000,
	}
	determineUnproductive(&res, false)
	if !res.Unproductive {
		t.Error("want unproductive for whitespace-only diff with high spend")
	}
}
