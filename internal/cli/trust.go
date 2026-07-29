package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Workspace trust is the third, previously uncovered layer of Claude Code's
// gating (#16). Init already covers the other two — enableAllProjectMcpServers
// grants SERVER trust, and the permissions.allow entry grants TOOL permission —
// but on a project that has never been opened interactively CC discards the
// permission entry entirely:
//
//	Ignoring 1 permissions.allow entry from .claude/settings.json: this workspace
//	has not been trusted. Run Claude Code interactively here once and accept the
//	trust dialog, or set projects["<repo>"].hasTrustDialogAccepted: true in
//	~/.claude.json
//
// So a headless `claude -p` on a fresh clone still burns its run on the
// permission wall. Rig can set that flag — it is the user's own file, and init
// already writes to it for the global MCP registration — but doing it silently
// would switch off a safety net that is not ours: the dialog exists so a repo
// you just cloned cannot act on you before you have looked at it. Hence the
// rule: the grant happens only on an explicit, per-run request (--trust-workspace,
// or the wizard's prompt), it is announced, and it is reversible by uninstall —
// which reverts ONLY a flag rig set, never one the human accepted themselves.
//
// trustMarkerFile records that ownership, in rig's own data dir rather than in
// the user's ~/.claude.json.
const trustMarkerFile = "workspace_trust.json"

// trustMarker is what rig remembers about a grant it made.
type trustMarker struct {
	Path string    `json:"path"`
	At   time.Time `json:"at"`
}

// workspaceTrusted reports whether ~/.claude.json already records the trust
// dialog as accepted for this canonical path — whether by rig or by the human.
func workspaceTrusted(canonical string) bool {
	root, err := readClaudeJSON()
	if err != nil {
		return false
	}
	projects, _ := root["projects"].(map[string]any)
	entry, _ := projects[canonical].(map[string]any)
	accepted, _ := entry["hasTrustDialogAccepted"].(bool)
	return accepted
}

// grantWorkspaceTrust sets projects[canonical].hasTrustDialogAccepted in
// ~/.claude.json, preserving every other key and project, and records rig's
// ownership of the grant in dataDir. It reports whether the flag was already
// set (in which case nothing is written and nothing is owned).
func grantWorkspaceTrust(canonical, dataDir string) (already bool, err error) {
	if workspaceTrusted(canonical) {
		return true, nil
	}
	root, err := readClaudeJSON()
	if err != nil {
		return false, err
	}
	projects, _ := root["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
	}
	entry, _ := projects[canonical].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
	}
	entry["hasTrustDialogAccepted"] = true
	projects[canonical] = entry
	root["projects"] = projects
	if err := writeClaudeJSON(root); err != nil {
		return false, err
	}
	return false, writeTrustMarker(dataDir, canonical)
}

// revokeWorkspaceTrust reverses a grant rig made, and ONLY that: with no marker
// (or a marker for a different path) the flag is left exactly as it is, because
// it is then either the human's own acceptance or someone else's business.
func revokeWorkspaceTrust(canonical, dataDir string) (revoked bool) {
	m, ok := readTrustMarker(dataDir)
	if !ok || m.Path != canonical {
		return false
	}
	defer os.Remove(filepath.Join(dataDir, trustMarkerFile))

	root, err := readClaudeJSON()
	if err != nil {
		return false
	}
	projects, _ := root["projects"].(map[string]any)
	entry, _ := projects[canonical].(map[string]any)
	if entry == nil {
		return false
	}
	delete(entry, "hasTrustDialogAccepted")
	// An entry that existed only to carry rig's flag goes with it; anything else
	// the user has under this project stays.
	if len(entry) == 0 {
		delete(projects, canonical)
	} else {
		projects[canonical] = entry
	}
	if len(projects) == 0 {
		delete(root, "projects")
	}
	return writeClaudeJSON(root) == nil
}

// untrustedWorkspaceNotice is what init prints when it did NOT grant trust and
// the workspace is not trusted: the failure it predicts, and both ways out. It
// returns "" when there is nothing to warn about.
func untrustedWorkspaceNotice(canonical string) string {
	if workspaceTrusted(canonical) {
		return ""
	}
	return fmt.Sprintf(
		"NOTE: this workspace is not trusted by Claude Code yet, so it will IGNORE the "+
			"permissions.allow entry init just wrote and a headless `claude -p` run here will stop "+
			"to ask a human for approval.\n"+
			"  Fix it either way:\n"+
			"    - run `claude` here once interactively and accept the trust dialog, or\n"+
			"    - re-run `rig-move-llm init --trust-workspace` (sets projects[%q].hasTrustDialogAccepted "+
			"in %s; `uninstall` reverts it).", canonical, userClaudeJSON())
}

func readClaudeJSON() (map[string]any, error) {
	root := map[string]any{}
	data, err := os.ReadFile(userClaudeJSON())
	if err != nil {
		if os.IsNotExist(err) {
			return root, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return root, nil
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", userClaudeJSON(), err)
	}
	return root, nil
}

func writeClaudeJSON(root map[string]any) error {
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(userClaudeJSON(), append(out, '\n'), 0o644)
}

func writeTrustMarker(dataDir, canonical string) error {
	b, err := json.Marshal(trustMarker{Path: canonical, At: time.Now()})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, trustMarkerFile), b, 0o644)
}

func readTrustMarker(dataDir string) (trustMarker, bool) {
	var m trustMarker
	b, err := os.ReadFile(filepath.Join(dataDir, trustMarkerFile))
	if err != nil || json.Unmarshal(b, &m) != nil {
		return m, false
	}
	return m, m.Path != ""
}
