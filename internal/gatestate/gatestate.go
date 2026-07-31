// Package gatestate is the tiny shared on-disk state between the worker MCP
// server (which produces Stage-0 explore evidence and records the triage
// decision) and the PreToolUse/PostToolUse hooks (which enforce them). Both
// processes resolve the same per-scope data dir from config, so a JSON file per
// concern is enough — no daemon, no IPC.
//
// Three files, all under <dataDir>/:
//
//   - explore_state.json — digest of the last worker.explore run (Stage 0):
//     which files the evidence grounds, how many candidate edit sites, whether
//     open questions remain. The triage consistency backstop reads it.
//   - triage_state.json — the EFFECTIVE triage decision (after the server-side
//     consistency override). The hook opens/closes MAIN's solo edit window
//     from it.
//   - repair_window.json — Gate B: a small, bounded edit budget opened when the
//     worker returns, letting MAIN patch tiny residue itself instead of paying
//     another delegation round-trip.
//
// stdlib only.
package gatestate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const (
	exploreFile = "explore_state.json"
	triageFile  = "triage_state.json"
	repairFile  = "repair_window.json"
	roundsFile  = "delegate_rounds.json"

	// ExploreTTL bounds how long Stage-0 evidence stays valid for triage.
	ExploreTTL = 2 * time.Hour
	// TriageTTL bounds a solo window; a stale decision falls back to deny.
	TriageTTL = 2 * time.Hour
	// RepairTTL bounds the Gate B window after a worker return.
	RepairTTL = 15 * time.Minute
	// RoundsTTL bounds the delegation counter. The counter is normally reset by
	// the UserPromptSubmit hook (a new message is a new intake); the TTL is the
	// backstop for an install where that hook never runs, so a stale count can
	// never wedge MAIN out of delegating forever.
	RoundsTTL = 2 * time.Hour
)

// Explore is the persisted digest of a worker.explore run — only what the
// triage backstop and the divergence check need, not the full report.
type Explore struct {
	Repo               string    `json:"repo"`
	RelevantFiles      []string  `json:"relevant_files"`
	EditSiteFiles      []string  `json:"edit_site_files"` // distinct files carrying candidate_edit_sites
	NSites             int       `json:"n_sites"`
	NOpenQuestions     int       `json:"n_open_questions"`
	CoverageIncomplete bool      `json:"coverage_incomplete"`
	At                 time.Time `json:"at"`
}

// Triage is the effective intake decision the hook enforces.
type Triage struct {
	Decision   string    `json:"decision"` // "solo" | "delegate"
	Reason     string    `json:"reason"`
	Overridden bool      `json:"overridden"` // consistency backstop changed MAIN's declaration
	SoloFiles  []string  `json:"solo_files"` // files MAIN may edit while solo (from Stage-0 evidence)
	At         time.Time `json:"at"`
}

// Repair is the Gate B budget: a few small edits, then the window closes.
type Repair struct {
	EditsLeft int       `json:"edits_left"`
	OpenedAt  time.Time `json:"opened_at"`
}

// MaxUnproductiveRefunds caps how many unproductive rounds one turn can be
// refunded. The refund exists because a round that produced nothing must not
// cost MAIN a slot it could have spent producing something (measured 2026-07-31:
// a round burned 16 iterations / 416k input tokens, returned an empty diff, and
// still consumed one of the three slots). The cap is what makes the rule
// terminate: without it, "unproductive rounds are free" reinstates exactly the
// unbounded retry loop the budget exists to stop. With the cap, a turn can never
// exceed MaxRounds + MaxUnproductiveRefunds delegate calls, however the rounds
// end. 2 covers the measured failure (one empty round) plus one retry of a
// narrowed spec.
const MaxUnproductiveRefunds = 2

// Rounds counts how many times MAIN has delegated to the worker in the CURRENT
// turn. It is the anti-runaway budget: a delegation that fails in a way that
// repeats (a timeout, a worker that returns nothing) invites MAIN to delegate
// again forever, which is the map9 money failure with no natural stop (#18).
//
// Refunded counts rounds given back because the engine flagged them
// unproductive (Result.Unproductive): the effective spend is Count - Refunded.
// It lives in the same file as Count so ClearTurn and the TTL reset both
// together — a parallel counter could drift and reopen the runaway loop.
type Rounds struct {
	Count    int       `json:"count"`
	Refunded int       `json:"refunded,omitempty"`
	At       time.Time `json:"at"`
}

