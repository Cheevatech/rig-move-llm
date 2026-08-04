package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/config"
)

// The wizard/init path writes the switch keys into config.env, and the config
// loader resolves them back — the whole product path from setup answer to
// running switch, minus the TUI keystrokes.
func TestApplyInitSwitchWritesConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	rc := applyInit(initOpts{
		global: true, workerBase: "http://w:8000/v1", workerModel: "m",
		ccBase: "http://localhost:4001", ccModel: "haiku",
		enabled: true, mainUpstream: "https://api.anthropic.com", port: "4000", force: true,
	})
	if rc != 0 {
		t.Fatalf("applyInit rc=%d", rc)
	}

	data, _ := os.ReadFile(filepath.Join(home, config.DirName, config.ConfigFile))
	for _, want := range []string{"RIG_CC_BASE_URL=http://localhost:4001", "RIG_CC_MODEL=haiku"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("config.env missing %q:\n%s", want, data)
		}
	}

	c := config.LoadFrom(t.TempDir())
	if c.CCBaseURL != "http://localhost:4001" || c.CCModel != "haiku" {
		t.Errorf("loader did not resolve the switch keys: base=%q model=%q",
			c.CCBaseURL, c.CCModel)
	}
}
