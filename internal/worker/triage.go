package worker

import (
	"fmt"
	"strings"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/gatestate"
)

// Gate A intake triage. MAIN declares solo|delegate on the Stage-0 evidence by
// calling the triage tool; the server applies the deterministic consistency
// backstop (evidence-relative, no fixed N/M thresholds) and persists the
// EFFECTIVE decision for the hook to enforce:
//
//   - declared solo but the evidence shows multi-file candidate edit sites,
//     many sites, open questions, or incomplete coverage → overridden to
//     delegate (a solo declaration must be consistent with what Stage 0 saw).
//   - declared solo with single-file, low-site, no-open-question evidence →
//     accepted; the hook opens a solo edit window restricted to those files
//     (the divergence check).
//
// In gate_mode=hard the effective decision is always delegate — the tool stays
// honest for A/B arms running the old hard-deny posture.

// TriageOutcome is what the triage tool reports back to MAIN.
type TriageOutcome struct {
	Declared   string   `json:"declared"`
	Effective  string   `json:"effective"`
	Overridden bool     `json:"overridden"`
	Reason     string   `json:"reason"`
	SoloFiles  []string `json:"solo_files,omitempty"`
	Guidance   string   `json:"guidance"`
}

// soloConsistencyMax bounds how many candidate edit sites still count as a
// "single obvious small change" — beyond this, solo is inconsistent with the
// evidence regardless of what MAIN declared.
const soloConsistencyMax = 3

// Triage records MAIN's intake decision, applying the consistency backstop
// against the persisted Stage-0 digest. gateMode is config.GateMode.
func (e *Engine) Triage(declared, reason, gateMode string) TriageOutcome {
	declared = strings.ToLower(strings.TrimSpace(declared))
	if declared != "solo" && declared != "delegate" {
		return TriageOutcome{Declared: declared, Effective: "delegate", Reason: "unknown decision — treated as delegate",
			Guidance: "Call with decision \"solo\" or \"delegate\"."}
	}

	out := TriageOutcome{Declared: declared, Effective: declared, Reason: reason}
	dir := e.stateDir()

	if gateMode != "soft" {
		out.Effective = "delegate"
		out.Overridden = declared == "solo"
		out.Reason = "gate_mode=hard: all code changes go through the worker (solo windows disabled)"
		out.Guidance = "Delegate the change to mcp__worker__implement."
		e.persistTriage(dir, out)
		return out
	}

	if declared == "solo" {
		ev, fresh := gatestate.ReadExplore(dir)
		switch {
		case !fresh:
			out.Effective = "delegate"
			out.Overridden = true
			out.Reason = "no fresh Stage-0 evidence — run mcp__worker__explore first; solo needs grounded scope"
		case len(ev.EditSiteFiles) > 1:
			out.Effective = "delegate"
			out.Overridden = true
			out.Reason = fmt.Sprintf("evidence shows edit sites across %d files (%s) — not a single-point change",
				len(ev.EditSiteFiles), strings.Join(ev.EditSiteFiles, ", "))
		case ev.NSites > soloConsistencyMax:
			out.Effective = "delegate"
			out.Overridden = true
			out.Reason = fmt.Sprintf("evidence lists %d candidate edit sites (> %d) — too sprawling for solo", ev.NSites, soloConsistencyMax)
		case ev.NOpenQuestions > 0:
			out.Effective = "delegate"
			out.Overridden = true
			out.Reason = fmt.Sprintf("Stage-0 report left %d open question(s) — unresolved uncertainty means delegate", ev.NOpenQuestions)
		case ev.CoverageIncomplete:
			out.Effective = "delegate"
			out.Overridden = true
			out.Reason = "Stage-0 coverage was incomplete — the evidence base is not solid enough for solo"
		default:
			out.SoloFiles = ev.EditSiteFiles
		}
	}

	if out.Effective == "solo" {
		out.Guidance = "Solo accepted: you may edit ONLY " + strings.Join(out.SoloFiles, ", ") +
			" (small, scoped edits at the verified sites). Edits elsewhere or oversized edits are denied — re-triage or delegate."
	} else if out.Guidance == "" {
		out.Guidance = "Delegate the change to mcp__worker__implement with a scoped spec (include the Stage-0 evidence)."
	}
	e.persistTriage(dir, out)
	return out
}

func (e *Engine) persistTriage(dir string, out TriageOutcome) {
	_ = gatestate.WriteTriage(dir, gatestate.Triage{
		Decision:   out.Effective,
		Reason:     out.Reason,
		Overridden: out.Overridden,
		SoloFiles:  out.SoloFiles,
		At:         time.Now(),
	})
}
