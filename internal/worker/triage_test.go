package worker

import (
	"testing"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/internal/gatestate"
)

func triageEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("RIG_STATE_DIR", dir)
	return NewEngine(config.Config{}), dir
}

func seedExplore(t *testing.T, dir string, ev gatestate.Explore) {
	t.Helper()
	ev.At = time.Now()
	if err := gatestate.WriteExplore(dir, ev); err != nil {
		t.Fatal(err)
	}
}

func TestTriageSoloAccepted(t *testing.T) {
	e, dir := triageEngine(t)
	seedExplore(t, dir, gatestate.Explore{EditSiteFiles: []string{"util.py"}, NSites: 1})
	out := e.Triage("solo", "single verified site", "soft")
	if out.Effective != "solo" || out.Overridden || len(out.SoloFiles) != 1 {
		t.Fatalf("clean single-file evidence should accept solo: %+v", out)
	}
	tr, fresh := gatestate.ReadTriage(dir)
	if !fresh || tr.Decision != "solo" || tr.SoloFiles[0] != "util.py" {
		t.Fatalf("persisted triage wrong: %+v", tr)
	}
}

func TestTriageConsistencyOverrides(t *testing.T) {
	cases := []struct {
		name string
		ev   gatestate.Explore
	}{
		{"multi-file", gatestate.Explore{EditSiteFiles: []string{"a.py", "b.py"}, NSites: 2}},
		{"too-many-sites", gatestate.Explore{EditSiteFiles: []string{"a.py"}, NSites: 4}},
		{"open-questions", gatestate.Explore{EditSiteFiles: []string{"a.py"}, NSites: 1, NOpenQuestions: 1}},
		{"coverage-incomplete", gatestate.Explore{EditSiteFiles: []string{"a.py"}, NSites: 1, CoverageIncomplete: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, dir := triageEngine(t)
			seedExplore(t, dir, tc.ev)
			out := e.Triage("solo", "looks easy", "soft")
			if out.Effective != "delegate" || !out.Overridden {
				t.Fatalf("%s must override solo->delegate: %+v", tc.name, out)
			}
		})
	}
}

func TestTriageSoloWithoutEvidence(t *testing.T) {
	e, _ := triageEngine(t)
	out := e.Triage("solo", "no explore ran", "soft")
	if out.Effective != "delegate" || !out.Overridden {
		t.Fatalf("solo without Stage-0 evidence must delegate: %+v", out)
	}
}

func TestTriageHardMode(t *testing.T) {
	e, dir := triageEngine(t)
	seedExplore(t, dir, gatestate.Explore{EditSiteFiles: []string{"a.py"}, NSites: 1})
	out := e.Triage("solo", "tiny", "hard")
	if out.Effective != "delegate" || !out.Overridden {
		t.Fatalf("hard mode must never open a solo window: %+v", out)
	}
}

func TestTriageDelegatePassesThrough(t *testing.T) {
	e, dir := triageEngine(t)
	out := e.Triage("delegate", "multi-file per evidence", "soft")
	if out.Effective != "delegate" || out.Overridden {
		t.Fatalf("declared delegate should stand: %+v", out)
	}
	if tr, fresh := gatestate.ReadTriage(dir); !fresh || tr.Decision != "delegate" {
		t.Fatalf("persisted triage wrong: %+v", tr)
	}
}
