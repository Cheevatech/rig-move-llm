package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The undo paths. Every "command that lies" found so far was found by running
// rig on a real machine, never by reading it — the state that exposes them (a
// backup written by a previous generation, an install layered on an install) is
// state nobody thinks to construct. These exercise the reversal paths that had
// never been walked end to end, each asserting the two things a unit test
// normally skips: what is left on disk, and what the command SAID.

// chdir moves into dir for the duration of the test.
func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

// capture runs fn with stdout redirected and returns what it printed. The
// printed line is half of what a reversal command promises, so it is half of
// what these tests check.
func capture(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- b.String()
	}()
	fn()
	os.Stdout = prev
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// TestUninstallWithoutPurgeKeepsDataAndSaysSo walks the branch that had never
// been run: every uninstall in this project's history passed --purge. Without it
// the data dir is deliberately kept, and the risk is the reverse of the bug
// fixed in 0.8.1 — a command that removes MORE than it says.
func TestUninstallWithoutPurgeKeepsData(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	proj := t.TempDir()
	chdir(t, proj)

	if rc := applyInit(initOpts{
		workerBase: "http://w:8000/v1", workerModel: "m", enabled: true,
		mainUpstream: "https://api.anthropic.com", port: "4711", force: true,
	}); rc != 0 {
		t.Fatalf("init rc=%d", rc)
	}
	dataDir := filepath.Join(proj, ".rig-move-llm")
	if _, err := os.Stat(dataDir); err != nil {
		t.Fatalf("init wrote no data dir: %v", err)
	}

	out := capture(t, func() { cmdUninstall(nil) })

	// The config survives — that is what leaving off --purge means.
	if _, err := os.Stat(filepath.Join(dataDir, "config.env")); err != nil {
		t.Errorf("uninstall without --purge deleted the config anyway: %v", err)
	}
	// ...and the command must not have claimed otherwise.
	if strings.Contains(out, "purged") {
		t.Errorf("said it purged without --purge:\n%s", out)
	}
	// The wiring is gone either way: that is what uninstall is for.
	if _, err := os.Stat(filepath.Join(proj, ".claude", "commands", "worker.md")); err == nil {
		t.Error("the /worker command survived uninstall")
	}
}

// TestUninstallGlobalLeavesForeignMCPServersAlone pins the blast radius of the
// one write uninstall makes into Claude Code's own state file. ~/.claude.json
// holds auth and every project's history, and Claude Code rewrites it
// continuously, so rig touching it at all is a lost-update risk.
func TestUninstallGlobalLeavesForeignMCPServersAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	chdir(t, t.TempDir())

	claudeJSON := filepath.Join(home, ".claude.json")
	original := `{
  "oauthAccount": {"emailAddress": "someone@example.com"},
  "numStartups": 4211,
  "mcpServers": {
    "worker": {"command": "rig-move-llm"},
    "serena": {"command": "serena-mcp"}
  }
}`
	if err := os.WriteFile(claudeJSON, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if !unregisterUserMCP() {
		t.Fatal("unregisterUserMCP reported nothing removed when rig's server was there")
	}

	var got map[string]any
	body, _ := os.ReadFile(claudeJSON)
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("uninstall left ~/.claude.json unparseable: %v\n%s", err, body)
	}
	servers, _ := got["mcpServers"].(map[string]any)
	if _, still := servers["worker"]; still {
		t.Error("rig's own MCP server survived uninstall")
	}
	if _, ok := servers["serena"]; !ok {
		t.Error("uninstall removed an MCP server that was not rig's")
	}
	if got["oauthAccount"] == nil || got["numStartups"] == nil {
		t.Errorf("uninstall dropped Claude Code's own state:\n%s", body)
	}
}

