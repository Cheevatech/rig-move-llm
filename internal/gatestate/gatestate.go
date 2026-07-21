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

	// ExploreTTL bounds how long Stage-0 evidence stays valid for triage.
	ExploreTTL = 2 * time.Hour
	// TriageTTL bounds a solo window; a stale decision falls back to deny.
	TriageTTL = 2 * time.Hour
	// RepairTTL bounds the Gate B window after a worker return.
	RepairTTL = 15 * time.Minute
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

// ClearTurn removes the per-task decisions (triage + repair) but keeps the
// explore evidence, which is expensive to redo and can be re-triaged against.
// Called on every UserPromptSubmit: a new user message is a new intake.
func ClearTurn(dir string) {
	_ = os.Remove(filepath.Join(dir, triageFile))
	_ = os.Remove(filepath.Join(dir, repairFile))
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
