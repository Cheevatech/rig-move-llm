package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/thin"
)

// TestInitAutoWire verifies P10-A as it stands after S4: `init` writes the
// persistent files a bare `claude` auto-loads — project-root .mcp.json pointing
// at the switch, and the enableAllProjectMcpServers + tool-permission grant in
// settings.json — writes NO hooks and NO output style, and `uninstall` reverses
// it. The steer itself is S3's to write.
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

	// 1. project-root .mcp.json auto-discovered by bare claude, pointing at the
	//    switch — not at `worker`, which no longer exists as a subcommand.
	rootMCP := filepath.Join(proj, ".mcp.json")
	data, err := os.ReadFile(rootMCP)
	if err != nil {
		t.Fatalf("root .mcp.json missing: %v", err)
	}
	var mcp struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
			Timeout int      `json:"timeout"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &mcp); err != nil {
		t.Fatalf("root .mcp.json invalid: %v", err)
	}
	srv, ok := mcp.MCPServers["worker"]
	if !ok {
		t.Fatalf("root .mcp.json missing the worker server: %s", data)
	}
	if !slices.Contains(srv.Args, "thin-worker") {
		t.Errorf("the MCP entry does not launch thin-worker: %v", srv.Args)
	}
	for _, dead := range srv.Args {
		if dead == "worker" {
			t.Error("the MCP entry still launches `rig worker`, a subcommand deleted in S4")
		}
	}
	if srv.Timeout <= int(thin.WallCeiling()/time.Millisecond) {
		t.Errorf("client timeout %dms must sit above the wall ceiling, or the client aborts before the run can report", srv.Timeout)
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

	// 2a. THE S4 GUARANTEE: init writes no hooks at all. Every hook rig used to
	// install called `rig hook <phase>`, a subcommand deleted with the contract
	// layer — writing one now would produce an install that fails on every tool
	// call, silently, in a way only the user would ever see.
	if _, ok := settings["hooks"]; ok {
		t.Errorf("init wrote a hooks block; `rig hook` no longer exists: %s", sData)
	}
	if strings.Contains(string(sData), "hook") {
		t.Errorf("settings.json still mentions a hook; `rig hook` no longer exists: %s", sData)
	}
	// 2b. and no output style: the one it used to activate told MAIN its edit
	// tools were "blocked for you by design (a PreToolUse hook denies them)".
	if _, ok := settings["outputStyle"]; ok {
		t.Errorf("init still activates an output style describing enforcement that was deleted: %s", sData)
	}
	if _, err := os.Stat(filepath.Join(proj, ".claude", "output-styles", "rig-delegate.md")); !os.IsNotExist(err) {
		t.Error("init still writes the rig-delegate output style")
	}

	// 3. uninstall reverses what init did
	if rc := cmdUninstall(nil); rc != 0 {
		t.Fatalf("cmdUninstall rc=%d", rc)
	}
	if _, err := os.Stat(rootMCP); !os.IsNotExist(err) {
		t.Error("root .mcp.json not removed by uninstall")
	}
	sData2, err := os.ReadFile(filepath.Join(proj, ".claude", "settings.json"))
	if err == nil {
		var s2 map[string]any
		_ = json.Unmarshal(sData2, &s2)
		if _, ok := s2["enableAllProjectMcpServers"]; ok {
			t.Error("enableAllProjectMcpServers not stripped by uninstall")
		}
		if _, ok := s2["permissions"]; ok {
			t.Errorf("permissions grant not stripped by uninstall: %s", sData2)
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
		if err := wireSettings(path, filepath.Join(dir, "bak.json")); err != nil {
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
