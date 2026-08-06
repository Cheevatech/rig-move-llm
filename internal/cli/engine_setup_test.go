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

// A global init writes exactly one file. Everything rig used to leave in a user's
// Claude Code config — hooks, CLAUDE.md, .mcp.json, settings.json, a slash command
// — went with the layers that read them, and a stray one would be a mechanism
// nobody maintains any more.
func TestApplyInitWritesOnlyConfigEnv(t *testing.T) {
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

	want := filepath.Join(config.DirName, config.ConfigFile)
	if len(written) != 1 || written[0] != want {
		t.Errorf("init wrote %v, want exactly [%s]", written, want)
	}
}
