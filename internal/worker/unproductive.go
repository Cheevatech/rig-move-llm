package worker

import "strings"

// Thresholds that separate a legitimate "nothing to change here" conclusion
// from an unproductive round that spun without producing work.
//
// Two shapes being separated:
//   - Legitimate: concluded in a couple iterations without touching the filesystem.
//     The right answer was "nothing to change here" and the worker found it quickly.
//   - Unproductive: burned many iterations and tokens, ended with no diff, no files
//     changed — clearly not a quick deliberate conclusion.
const (
	// unproductiveIterThreshold: a "quick" conclusion uses fewer iterations.
	// 5 sits past a worker that reads, checks, and concludes ("nothing here").
	unproductiveIterThreshold = 5

	// unproductiveSpendThreshold: total tokens (input + output) below which a
	// round with no changes is still considered a quick deliberate conclusion.
	// 15000 is ~6KB of context — a worker that reads one file and concludes
	// is well under this. A worker that spins is well over it.
	unproductiveSpendThreshold = 15000
)

// determineUnproductive checks if a round that ended cleanly but produced no
// diff was unproductive — high spend with no tangible result. It sets the
// Unproductive and UnproductiveJustification fields on the Result.
//
// touchedFiles tells whether the worker wrote to the filesystem during the round
// (tracked by the engine from tool calls). A round that touched files but left
// nothing surviving is unproductive regardless of spend — it tried and failed.
func determineUnproductive(res *Result, touchedFiles bool) {
	if res.Stopped != "done" {
		return
	}
	if strings.TrimSpace(res.Diff) != "" || len(res.FilesChanged) > 0 {
		return
	}
	// No diff, no files changed, round ended on its own.
	// Was this a quick deliberate "nothing to change" or an unproductive spin?
	highIterations := res.Iterations >= unproductiveIterThreshold
	highSpend := (res.InputTokens + res.OutputTokens) > unproductiveSpendThreshold

	if touchedFiles {
		res.Unproductive = true
		res.UnproductiveJustification = "touched-files no-change"
		return
	}
	if highIterations && highSpend {
		res.Unproductive = true
		res.UnproductiveJustification = "high-spend no-change"
		return
	}
}
