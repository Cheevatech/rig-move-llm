package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/config"
)

// The wizard/init path writes the cc engine keys into config.env, and the
// config loader resolves them back — the whole product path from setup answer
// to running engine, minus the TUI keystrokes.
func TestApplyInitCCEngineWritesConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	rc := applyInit(initOpts{
		global: true, workerBase: "http://w:8000/v1", workerModel: "m",
		workerEngine: "cc", ccBase: "http://localhost:4001", ccModel: "haiku",
		enabled: true, mainUpstream: "https://api.anthropic.com", port: "4000", force: true,
	})
	if rc != 0 {
		t.Fatalf("applyInit rc=%d", rc)
	}

	data, _ := os.ReadFile(filepath.Join(home, config.DirName, config.ConfigFile))
	for _, want := range []string{"RIG_WORKER_ENGINE=cc", "RIG_CC_BASE_URL=http://localhost:4001", "RIG_CC_MODEL=haiku"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("config.env missing %q:\n%s", want, data)
		}
	}

	c := config.LoadFrom(t.TempDir())
	if c.WorkerEngine != "cc" || c.CCBaseURL != "http://localhost:4001" || c.CCModel != "haiku" {
		t.Errorf("loader did not resolve the engine keys: engine=%q base=%q model=%q",
			c.WorkerEngine, c.CCBaseURL, c.CCModel)
	}
}

// The default path must not mention an engine choice as active: keys are
// present only as commented documentation.
func TestApplyInitDefaultKeepsLoopEngine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if rc := applyInit(initOpts{global: true, workerBase: "http://w:8000/v1", enabled: true,
		mainUpstream: "https://api.anthropic.com", port: "4000", force: true}); rc != 0 {
		t.Fatalf("applyInit rc=%d", rc)
	}
	data, _ := os.ReadFile(filepath.Join(home, config.DirName, config.ConfigFile))
	if strings.Contains(string(data), "\nRIG_WORKER_ENGINE=") {
		t.Errorf("engine key must stay commented by default:\n%s", data)
	}
	if config.LoadFrom(t.TempDir()).WorkerEngine != "" {
		t.Error("default install must resolve to the loop engine")
	}
}

// --worker-engine=cc without a base URL fails at init time with a clear error,
// not at the first delegation (money-safety rail: the engine refuses to launch
// without a base URL, so an install that omits it can never work).
func TestCmdInitCCRequiresBaseURL(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if rc := cmdInit([]string{"--worker-engine=cc", "--no-detect"}); rc != 2 {
		t.Errorf("cc without --cc-base-url must be rejected, rc=%d", rc)
	}
	if rc := cmdInit([]string{"--worker-engine=bogus", "--no-detect"}); rc != 2 {
		t.Errorf("unknown engine must be rejected, rc=%d", rc)
	}
}

// cmdWorker promotes config-file engine values into the environment — the
// surface the cc engine actually reads — without overriding explicit env.
func TestCmdWorkerPromotesEngineConfigToEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, config.DirName), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "RIG_WORKER_ENGINE=cc\nRIG_CC_BASE_URL=http://localhost:4001\nRIG_CC_MODEL=haiku\n"
	if err := os.WriteFile(filepath.Join(home, config.DirName, config.ConfigFile), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// t.Setenv registers restoration of the original value; the Unsetenv after it
	// makes the variable truly ABSENT for the test (set-empty would win the
	// precedence race and defeat the promotion).
	for _, k := range []string{"RIG_WORKER_ENGINE", "RIG_CC_BASE_URL", "RIG_CC_MODEL"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}

	if rc := cmdWorker(nil); rc != 0 {
		t.Fatalf("cmdWorker rc=%d", rc)
	}
	if got := os.Getenv("RIG_WORKER_ENGINE"); got != "cc" {
		t.Errorf("RIG_WORKER_ENGINE not promoted, got %q", got)
	}
	if got := os.Getenv("RIG_CC_BASE_URL"); got != "http://localhost:4001" {
		t.Errorf("RIG_CC_BASE_URL not promoted, got %q", got)
	}
	if got := os.Getenv("RIG_CC_MODEL"); got != "haiku" {
		t.Errorf("RIG_CC_MODEL not promoted, got %q", got)
	}
}
