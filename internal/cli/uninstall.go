package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/internal/service"
)

// cmdUninstall reverses `init` for a scope: it restores the pre-init
// settings.json (from the backup taken at init time) or, failing that, strips the
// rig-move-llm hook entries; then removes the generated subagent and MCP toolbelt.
// --purge additionally deletes the scope data dir (config + logs + stats).
func cmdUninstall(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	global := fs.Bool("global", false, "uninstall the global scope (~/.claude + ~/.rig-move-llm)")
	purge := fs.Bool("purge", false, "also delete the data dir (config, logs, stats)")
	_ = fs.Parse(args)

	dataDir := config.LocalDir()
	claudeDir := filepath.Join(".", ".claude")
	if !*global {
		// Reverse init's allowlist registration (idempotent when absent).
		if canon, err := config.CanonicalPath("."); err == nil && config.ProjectAllowed(canon) {
			if err := config.UnregisterProject(canon); err == nil {
				fmt.Println("deregistered", canon, "from", config.ProjectsPath())
			}
		}
	}
	if *global {
		dataDir = config.GlobalDir()
		home, _ := os.UserHomeDir()
		claudeDir = filepath.Join(home, ".claude")

		// Reverse `init --service` (idempotent: a no-op when never installed).
		self, _ := os.Executable()
		if msg, err := service.New(self, home, dataDir).Uninstall(); err == nil {
			fmt.Println(msg)
		}

		// Reverse the user-scope MCP registration (global "follows you" wiring).
		unregisterUserMCP()
	}

	settingsPath := filepath.Join(claudeDir, "settings.json")
	backupPath := filepath.Join(dataDir, "settings.json.bak")
	if fileExists(backupPath) {
		if data, err := os.ReadFile(backupPath); err == nil {
			_ = os.WriteFile(settingsPath, data, 0o644)
			_ = os.Remove(backupPath)
			fmt.Println("restored", settingsPath, "from backup")
		}
	} else if err := stripRigHooks(settingsPath); err == nil {
		fmt.Println("removed rig-move-llm hooks from", settingsPath)
	}

	remove(filepath.Join(dataDir, "mcp.json"))

	// Reverse the auto-wire files (P10-A). Project-root .mcp.json is written only for
	// local scope; the delegate steer is removed only when it is ours (sentinel).
	if !*global {
		remove(filepath.Join(".", ".mcp.json"))
	}
	removeOwnedSteer(filepath.Join(claudeDir, "CLAUDE.md"))
	remove(filepath.Join(claudeDir, "output-styles", "rig-delegate.md"))
	// prune the output-styles dir if we left it empty
	_ = os.Remove(filepath.Join(claudeDir, "output-styles"))

	if *purge {
		if err := os.RemoveAll(dataDir); err == nil {
			fmt.Println("purged", dataDir)
		}
	}

	fmt.Println("uninstall complete")
	return 0
}

// stripRigHooks removes only the hook entries whose command mentions rig-move-llm,
// leaving any user-added hooks intact. Empty arrays/objects are pruned.
func stripRigHooks(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return err
	}
	// We reach stripRigHooks only when no pre-init backup exists (settings.json was
	// created fresh by init), so removing our injected keys is safe here.
	delete(settings, "enableAllProjectMcpServers")
	if settings["outputStyle"] == "rig-delegate" {
		delete(settings, "outputStyle")
	}
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		out, err := json.MarshalIndent(settings, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(path, append(out, '\n'), 0o644)
	}
	for _, phase := range []string{"PreToolUse", "PostToolUse", "SessionStart"} {
		entries, ok := hooks[phase].([]any)
		if !ok {
			continue
		}
		kept := make([]any, 0, len(entries))
		for _, e := range entries {
			if !mentionsRig(e) {
				kept = append(kept, e)
			}
		}
		if len(kept) == 0 {
			delete(hooks, phase)
		} else {
			hooks[phase] = kept
		}
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

// mentionsRig reports whether a PreToolUse/PostToolUse entry contains a command
// referencing rig-move-llm (i.e. one we installed).
func mentionsRig(entry any) bool {
	m, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	inner, ok := m["hooks"].([]any)
	if !ok {
		return false
	}
	for _, h := range inner {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if cmd, _ := hm["command"].(string); strings.Contains(cmd, "rig-move-llm") {
			return true
		}
	}
	return false
}

// removeOwnedSteer deletes the delegate-steer CLAUDE.md only if it carries our
// sentinel, so a user's own memory file is never touched.
func removeOwnedSteer(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if strings.Contains(string(data), steerSentinel) {
		remove(path)
	}
}

func remove(path string) {
	if err := os.Remove(path); err == nil {
		fmt.Println("removed", path)
	}
}
