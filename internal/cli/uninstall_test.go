package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// settingsWithRigHooks is what an enforcement-era rig left in settings.json: four
// hooks calling a subcommand that no longer exists, plus the output style whose
// file uninstall deletes on its way past.
const settingsWithRigHooks = `{
  "enableAllProjectMcpServers": true,
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "*",
        "hooks": [{"type": "command", "command": "rig-move-llm hook pre-tool"}]
      },
      {
        "matcher": "Bash",
        "hooks": [{"type": "command", "command": "my-own-linter"}]
      }
    ],
    "SessionStart": [
      {
        "matcher": "startup|resume",
        "hooks": [{"type": "command", "command": "npx -y rig-move-llm hook session-start"}]
      }
    ]
  },
  "outputStyle": "rig-delegate",
  "theme": "dark"
}`

// TestUninstallDoesNotRestoreAnOlderRigsHooks pins the bug that shipped in 0.8.0
// and was found on a real machine: `uninstall` printed "restored settings.json
// from backup" followed by "uninstall complete", and the file it restored was
// itself written by an older rig. Four dead hooks and a dead output style came
// back, pointing at `rig hook` — a subcommand 0.8 deleted — so every tool call in
// that project fired a failing hook afterwards.
//
// A backup only proves what was on disk when init ran. It does not prove that
// nothing of rig's was in it.
func TestUninstallDoesNotRestoreAnOlderRigsHooks(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	backup := filepath.Join(dir, "settings.json.bak")

	// The state on disk at uninstall time: current settings, and a backup that a
	// previous generation of rig had already contaminated.
	if err := os.WriteFile(settings, []byte(`{"theme":"dark"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup, []byte(settingsWithRigHooks), 0o644); err != nil {
		t.Fatal(err)
	}

	// Exactly what cmdUninstall does with those two paths.
	data, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, data, 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := stripRigSettings(settings, false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("strip reported no change while restoring a backup full of rig hooks")
	}

	body, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "rig-move-llm") {
		t.Errorf("uninstall left a hook invoking rig behind:\n%s", body)
	}
	if strings.Contains(string(body), "rig-delegate") {
		t.Errorf("uninstall left outputStyle pointing at a file it deleted:\n%s", body)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	// The backup's whole reason to exist: settings only it knows must survive.
	if got["theme"] != "dark" {
		t.Errorf("restore lost the user's own settings: %v", got)
	}
	// ...including a hook the user wrote, which is not ours to remove.
	hooks, _ := got["hooks"].(map[string]any)
	pre, _ := hooks["PreToolUse"].([]any)
	if len(pre) != 1 {
		t.Fatalf("want the user's one non-rig hook kept, got %v", pre)
	}
	if !strings.Contains(string(mustJSON(t, pre[0])), "my-own-linter") {
		t.Errorf("the hook that survived is not the user's: %s", mustJSON(t, pre[0]))
	}
	// An ambiguous key is not ours to delete once the file is the user's again.
	if _, ok := got["enableAllProjectMcpServers"]; !ok {
		t.Error("restored file lost enableAllProjectMcpServers, which a user may have set themselves")
	}
}

// TestStripRigSettingsOwnsFileRemovesAmbiguousKeys covers the other path: no
// backup means init created the file, so the keys a user could also have set are
// ours to take back.
func TestStripRigSettingsOwnsFileRemovesAmbiguousKeys(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(settings, []byte(settingsWithRigHooks), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := stripRigSettings(settings, true); err != nil {
		t.Fatal(err)
	}

	body, _ := os.ReadFile(settings)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["enableAllProjectMcpServers"]; ok {
		t.Errorf("init created this file, so its own key should be gone:\n%s", body)
	}
	if got["theme"] != "dark" {
		t.Errorf("stripped a key that was never ours:\n%s", body)
	}
}

// TestStripRigSettingsReportsNoChangeWhenClean pins the line uninstall prints.
// It used to announce "removed rig-move-llm hooks" on any settings.json it could
// parse, including one that had never seen rig — the same shape of untrue
// success line as the bug above, just cheaper.
func TestStripRigSettingsReportsNoChangeWhenClean(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	clean := `{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [{"type": "command", "command": "my-own-linter"}]
      }
    ]
  },
  "theme": "dark"
}`
	if err := os.WriteFile(settings, []byte(clean), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := stripRigSettings(settings, true)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("reported a removal from a settings.json that contains nothing of rig's")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
