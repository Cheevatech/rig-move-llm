package worker

import (
	"context"
	"strings"
	"testing"
)

// The contract B3 and B4 locked: every drill argument the manifest publishes
// must be servable by the drill, and the bytes it serves must be the change.
func TestManifestDrillsBackToTheRealChange(t *testing.T) {
	repo := fixtureRepo(t)
	diff, _ := (&Engine{}).collectDiff(context.Background(), repo)

	tier := TierResult(Result{Diff: diff, Stopped: "done", LastTest: "1 passed\nall green\n"}, repo, 1)
	if len(tier.Changes) == 0 {
		t.Fatal("no manifest entries")
	}
	for _, c := range tier.Changes {
		res, err := ShowChange(context.Background(), repo, c.File, c.Line, c.EndLine, "diff")
		if err != nil {
			t.Fatalf("manifest entry %s:%d-%d is not drillable: %v", c.File, c.Line, c.EndLine, err)
		}
		if res.Hunks == 0 {
			t.Errorf("manifest entry %s:%d-%d drilled to nothing", c.File, c.Line, c.EndLine)
		}
	}
	if tier.Verify.Drill == nil {
		t.Fatal("verify tier published no drill ref")
	}
	d := tier.Verify.Drill
	if d.Tool != "show_change" {
		t.Fatalf("verify drill names %q, not show_change", d.Tool)
	}
	log, err := ShowChange(context.Background(), repo, d.File, d.StartLine, d.EndLine, d.Kind)
	if err != nil {
		t.Fatalf("parked test log is not drillable: %v", err)
	}
	if !strings.Contains(log.Body, "all green") {
		t.Fatalf("drilled log lost the raw output: %q", log.Body)
	}
}
