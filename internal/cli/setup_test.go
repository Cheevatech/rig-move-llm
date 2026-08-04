package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/config"
)

// TestApplyInitGlobalFollowsYou asserts the global "follows you" wiring: a
// user-scope worker in ~/.claude.json, the tool-permission grant, and
// ENABLED=true. Since S4 there is no SessionStart hook to assert: every hook rig
// installed called `rig hook`, which no longer exists.
func TestApplyInitGlobalFollowsYou(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	rc := applyInit(initOpts{
		global: true, workerBase: "http://w:8000/v1", workerModel: "m",
		enabled: true, mainUpstream: "https://api.anthropic.com", port: "4000", force: true,
	})
	if rc != 0 {
		t.Fatalf("applyInit rc=%d", rc)
	}

	// user-scope MCP registration (loads in every project, no per-project .mcp.json)
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatalf("~/.claude.json not written: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("~/.claude.json invalid: %v", err)
	}
	servers, _ := root["mcpServers"].(map[string]any)
	if servers["worker"] == nil {
		t.Errorf("worker not registered at user scope: %s", data)
	}

	// The global settings carry the grant and NO hook.
	sdata, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if !strings.Contains(string(sdata), workerToolPermission) {
		t.Errorf("tool permission not granted at global scope: %s", sdata)
	}
	if strings.Contains(string(sdata), "hook") {
		t.Errorf("a global install still wires a hook; `rig hook` was deleted in S4: %s", sdata)
	}

	// global config carries ENABLED=true
	cdata, _ := os.ReadFile(filepath.Join(home, config.DirName, config.ConfigFile))
	if !strings.Contains(string(cdata), "ENABLED=true") {
		t.Errorf("ENABLED=true missing from global config: %s", cdata)
	}
}

// TestApplyInitSkippedWorkerIsInert asserts skipping the worker installs an
// inert config (ENABLED=false) so Claude Code runs normally.
func TestApplyInitSkippedWorkerIsInert(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if rc := applyInit(initOpts{global: true, enabled: false, mainUpstream: "https://api.anthropic.com", port: "4000", force: true}); rc != 0 {
		t.Fatalf("applyInit rc=%d", rc)
	}
	cdata, _ := os.ReadFile(filepath.Join(home, config.DirName, config.ConfigFile))
	if !strings.Contains(string(cdata), "ENABLED=false") {
		t.Errorf("skipped worker should yield ENABLED=false: %s", cdata)
	}
}

// TestSessionStartMaterializes asserts the SessionStart hook creates a per-project
// .rig-move-llm carrying the global settings, with the API key inherited (not
// duplicated) and the folder git-ignored — mirroring Serena's .serena.
// TestCCDefaultBaseIsOwnServeRoute covers #13 (map13 A4 MAJOR-2): the wizard's
// suggested cc base URL is this install's own /r/worker route, so choosing the
// cc engine is no longer a dead end for users without an external
// Anthropic-format endpoint.
func TestCCDefaultBaseIsOwnServeRoute(t *testing.T) {
	if got := ccDefaultBase("4010"); got != "http://localhost:4010/r/worker" {
		t.Errorf("ccDefaultBase(4010) = %q", got)
	}
	if got := ccDefaultBase(""); got != "http://localhost:4000/r/worker" {
		t.Errorf("ccDefaultBase(\"\") = %q", got)
	}
}