// Effective is the spend the budget compares against MaxRounds: rounds paid
// minus rounds refunded, never negative.
func (r Rounds) Effective() int {
	if n := r.Count - r.Refunded; n > 0 {
		return n
	}
	return 0
}

// BumpRound records one delegation and returns the new count for this turn. A
// missing or expired counter starts over at 1. A write failure returns the count
// it would have been: the budget must never deny MAIN because of a disk error,
// so an unwritable state dir degrades to "no budget" rather than "no delegating".
func BumpRound(dir string) int {
	r, _ := ReadRounds(dir)
	r.Count++
	r.At = time.Now()
	if write(filepath.Join(dir, roundsFile), r) != nil {
		return 1
	}
	return r.Count
}

// RefundRound gives back one delegation slot for a round the engine flagged
// unproductive, up to MaxUnproductiveRefunds per turn. It returns the new state
// and whether a refund was granted. No fresh counter, nothing spent, or the cap
// reached means no refund. Like the rest of this file it is fail-open: a write
// error grants nothing but harms nothing — the budget merely stays as strict as
// it already was, and a disk error must never manufacture extra rounds.
func RefundRound(dir string) (Rounds, bool) {
	r, ok := ReadRounds(dir)
	if !ok || r.Count == 0 || r.Refunded >= MaxUnproductiveRefunds {
		return r, false
	}
	r.Refunded++
	if write(filepath.Join(dir, roundsFile), r) != nil {
		r.Refunded--
		return r, false
	}
	return r, true
}

// ReadRounds returns this turn's delegation count and whether it exists and is
// still fresh. An expired counter reads as zero.
func ReadRounds(dir string) (Rounds, bool) {
	var r Rounds
	if !read(filepath.Join(dir, roundsFile), &r) {
		return Rounds{}, false
	}
	if time.Since(r.At) > RoundsTTL {
		return Rounds{}, false
	}
	return r, true
}

func WriteExplore(dir string, e Explore) error { return write(filepath.Join(dir, exploreFile), e) }
func WriteTriage(dir string, t Triage) error   { return write(filepath.Join(dir, triageFile), t) }
func WriteRepair(dir string, r Repair) error   { return write(filepath.Join(dir, repairFile), r) }

// ReadExplore returns the digest and whether it exists and is still fresh.
func ReadExplore(dir string) (Explore, bool) {
	var e Explore
	if !read(filepath.Join(dir, exploreFile), &e) {
		return e, false
	}
	return e, time.Since(e.At) <= ExploreTTL
}

// ReadTriage returns the decision and whether it exists and is still fresh.
func ReadTriage(dir string) (Triage, bool) {
	var t Triage
	if !read(filepath.Join(dir, triageFile), &t) {
		return t, false
	}
	return t, time.Since(t.At) <= TriageTTL
}

// ReadRepair returns the window and whether it is open (fresh + budget left).
func ReadRepair(dir string) (Repair, bool) {
	var r Repair
	if !read(filepath.Join(dir, repairFile), &r) {
		return r, false
	}
	return r, r.EditsLeft > 0 && time.Since(r.OpenedAt) <= RepairTTL
}

// ClearTurn removes the per-task decisions (triage + repair + the delegation
// budget) but keeps the explore evidence, which is expensive to redo and can be
// re-triaged against. Called on every UserPromptSubmit: a new user message is a
// new intake — and, for the budget, it is also the human's acknowledgement that
// another round is wanted.
func ClearTurn(dir string) {
	_ = os.Remove(filepath.Join(dir, triageFile))
	_ = os.Remove(filepath.Join(dir, repairFile))
	_ = os.Remove(filepath.Join(dir, roundsFile))
}

// ClearRepair closes the Gate B window (e.g. when MAIN re-delegates instead).
func ClearRepair(dir string) { _ = os.Remove(filepath.Join(dir, repairFile)) }

func write(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func read(path string, v any) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(b, v) == nil
}
