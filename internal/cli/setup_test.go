package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/config"
)

// TestApplyInitGlobalFollowsYou asserts the global "follows you" wiring: a
// user-scope worker in ~/.claude.json, a SessionStart hook, and ENABLED=true.
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

	// SessionStart hook (the Serena-like auto-materialize trigger)
	sdata, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if !strings.Contains(string(sdata), "SessionStart") || !strings.Contains(string(sdata), "session-start") {
		t.Errorf("SessionStart hook not wired: %s", sdata)
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
func TestSessionStartMaterializes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	gdir := filepath.Join(home, config.DirName)
	if err := os.MkdirAll(gdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gdir, config.ConfigFile),
		[]byte("WORKER_API_BASE=http://w:8000/v1\nWORKER_API_KEY=secret\nENABLED=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	proj := t.TempDir()
	t.Setenv("CLAUDE_PROJECT_DIR", proj)

	if rc := cmdSessionStart(strings.NewReader("{}"), io.Discard); rc != 0 {
		t.Fatalf("cmdSessionStart rc=%d", rc)
	}

	local := filepath.Join(proj, config.DirName, config.ConfigFile)
	data, err := os.ReadFile(local)
	if err != nil {
		t.Fatalf("per-project config not materialized: %v", err)
	}
	// inherit-not-copy (the Serena model): the marker carries NO settings, so a
	// global change propagates. It must not duplicate worker settings or the key.
	if strings.Contains(string(data), "\nWORKER_API_BASE=") ||
		strings.Contains(string(data), "\nENABLED=") ||
		strings.Contains(string(data), "secret") {
		t.Errorf("materialized config must inherit, not copy settings, got: %s", data)
	}
	// everything resolves from the global scope...
	cfg := config.LoadFrom(proj)
	if cfg.WorkerAPIKey != "secret" {
		t.Errorf("key should inherit global, got %q", cfg.WorkerAPIKey)
	}
	if cfg.WorkerAPIBase != "http://w:8000/v1" {
		t.Errorf("base should inherit global, got %q", cfg.WorkerAPIBase)
	}
	if !cfg.Enabled {
		t.Error("ENABLED should inherit global true")
	}
	// ...but the project owns its own data dir (stats/logs)
	if want := filepath.Join(proj, config.DirName); cfg.DataDir != want {
		t.Errorf("dataDir should be project-local %q, got %q", want, cfg.DataDir)
	}
	if _, err := os.Stat(filepath.Join(proj, config.DirName, ".gitignore")); err != nil {
		t.Errorf(".gitignore not written: %v", err)
	}

	// The conflict fix: flipping the GLOBAL switch propagates to the materialized
	// project (it inherits, it did not copy).
	if err := os.WriteFile(filepath.Join(gdir, config.ConfigFile),
		[]byte("WORKER_API_BASE=http://w:8000/v1\nWORKER_API_KEY=secret\nENABLED=false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if config.LoadFrom(proj).Enabled {
		t.Error("global ENABLED=false must propagate to the materialized project (inherit, not copy)")
	}

	// idempotent: a second session must not clobber or error
	if rc := cmdSessionStart(strings.NewReader("{}"), io.Discard); rc != 0 {
		t.Errorf("second cmdSessionStart rc=%d", rc)
	}
}
