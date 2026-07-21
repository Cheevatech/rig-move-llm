package hook

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/gatestate"
)

func softState(t *testing.T) (*State, string) {
	t.Helper()
	dir := t.TempDir()
	return &State{
		Enabled:   true,
		GateMode:  "soft",
		StateDir:  dir,
		LogPath:   filepath.Join(dir, "log"),
		GatePaths: filepath.Join(dir, "gate_paths"),
	}, dir
}

func preTool(t *testing.T, s *State, payload string) string {
	t.Helper()
	var out bytes.Buffer
	if err := s.PreTool(strings.NewReader(payload), &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func editPayload(file, newString string) string {
	b, _ := json.Marshal(map[string]any{
		"tool_name":  "Edit",
		"tool_input": map[string]any{"file_path": file, "old_string": "x", "new_string": newString},
	})
	return string(b)
}

func TestSoloWindowAllowsGroundedFile(t *testing.T) {
	s, dir := softState(t)
	_ = gatestate.WriteTriage(dir, gatestate.Triage{Decision: "solo", SoloFiles: []string{"util.py"}, At: time.Now()})

	if out := preTool(t, s, editPayload("/repo/util.py", "small fix")); out != "" {
		t.Fatalf("grounded solo edit should be allowed, got %s", out)
	}
	if out := preTool(t, s, editPayload("/repo/other.py", "small fix")); !strings.Contains(out, "DIVERGENCE") {
		t.Fatalf("edit outside grounded scope must deny with divergence, got %s", out)
	}
	if out := preTool(t, s, editPayload("/repo/util.py", strings.Repeat("x", soloEditMaxBytes+1))); !strings.Contains(out, "too large") {
		t.Fatalf("oversize solo edit must deny, got %s", out)
	}
}

func TestRepairWindowBudget(t *testing.T) {
	s, dir := softState(t)

	// Worker return opens the window via PostTool.
	post := `{"tool_name":"mcp__worker__implement","tool_input":{}}`
	var out bytes.Buffer
	if err := s.PostTool(strings.NewReader(post), &out); err != nil {
		t.Fatal(err)
	}
	if _, open := gatestate.ReadRepair(dir); !open {
		t.Fatal("worker return should open the repair window")
	}

	for i := 0; i < repairEditCount; i++ {
		if out := preTool(t, s, editPayload("/repo/any.py", "tiny")); out != "" {
			t.Fatalf("repair edit %d should be allowed, got %s", i+1, out)
		}
	}
	if out := preTool(t, s, editPayload("/repo/any.py", "tiny")); !strings.Contains(out, "deny") {
		t.Fatalf("budget exhausted must fall back to deny, got %s", out)
	}
}

func TestRepairWindowOversizeDenies(t *testing.T) {
	s, dir := softState(t)
	_ = gatestate.WriteRepair(dir, gatestate.Repair{EditsLeft: repairEditCount, OpenedAt: time.Now()})
	out := preTool(t, s, editPayload("/repo/any.py", strings.Repeat("x", repairEditMaxBytes+1)))
	if !strings.Contains(out, "re-delegate") {
		t.Fatalf("oversize repair must deny with re-delegate steer, got %s", out)
	}
	if rep, _ := gatestate.ReadRepair(dir); rep.EditsLeft != repairEditCount {
		t.Fatalf("denied edit must not burn budget: %+v", rep)
	}
}

func TestHardModeIgnoresWindows(t *testing.T) {
	s, dir := softState(t)
	s.GateMode = "hard"
	_ = gatestate.WriteTriage(dir, gatestate.Triage{Decision: "solo", SoloFiles: []string{"util.py"}, At: time.Now()})
	_ = gatestate.WriteRepair(dir, gatestate.Repair{EditsLeft: 3, OpenedAt: time.Now()})
	if out := preTool(t, s, editPayload("/repo/util.py", "tiny")); !strings.Contains(out, "deny") {
		t.Fatalf("hard mode must deny regardless of windows, got %s", out)
	}
}

func TestImplementCallClosesRepairWindow(t *testing.T) {
	s, dir := softState(t)
	_ = gatestate.WriteRepair(dir, gatestate.Repair{EditsLeft: 3, OpenedAt: time.Now()})
	payload := `{"tool_name":"mcp__worker__implement","tool_input":{}}`
	if out := preTool(t, s, payload); out != "" {
		t.Fatalf("worker call should be allowed, got %s", out)
	}
	if _, open := gatestate.ReadRepair(dir); open {
		t.Fatal("re-delegating must close the open repair window")
	}
}

func TestStaleTriageFallsBackToDeny(t *testing.T) {
	s, dir := softState(t)
	_ = gatestate.WriteTriage(dir, gatestate.Triage{Decision: "solo", SoloFiles: []string{"util.py"}, At: time.Now().Add(-3 * time.Hour)})
	if out := preTool(t, s, editPayload("/repo/util.py", "tiny")); !strings.Contains(out, "deny") {
		t.Fatalf("stale triage must not open a solo window, got %s", out)
	}
}