// TestUnregisterUserMCPDoesNotRewriteWhenNothingToRemove is the fix for the
// risk above. Before it, uninstall --global read ~/.claude.json, round-tripped
// it through map[string]any and wrote it back on every run — including the
// common case where rig's server was not in the file at all. Claude Code writes
// that file continuously; a read-modify-write with nothing to change is a lost
// update with no upside whatsoever.
func TestUnregisterUserMCPDoesNotRewriteWhenNothingToRemove(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no mcpServers at all", `{"oauthAccount":{"emailAddress":"a@b.c"},"numStartups":9}`},
		{"only other people's servers", `{"mcpServers":{"serena":{"command":"serena-mcp"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			path := filepath.Join(home, ".claude.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}

			if unregisterUserMCP() {
				t.Error("reported a removal from a file that has nothing of rig's in it")
			}

			after, _ := os.ReadFile(path)
			if string(after) != tc.body {
				t.Errorf("rewrote Claude Code's state file with nothing to change:\nbefore: %s\nafter:  %s", tc.body, after)
			}
			if st, _ := os.Stat(path); st.ModTime() != before.ModTime() {
				t.Error("touched the file's mtime with nothing to change")
			}
		})
	}
}

// TestRemoveIfUnchangedKeepsAUserEdit covers the promise the code makes in a
// comment and nothing checked: a /worker command the user edited is theirs, and
// uninstall reverses rig's writes, not the user's.
func TestRemoveIfUnchangedKeepsAUserEdit(t *testing.T) {
	dir := t.TempDir()
	mine := filepath.Join(dir, "worker.md")
	if err := os.WriteFile(mine, []byte(workerCommandMD+"\nmy own extra line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	removeIfUnchanged(mine, workerCommandMD)
	if _, err := os.Stat(mine); err != nil {
		t.Error("uninstall deleted a /worker command the user had edited")
	}

	untouched := filepath.Join(dir, "pristine.md")
	if err := os.WriteFile(untouched, []byte(workerCommandMD), 0o644); err != nil {
		t.Fatal(err)
	}
	removeIfUnchanged(untouched, workerCommandMD)
	if _, err := os.Stat(untouched); err == nil {
		t.Error("uninstall left behind a /worker command it had written itself")
	}
}

// TestRemoveOwnedSteerNeedsTheSentinel pins the same rule for the steer files:
// ours carry a marker, and a file without one is somebody's own memory file.
func TestRemoveOwnedSteerNeedsTheSentinel(t *testing.T) {
	dir := t.TempDir()

	theirs := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(theirs, []byte("# my own project memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	removeOwnedSteer(theirs)
	if _, err := os.Stat(theirs); err != nil {
		t.Error("uninstall deleted a CLAUDE.md that was not rig's")
	}

	ours := filepath.Join(dir, "ours.md")
	if err := os.WriteFile(ours, []byte(steerSentinel+"\nsteer text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	removeOwnedSteer(ours)
	if _, err := os.Stat(ours); err == nil {
		t.Error("uninstall left behind a steer carrying rig's own sentinel")
	}
}

// TestInitForceOverAnExistingInstallIsClean walks init --force onto an install
// that is already there — the upgrade path a user takes when told to reinstall,
// and one that had no test at all.
func TestInitForceOverAnExistingInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	proj := t.TempDir()
	chdir(t, proj)

	if rc := applyInit(initOpts{
		workerBase: "http://old:8000/v1", workerModel: "old", enabled: true,
		mainUpstream: "https://api.anthropic.com", port: "4711", force: true,
	}); rc != 0 {
		t.Fatalf("first init rc=%d", rc)
	}
	if rc := applyInit(initOpts{
		workerBase: "http://new:9000/v1", workerModel: "new", enabled: true,
		mainUpstream: "https://api.anthropic.com", port: "4712", force: true,
	}); rc != 0 {
		t.Fatalf("second init rc=%d", rc)
	}

	body, err := os.ReadFile(filepath.Join(proj, ".rig-move-llm", "config.env"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "old:8000") {
		t.Errorf("--force left the previous endpoint behind:\n%s", body)
	}
	if !strings.Contains(string(body), "new:9000") {
		t.Errorf("--force did not write the new endpoint:\n%s", body)
	}
	// The allowlist is the fail-closed gate; a second init must not double-register.
	regs, err := os.ReadFile(filepath.Join(home, ".rig-move-llm", "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		Projects []string `json:"projects"`
	}
	if err := json.Unmarshal(regs, &reg); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, p := range reg.Projects {
		seen[p]++
	}
	for p, n := range seen {
		if n > 1 {
			t.Errorf("project registered %d times after a second init: %s", n, p)
		}
	}
}
