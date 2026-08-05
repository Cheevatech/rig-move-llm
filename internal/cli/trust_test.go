package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// trustEnv points HOME at a scratch dir and returns (home, dataDir, canonical
// project path) for a workspace under it.
func trustEnv(t *testing.T) (home, dataDir, canonical string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	dataDir = filepath.Join(home, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return home, dataDir, filepath.Join(home, "repo")
}

func readProjects(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile(userClaudeJSON())
	if err != nil {
		return nil
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatalf("~/.claude.json is not valid JSON: %v", err)
	}
	p, _ := root["projects"].(map[string]any)
	return p
}

func TestGrantWorkspaceTrust(t *testing.T) {
	_, dataDir, canon := trustEnv(t)

	if workspaceTrusted(canon) {
		t.Fatal("a fresh workspace must not read as trusted")
	}
	already, err := grantWorkspaceTrust(canon, dataDir)
	if err != nil || already {
		t.Fatalf("grant: already=%v err=%v", already, err)
	}
	if !workspaceTrusted(canon) {
		t.Error("the grant did not take")
	}

	// Second grant is a no-op that reports itself as one.
	already, err = grantWorkspaceTrust(canon, dataDir)
	if err != nil || !already {
		t.Fatalf("re-grant: already=%v err=%v", already, err)
	}
}

// ~/.claude.json is the user's file and carries their whole CC state. A grant
// must be a surgical merge, never a rewrite.
func TestGrantWorkspaceTrustPreservesTheUsersFile(t *testing.T) {
	_, dataDir, canon := trustEnv(t)
	other := filepath.Join(t.TempDir(), "other-repo")

	seed := map[string]any{
		"numStartups": float64(7),
		"mcpServers":  map[string]any{"serena": map[string]any{"command": "uvx"}},
		"projects": map[string]any{
			other: map[string]any{"hasTrustDialogAccepted": true, "history": []any{"a"}},
		},
	}
	b, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(userClaudeJSON(), b, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := grantWorkspaceTrust(canon, dataDir); err != nil {
		t.Fatal(err)
	}

	var root map[string]any
	raw, _ := os.ReadFile(userClaudeJSON())
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	if root["numStartups"] != float64(7) {
		t.Error("unrelated top-level key was lost")
	}
	if _, ok := root["mcpServers"].(map[string]any)["serena"]; !ok {
		t.Error("the user's MCP servers were lost")
	}
	if !workspaceTrusted(other) {
		t.Error("another project's trust was lost")
	}
	if !workspaceTrusted(canon) {
		t.Error("the new grant is missing")
	}
}

// Uninstall reverts what rig set, and nothing else.
func TestRevokeWorkspaceTrust(t *testing.T) {
	t.Run("reverts rig's own grant", func(t *testing.T) {
		_, dataDir, canon := trustEnv(t)
		if _, err := grantWorkspaceTrust(canon, dataDir); err != nil {
			t.Fatal(err)
		}
		if !revokeWorkspaceTrust(canon, dataDir) {
			t.Fatal("revoke reported no change")
		}
		if workspaceTrusted(canon) {
			t.Error("the flag survived the revoke")
		}
		if p := readProjects(t); len(p) != 0 {
			t.Errorf("an entry rig created should go with the flag, got %v", p)
		}
	})

	t.Run("leaves a trust the human accepted", func(t *testing.T) {
		_, dataDir, canon := trustEnv(t)
		seed := map[string]any{"projects": map[string]any{
			canon: map[string]any{"hasTrustDialogAccepted": true},
		}}
		b, _ := json.MarshalIndent(seed, "", "  ")
		if err := os.WriteFile(userClaudeJSON(), b, 0o644); err != nil {
			t.Fatal(err)
		}

		if revokeWorkspaceTrust(canon, dataDir) {
			t.Fatal("revoked a trust rig never granted")
		}
		if !workspaceTrusted(canon) {
			t.Error("the human's own trust was withdrawn")
		}
	})

	t.Run("keeps the rest of the project entry", func(t *testing.T) {
		_, dataDir, canon := trustEnv(t)
		if _, err := grantWorkspaceTrust(canon, dataDir); err != nil {
			t.Fatal(err)
		}
		// The user works in the project; CC accumulates state under it.
		root, _ := readClaudeJSON()
		root["projects"].(map[string]any)[canon].(map[string]any)["history"] = []any{"a prompt"}
		if err := writeClaudeJSON(root); err != nil {
			t.Fatal(err)
		}

		if !revokeWorkspaceTrust(canon, dataDir) {
			t.Fatal("revoke reported no change")
		}
		entry, ok := readProjects(t)[canon].(map[string]any)
		if !ok {
			t.Fatal("the project entry was deleted along with the flag")
		}
		if _, gone := entry["hasTrustDialogAccepted"]; gone {
			t.Error("the flag survived")
		}
		if len(entry["history"].([]any)) != 1 {
			t.Error("the user's history was lost")
		}
	})
}

// Without a grant, init must SAY the headless path is still broken — that
// silence is what made #16 cost a whole run to discover.
func TestUntrustedWorkspaceNotice(t *testing.T) {
	_, dataDir, canon := trustEnv(t)

	notice := untrustedWorkspaceNotice(canon)
	for _, want := range []string{"not trusted", "permissions.allow", "--trust-workspace", "accept the trust dialog"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice missing %q:\n%s", want, notice)
		}
	}

	if _, err := grantWorkspaceTrust(canon, dataDir); err != nil {
		t.Fatal(err)
	}
	if n := untrustedWorkspaceNotice(canon); n != "" {
		t.Errorf("a trusted workspace must warn about nothing, got:\n%s", n)
	}
}

// A malformed ~/.claude.json must surface as an error, never as a silent
// overwrite of a file that holds the user's entire Claude Code state.
func TestGrantWorkspaceTrustRefusesBrokenJSON(t *testing.T) {
	_, dataDir, canon := trustEnv(t)
	if err := os.WriteFile(userClaudeJSON(), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := grantWorkspaceTrust(canon, dataDir); err == nil {
		t.Fatal("want an error on malformed ~/.claude.json")
	}
	b, _ := os.ReadFile(userClaudeJSON())
	if string(b) != "{not json" {
		t.Errorf("the user's file was rewritten: %q", b)
	}
}
