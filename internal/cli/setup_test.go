package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/config"
)

// TestApplyInitWritesConfigAndNothingElse locks in what a global install is now:
// config, and nothing that points Claude Code at a tool. The MCP registration, the
// tool-permission grant and the steer all existed to hand work to a worker
// subprocess; the switch swaps the model underneath one session instead, so an
// install that still wrote them would be wiring up a thing that is not there.
func TestApplyInitWritesConfigAndNothingElse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	rc := applyInit(initOpts{
		global: true, workerBase: "http://w:8000/v1", workerModel: "m",
		enabled: true, mainUpstream: "https://api.anthropic.com", port: "4000", force: true,
	})
	if rc != 0 {
		t.Fatalf("applyInit rc=%d", rc)
	}

	cdata, _ := os.ReadFile(filepath.Join(home, config.DirName, config.ConfigFile))
	if !strings.Contains(string(cdata), "ENABLED=true") {
		t.Errorf("ENABLED=true missing from global config: %s", cdata)
	}

	if data, err := os.ReadFile(filepath.Join(home, ".claude.json")); err == nil {
		var root map[string]any
		_ = json.Unmarshal(data, &root)
		if servers, _ := root["mcpServers"].(map[string]any); servers["worker"] != nil {
			t.Errorf("install still registers a worker MCP server; there is no worker: %s", data)
		}
	}

	sdata, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	for _, gone := range []string{"mcp__worker__implement", "enableAllProjectMcpServers", "hook"} {
		if strings.Contains(string(sdata), gone) {
			t.Errorf("settings.json still carries %q from the delegate era: %s", gone, sdata)
		}
	}

	for _, p := range []string{
		filepath.Join(home, ".claude", "CLAUDE.md"),
		filepath.Join(home, ".claude", steerImportFile),
		filepath.Join(home, ".claude", "commands", "qwen.md"),
	} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("install still writes %s; nothing is being steered anywhere", p)
		}
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
