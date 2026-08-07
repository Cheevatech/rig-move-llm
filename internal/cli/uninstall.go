package cli

import (
	"bytes"
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
	global := fs.Bool("global", false, "uninstall the global scope (~/.rig-move-llm, plus anything an older rig left in ~/.claude)")
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
		if msg, err := service.New(self, home, dataDir).Uninstall(); err == nil && msg != "" {
			fmt.Println(msg)
		}

		// Reverse the user-scope MCP registration (global "follows you" wiring).
		if unregisterUserMCP() {
			fmt.Println("removed the worker MCP server from", userClaudeJSON())
		}
	}

	// Reverse a workspace-trust grant, and only one rig made (trust.go keeps the
	// ownership marker); a trust the human accepted themselves is not ours to
	// withdraw.
	if canon, err := config.CanonicalPath("."); err == nil {
		if revokeWorkspaceTrust(canon, dataDir) {
			fmt.Println("revoked the workspace trust rig granted for", canon)
		}
	}

	settingsPath := filepath.Join(claudeDir, "settings.json")
	backupPath := filepath.Join(dataDir, "settings.json.bak")
	restored := false
	if fileExists(backupPath) {
		if data, err := os.ReadFile(backupPath); err == nil {
			_ = os.WriteFile(settingsPath, data, 0o644)
			_ = os.Remove(backupPath)
			restored = true
			fmt.Println("restored", settingsPath, "from backup")
		}
	}
	// A backup is not evidence of a pre-rig state. `init` snapshots whatever is on
	// disk, so on a machine where rig was installed more than once the snapshot
	// already contains an older rig's hooks — and restoring it put them back, after
	// this command had just removed them, under the word "complete". Strip on both
	// paths: restore recovers the settings only the backup knows, the strip makes
	// sure nothing of ours survives it.
	if changed, err := stripRigSettings(settingsPath, !restored); err == nil && changed {
		fmt.Println("removed rig-move-llm settings from", settingsPath)
	}

	remove(filepath.Join(dataDir, "mcp.json"))

	// Reverse the auto-wire files (P10-A). Project-root .mcp.json is written only for
	// local scope; the delegate steer is removed only when it is ours (sentinel).
	if !*global {
		remove(filepath.Join(".", ".mcp.json"))
	}
	removeOwnedSteer(filepath.Join(claudeDir, "CLAUDE.md"))
	// /worker is the one file init writes outside rig's own dir, so uninstall owes
	// it a removal. Only if it still matches what init wrote: a user who edited it
	// owns their copy, and this command is not the place to discover that.
	removeIfUnchanged(filepath.Join(claudeDir, "commands", "worker.md"), workerCommandMD)
	removeOwnedSteer(filepath.Join(claudeDir, "commands", "qwen.md"))
	// The side file written when the user owns their own CLAUDE.md. Their @-import
	// line is left in place: it is one line in a file we do not own, and an import
	// of a missing file is inert.
	removeOwnedSteer(filepath.Join(claudeDir, steerImportFile))
	// prune the commands dir if ours was the only file in it
	_ = os.Remove(filepath.Join(claudeDir, "commands"))
	// init stopped writing these in S4, but uninstall must still remove them:
	// a machine that installed an older rig has them on disk, and this is the
	// only thing that ever cleans them up.
	remove(filepath.Join(claudeDir, "output-styles", "rig-delegate.md"))
	remove(filepath.Join(claudeDir, "output-styles", "rig-explore.md"))
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

// stripRigSettings removes what rig put in settings.json — hook entries whose
// command mentions rig-move-llm, our output style, our worker-tool permission —
// leaving everything the user added intact. Empty arrays/objects are pruned. It
// reports whether the file changed, so the caller says "removed" only when
// something was.
//
// ownsFile says the file exists because init created it, which is the only case
// where an ambiguous key (one a user could plausibly have set themselves) is
// safe to delete. After a restore from backup the file is the user's again, so
// only the unambiguous markers go.
func stripRigSettings(path string, ownsFile bool) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return false, err
	}
	if ownsFile {
		delete(settings, "enableAllProjectMcpServers")
	}
	if settings["outputStyle"] == "rig-delegate" || settings["outputStyle"] == "rig-explore" {
		delete(settings, "outputStyle")
	}
	// Remove only our worker-tool grant; user-managed permission rules stay.
	if perms, ok := settings["permissions"].(map[string]any); ok {
		if allow, ok := perms["allow"].([]any); ok {
			kept := make([]any, 0, len(allow))
			for _, v := range allow {
				if v != workerToolPermission {
					kept = append(kept, v)
				}
			}
			if len(kept) == 0 {
				delete(perms, "allow")
			} else {
				perms["allow"] = kept
			}
		}
		if len(perms) == 0 {
			delete(settings, "permissions")
		}
	}
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		return writeSettings(path, settings, data)
	}
	for phase, entriesAny := range hooks {
		entries, ok := entriesAny.([]any)
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
	return writeSettings(path, settings, data)
}

// writeSettings persists settings and reports whether that changed the file.
// The comparison is against the re-marshalled original rather than the bytes on
// disk, so a pure reformat (key order, indentation) does not get announced as a
// removal — the caller's line is about rig's entries, not whitespace.
func writeSettings(path string, settings map[string]any, before []byte) (bool, error) {
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return false, err
	}
	changed := true
	var orig map[string]any
	if json.Unmarshal(before, &orig) == nil {
		if canon, err := json.MarshalIndent(orig, "", "  "); err == nil {
			changed = !bytes.Equal(canon, out)
		}
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		return false, err
	}
	return changed, nil
}

// mentionsRig reports whether a hook entry contains a command
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
		if cmd, _ := hm["command"].(string); commandMentionsRig(cmd) {
			return true
		}
	}
	return false
}

// removeOwnedSteer deletes a file only if it carries our sentinel, so a user's own
// memory file or slash command is never touched. Both the current steer and the
// one older versions wrote carry the same marker, so this reverses either.
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

// removeIfUnchanged deletes path only when its contents are byte-identical to
// want. Anything else is the user's edit, and uninstall reverses rig's writes —
// not the user's.
func removeIfUnchanged(path, want string) {
	b, err := os.ReadFile(path)
	if err != nil || string(b) != want {
		return
	}
	_ = os.Remove(path)
}
