package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInitAutoWire verifies P10-A: `init` writes the persistent files a bare
// `claude` auto-loads — project-root .mcp.json, the enableAllProjectMcpServers
// pre-approve in settings.json, and the delegate-steer CLAUDE.md — and that
// `uninstall` reverses them.
func TestInitAutoWire(t *testing.T) {
	home := t.TempDir()
	proj := t.TempDir()
	t.Setenv("HOME", home)

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(proj); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	if rc := cmdInit([]string{"--no-detect", "--backend", "ollama",
		"--worker-base", "http://localhost:11434/v1", "--worker-model", "m"}); rc != 0 {
		t.Fatalf("cmdInit rc=%d", rc)
	}

	// 1. project-root .mcp.json auto-discovered by bare claude
	rootMCP := filepath.Join(proj, ".mcp.json")
	data, err := os.ReadFile(rootMCP)
	if err != nil {
		t.Fatalf("root .mcp.json missing: %v", err)
	}
	var mcp struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &mcp); err != nil {
		t.Fatalf("root .mcp.json invalid: %v", err)
	}
	if _, ok := mcp.MCPServers["worker"]; !ok {
		t.Errorf("root .mcp.json missing worker server: %s", data)
	}

	// 2. enableAllProjectMcpServers pre-approve in settings.json
	sData, err := os.ReadFile(filepath.Join(proj, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(sData, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["enableAllProjectMcpServers"] != true {
		t.Errorf("enableAllProjectMcpServers not set: %v", settings["enableAllProjectMcpServers"])
	}

	// 2c. tool-use permission granted (#6): trust alone still stalls headless -p
	if !permissionAllowed(settings, workerToolPermission) {
		t.Errorf("permissions.allow missing %s: %s", workerToolPermission, sData)
	}

	// 2a. UserPromptSubmit health-check hook wired (zero-token worker liveness probe)
	if !strings.Contains(string(sData), "UserPromptSubmit") ||
		!strings.Contains(string(sData), "rig-move-llm hook user-prompt") {
		t.Errorf("settings.json missing UserPromptSubmit health-check hook: %s", sData)
	}

	// 2b. output style file + activation key (the savings-recovery lever, P10-C)
	if settings["outputStyle"] != "rig-delegate" {
		t.Errorf("outputStyle not set to rig-delegate: %v", settings["outputStyle"])
	}
	style, err := os.ReadFile(filepath.Join(proj, ".claude", "output-styles", "rig-delegate.md"))
	if err != nil {
		t.Fatalf("output style missing: %v", err)
	}
	if !strings.Contains(string(style), "name: rig-delegate") {
		t.Error("output style missing name frontmatter")
	}
	if !strings.Contains(string(style), "mcp__worker__implement") {
		t.Error("output style missing delegate tool name")
	}

	// 3. delegate-steer CLAUDE.md with sentinel + tool name
	mem, err := os.ReadFile(filepath.Join(proj, ".claude", "CLAUDE.md"))
	if err != nil {
		t.Fatalf("CLAUDE.md missing: %v", err)
	}
	if !strings.Contains(string(mem), steerSentinel) {
		t.Error("CLAUDE.md missing sentinel")
	}
	if !strings.Contains(string(mem), "mcp__worker__implement") {
		t.Error("CLAUDE.md missing delegate tool name")
	}

	// Inject a user hook to verify uninstall preserves it
	{
		sPath := filepath.Join(proj, ".claude", "settings.json")
		sRaw, _ := os.ReadFile(sPath)
		var sInject map[string]any
		json.Unmarshal(sRaw, &sInject)
		hooksInject := sInject["hooks"].(map[string]any)
		userHook := map[string]any{"hooks": []any{map[string]any{"command": "echo user-health-check"}}}
		hooksInject["UserPromptSubmit"] = append(hooksInject["UserPromptSubmit"].([]any), userHook)
		sOut, _ := json.MarshalIndent(sInject, "", "  ")
		os.WriteFile(sPath, append(sOut, '\n'), 0o644)
	}

	// uninstall reverses all three
	if rc := cmdUninstall(nil); rc != 0 {
		t.Fatalf("cmdUninstall rc=%d", rc)
	}
	if _, err := os.Stat(rootMCP); !os.IsNotExist(err) {
		t.Error("root .mcp.json not removed by uninstall")
	}
	if _, err := os.Stat(filepath.Join(proj, ".claude", "CLAUDE.md")); !os.IsNotExist(err) {
		t.Error("delegate-steer CLAUDE.md not removed by uninstall")
	}
	if _, err := os.Stat(filepath.Join(proj, ".claude", "output-styles", "rig-delegate.md")); !os.IsNotExist(err) {
		t.Error("output style not removed by uninstall")
	}
	// settings.json created fresh by init → injected keys stripped (no backup path)
	sData2, err := os.ReadFile(filepath.Join(proj, ".claude", "settings.json"))
	if err == nil {
		var s2 map[string]any
		_ = json.Unmarshal(sData2, &s2)
		if _, ok := s2["enableAllProjectMcpServers"]; ok {
			t.Error("enableAllProjectMcpServers not stripped by uninstall")
		}
		if _, ok := s2["outputStyle"]; ok {
			t.Error("outputStyle not stripped by uninstall")
		}
		if _, ok := s2["permissions"]; ok {
			t.Errorf("permissions grant not stripped by uninstall: %s", sData2)
		}
		// No rig hook remains in any phase
		if hooks2, ok := s2["hooks"].(map[string]any); ok {
			for phaseName, phaseEntries := range hooks2 {
				for _, entry := range phaseEntries.([]any) {
					if eMap, ok := entry.(map[string]any); ok {
						if inner, ok := eMap["hooks"].([]any); ok {
							for _, h := range inner {
								if hm, ok := h.(map[string]any); ok {
									if cmd, ok := hm["command"].(string); ok {
										if strings.Contains(cmd, "rig-move-llm") {
											t.Errorf("rig hook still present in phase %s: %s", phaseName, cmd)
										}
									}
								}
							}
						}
					}
				}
			}
		}
		// User-added hook survives uninstall
		if hooks2, ok := s2["hooks"].(map[string]any); ok {
			found := false
			if phaseEntries, ok := hooks2["UserPromptSubmit"].([]any); ok {
				for _, entry := range phaseEntries {
					if eMap, ok := entry.(map[string]any); ok {
						if inner, ok := eMap["hooks"].([]any); ok {
							for _, h := range inner {
								if hm, ok := h.(map[string]any); ok {
									if cmd, ok := hm["command"].(string); ok {
										if cmd == "echo user-health-check" {
											found = true
										}
									}
								}
							}
						}
					}
				}
			}
			if !found {
				t.Errorf("user hook not preserved after uninstall: %s", sData2)
			}
		} else {
			t.Error("user hook not preserved: hooks map missing after uninstall")
		}
	}
}

// permissionAllowed reports whether settings carries rule in permissions.allow.
func permissionAllowed(settings map[string]any, rule string) bool {
	perms, _ := settings["permissions"].(map[string]any)
	allow, _ := perms["allow"].([]any)
	for _, v := range allow {
		if v == rule {
			return true
		}
	}
	return false
}

// TestWireSettingsPreservesUserPermissions verifies the #6 grant merges into an
// existing permissions block instead of clobbering it, is idempotent, and that
// uninstall strips only our rule.
func TestWireSettingsPreservesUserPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	seed := `{"permissions":{"allow":["Bash(ls:*)"],"deny":["WebFetch"]}}`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ { // twice: the grant must not duplicate
		if err := wireSettings(path, filepath.Join(dir, "bak.json"), "", false); err != nil {
			t.Fatalf("wireSettings: %v", err)
		}
	}
	data, _ := os.ReadFile(path)
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	if !permissionAllowed(settings, workerToolPermission) {
		t.Errorf("worker grant missing: %s", data)
	}
	if !permissionAllowed(settings, "Bash(ls:*)") {
		t.Errorf("user allow rule lost: %s", data)
	}
	perms := settings["permissions"].(map[string]any)
	if allow := perms["allow"].([]any); len(allow) != 2 {
		t.Errorf("grant not idempotent, allow=%v", allow)
	}
	if _, ok := perms["deny"]; !ok {
		t.Errorf("user deny rule lost: %s", data)
	}

	if err := stripRigHooks(path); err != nil {
		t.Fatalf("stripRigHooks: %v", err)
	}
	data2, _ := os.ReadFile(path)
	var s2 map[string]any
	if err := json.Unmarshal(data2, &s2); err != nil {
		t.Fatal(err)
	}
	if permissionAllowed(s2, workerToolPermission) {
		t.Errorf("worker grant not stripped: %s", data2)
	}
	if !permissionAllowed(s2, "Bash(ls:*)") {
		t.Errorf("uninstall removed user allow rule: %s", data2)
	}
}

// TestInitNpxHookForm verifies init writes the correct hook command form
// depending on the --npx flag: bare `rig-move-llm hook <name>` without --npx,
// and `npx -y rig-move-llm hook <name>` with --npx.
func TestInitNpxHookForm(t *testing.T) {
	// Helper to read and return hook commands from settings.json
	readHookCommands := func(dir string) map[string]string {
		data, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
		if err != nil {
			t.Helper()
			t.Fatalf("settings.json missing: %v", err)
		}
		var settings map[string]any
		if err := json.Unmarshal(data, &settings); err != nil {
			t.Helper()
			t.Fatalf("settings.json invalid: %v", err)
		}
		hooks := settings["hooks"].(map[string]any)
		cmds := map[string]string{}
		for name, arr := range hooks {
			hookList := arr.([]any)
			entry := hookList[0].(map[string]any)
			hooksInner := entry["hooks"].([]any)
			hookEntry := hooksInner[0].(map[string]any)
			cmds[name] = hookEntry["command"].(string)
		}
		return cmds
	}

	wd, _ := os.Getwd()

	// --- Without --npx: bare form ---
	t.Run("bare", func(t *testing.T) {
		home := t.TempDir()
		proj := t.TempDir()
		t.Setenv("HOME", home)
		if err := os.Chdir(proj); err != nil {
			t.Fatal(err)
		}
		defer os.Chdir(wd)

		if rc := cmdInit([]string{"--no-detect", "--backend", "ollama",
			"--worker-base", "http://localhost:11434/v1", "--worker-model", "m"}); rc != 0 {
			t.Fatalf("cmdInit rc=%d", rc)
		}

		cmds := readHookCommands(proj)
		want := map[string]string{
			"PreToolUse":       "rig-move-llm hook pre-tool",
			"PostToolUse":      "rig-move-llm hook post-tool",
			"SessionStart":     "rig-move-llm hook session-start",
			"UserPromptSubmit": "rig-move-llm hook user-prompt",
		}
		for name, expected := range want {
			if cmds[name] != expected {
				t.Errorf("hook %s: got %q, want %q", name, cmds[name], expected)
			}
		}
	})

	// --- With --npx: npx form ---
	t.Run("npx", func(t *testing.T) {
		home := t.TempDir()
		proj := t.TempDir()
		t.Setenv("HOME", home)
		if err := os.Chdir(proj); err != nil {
			t.Fatal(err)
		}
		defer os.Chdir(wd)

		if rc := cmdInit([]string{"--no-detect", "--backend", "ollama",
			"--worker-base", "http://localhost:11434/v1", "--worker-model", "m",
			"--npx"}); rc != 0 {
			t.Fatalf("cmdInit rc=%d", rc)
		}

		cmds := readHookCommands(proj)
		want := map[string]string{
			"PreToolUse":       "npx -y rig-move-llm hook pre-tool",
			"PostToolUse":      "npx -y rig-move-llm hook post-tool",
			"SessionStart":     "npx -y rig-move-llm hook session-start",
			"UserPromptSubmit": "npx -y rig-move-llm hook user-prompt",
		}
		for name, expected := range want {
			if cmds[name] != expected {
				t.Errorf("hook %s: got %q, want %q", name, cmds[name], expected)
			}
		}
	})
}

// TestInitPreservesUserCLAUDEmd verifies init never clobbers a user's own
// CLAUDE.md and uninstall leaves it intact.
func TestInitPreservesUserCLAUDEmd(t *testing.T) {
	home := t.TempDir()
	proj := t.TempDir()
	t.Setenv("HOME", home)

	wd, _ := os.Getwd()
	if err := os.Chdir(proj); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	userMem := "# My own memory\nkeep me\n"
	memPath := filepath.Join(proj, ".claude", "CLAUDE.md")
	if err := os.WriteFile(memPath, []byte(userMem), 0o644); err != nil {
		t.Fatal(err)
	}

	if rc := cmdInit([]string{"--no-detect", "--backend", "ollama"}); rc != 0 {
		t.Fatalf("cmdInit rc=%d", rc)
	}
	got, _ := os.ReadFile(memPath)
	if string(got) != userMem {
		t.Errorf("user CLAUDE.md was modified:\n%s", got)
	}

	cmdUninstall(nil)
	got2, err := os.ReadFile(memPath)
	if err != nil || string(got2) != userMem {
		t.Errorf("user CLAUDE.md removed/modified by uninstall: err=%v content=%q", err, got2)
	}
}

// #56 reported that `uninstall` leaves an empty project entry behind in
// ~/.claude.json — proven on v0.7.5, on a clone under a live dogfood HOME.
//
// It does not reproduce. revokeWorkspaceTrust has deleted an entry that carried
// nothing but rig's flag since v0.7.0 (369a967), which is BEFORE the version the
// report was filed against, and the unit tests in trust_test.go pin that. What
// was missing is a test of the path the report actually walked: the whole
// init --trust-workspace / uninstall cycle through the commands, not the helper.
// So the residue came from something other than rig forgetting to delete it —
// most plausibly a Claude Code process alive in that HOME rewriting .claude.json
// from its own state after uninstall, which is a race rig cannot win and should
// not try to.
//
// This test is the pin: end to end, through the commands, the entry goes.
func TestUninstallLeavesNoEmptyProjectEntry(t *testing.T) {
	home := t.TempDir()
	proj := t.TempDir()
	t.Setenv("HOME", home)

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(proj); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	// A .claude.json that already has another project in it: the entry rig makes
	// must go, and the neighbour must not.
	other := "/somewhere/else"
	seed, _ := json.Marshal(map[string]any{"projects": map[string]any{
		other: map[string]any{"history": []any{"a prompt"}},
	}})
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), seed, 0o644); err != nil {
		t.Fatal(err)
	}

	if rc := cmdInit([]string{"--no-detect", "--trust-workspace", "--backend", "ollama",
		"--worker-base", "http://localhost:11434/v1", "--worker-model", "m"}); rc != 0 {
		t.Fatalf("cmdInit rc=%d", rc)
	}

	canon, _ := filepath.EvalSymlinks(proj)
	projects := readProjects(t)
	entry, ok := projects[canon].(map[string]any)
	if !ok || entry["hasTrustDialogAccepted"] != true {
		t.Fatalf("init --trust-workspace did not grant trust for %s: %v", canon, projects)
	}

	if rc := cmdUninstall(nil); rc != 0 {
		t.Fatalf("cmdUninstall rc=%d", rc)
	}

	projects = readProjects(t)
	if e, still := projects[canon]; still {
		t.Errorf("uninstall left a project entry behind (#56): %v", e)
	}
	if n, ok := projects[other].(map[string]any); !ok || len(n["history"].([]any)) != 1 {
		t.Errorf("uninstall touched a project that was not rig's: %v", projects[other])
	}
}
