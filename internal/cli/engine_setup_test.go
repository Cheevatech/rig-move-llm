package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/config"
)

// The wizard/init path writes the switch into config.env and the loader resolves
// it back — the whole product path from setup answer to running switch, minus the
// TUI keystrokes.
func TestApplyInitSwitchWritesConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	rc := applyInit(initOpts{
		global: true, workerBase: "http://w:8000/v1", workerModel: "m",
		enabled: true, mainUpstream: "https://api.anthropic.com", port: "4000", force: true,
	})
	if rc != 0 {
		t.Fatalf("applyInit rc=%d", rc)
	}

	data, _ := os.ReadFile(filepath.Join(home, config.DirName, config.ConfigFile))
	for _, want := range []string{
		"RIG_ROUTE_ALL_TO_WORKER=false",
		"ENABLED=true",
		"WORKER_API_BASE=http://w:8000/v1",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("config.env missing %q:\n%s", want, data)
		}
	}

	// The keys that went with the deleted delegate arm must not come back: a
	// config.env that still names them reads as configuration a user can act on,
	// and every one of them is inert.
	for _, gone := range []string{"RIG_CC_BASE_URL", "RIG_CC_MODEL", "WORKER_HEALTH_PATH", "MAIN_SHARED_MCP"} {
		if strings.Contains(string(data), gone) {
			t.Errorf("config.env still mentions the dead key %q:\n%s", gone, data)
		}
	}

	c := config.LoadFrom(t.TempDir())
	if c.WorkerAPIBase != "http://w:8000/v1" || !c.Enabled || c.RouteAllToWorker {
		t.Errorf("loader did not resolve the config: worker=%q enabled=%v switch=%v",
			c.WorkerAPIBase, c.Enabled, c.RouteAllToWorker)
	}
}

// A global init writes its config and the one slash command, and nothing else.
//
// The list this pins down is the point: hooks, CLAUDE.md, .mcp.json and
// settings.json went with the layers that read them, and a stray one would be a
// mechanism nobody maintains any more. /worker is deliberately NOT in that
// category — it is inert until typed and its absence is visible the moment it is
// — but it is the only exception, so the test names it rather than counting files.
func TestApplyInitWritesConfigAndTheWorkerCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if rc := applyInit(initOpts{
		global: true, workerBase: "http://w:8000/v1", workerModel: "m",
		enabled: true, mainUpstream: "https://api.anthropic.com", port: "4000", force: true,
	}); rc != 0 {
		t.Fatalf("applyInit rc=%d", rc)
	}

	var written []string
	_ = filepath.Walk(home, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			rel, _ := filepath.Rel(home, p)
			written = append(written, rel)
		}
		return nil
	})

	want := map[string]bool{
		filepath.Join(config.DirName, config.ConfigFile):  true,
		filepath.Join(".claude", "commands", "worker.md"): true,
	}
	for _, w := range written {
		if !want[w] {
			t.Errorf("init wrote an unexpected file: %s", w)
		}
		delete(want, w)
	}
	for missing := range want {
		t.Errorf("init did not write %s", missing)
	}
}

// The slash command has to invoke the command that exists. A rename that misses
// this file leaves a button that reports "unknown command" to whichever model
// pressed it — and one of those models is the worker, mid-task.
func TestWorkerCommandInvokesTheRealCommand(t *testing.T) {
	if !strings.Contains(workerCommandMD, "rig-move-llm worker $ARGUMENTS") {
		t.Errorf("/worker must call `rig-move-llm worker $ARGUMENTS`:\n%s", workerCommandMD)
	}
	for _, gone := range []string{"qwen", "mcp__worker__implement"} {
		if strings.Contains(workerCommandMD, gone) {
			t.Errorf("/worker still mentions %q", gone)
		}
	}
}

// A hand-edited command is the user's; a re-init must not stamp over it.
func TestWorkerCommandDoesNotClobberAnEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "commands", "worker.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	mine := "my own version\n"
	if err := os.WriteFile(path, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeWorkerCommand(dir); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != mine {
		t.Errorf("re-init overwrote a user-edited command:\n%s", got)
	}
}

// init writes /worker outside rig's own dir, so uninstall owes it a removal —
// but only the copy rig wrote. A user's edit is theirs.
func TestUninstallRemovesOnlyAnUneditedWorkerCommand(t *testing.T) {
	t.Run("ours is removed", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "commands", "worker.md")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(workerCommandMD), 0o644); err != nil {
			t.Fatal(err)
		}
		removeIfUnchanged(path, workerCommandMD)
		if fileExists(path) {
			t.Error("uninstall left the command rig wrote")
		}
	})
	t.Run("theirs is kept", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "commands", "worker.md")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(workerCommandMD+"\n<!-- my note -->\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		removeIfUnchanged(path, workerCommandMD)
		if !fileExists(path) {
			t.Error("uninstall deleted a command the user had edited")
		}
	})
}

// A --global install has no local config.env, and `worker` used to default to the
// local scope unconditionally — so every flip in a project answered "no config
// here, run init first", including the one /worker makes from inside a session.
// Global is the mode the docs recommend first, so this was the common path.
func TestWorkerTargetsTheScopeThatWillBeRead(t *testing.T) {
	run := func(t *testing.T, args ...string) (home, proj string, rc int) {
		t.Helper()
		home = t.TempDir()
		t.Setenv("HOME", home)
		proj = t.TempDir()
		cwd, _ := os.Getwd()
		if err := os.Chdir(proj); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chdir(cwd) })
		if rc := applyInit(initOpts{
			global: true, workerBase: "http://w/v1", workerModel: "m",
			enabled: true, mainUpstream: "https://api.anthropic.com", port: "4000", force: true,
		}); rc != 0 {
			t.Fatalf("global init rc=%d", rc)
		}
		return home, proj, cmdWorker(args)
	}

	t.Run("global-only install flips the global scope", func(t *testing.T) {
		home, _, rc := run(t, "on")
		if rc != 0 {
			t.Fatalf("worker on rc=%d, want 0 — a global install could not flip", rc)
		}
		b, err := os.ReadFile(filepath.Join(home, config.DirName, config.ConfigFile))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "RIG_ROUTE_ALL_TO_WORKER=true") {
			t.Errorf("global config did not get the flag:\n%s", b)
		}
	})

	t.Run("a project with its own config owns the answer", func(t *testing.T) {
		home, proj, _ := run(t, "status")
		if rc := applyInit(initOpts{
			workerBase: "http://w/v1", workerModel: "m", enabled: true,
			mainUpstream: "https://api.anthropic.com", port: "4000", force: true,
		}); rc != 0 {
			t.Fatal("local init failed")
		}
		if rc := cmdWorker([]string{"on"}); rc != 0 {
			t.Fatalf("worker on rc=%d", rc)
		}
		local, _ := os.ReadFile(filepath.Join(proj, config.DirName, config.ConfigFile))
		if !strings.Contains(string(local), "RIG_ROUTE_ALL_TO_WORKER=true") {
			t.Errorf("local config should have been the target:\n%s", local)
		}
		global, _ := os.ReadFile(filepath.Join(home, config.DirName, config.ConfigFile))
		if strings.Contains(string(global), "RIG_ROUTE_ALL_TO_WORKER=true") {
			t.Error("global scope was flipped even though the project has its own config")
		}
	})
}
