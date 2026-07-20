package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/config"
)

// TestSetConfigKey verifies the env-file rewriter flips an existing key in place,
// un-comments a commented key, and appends a missing one — never duplicating.
func TestSetConfigKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.env")

	orig := "# header comment\nWORKER_API_BASE=http://x/v1\nENABLED=true\n# LOG_BODIES=\n"
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}

	// flip an uncommented key in place
	if err := setConfigKey(path, "ENABLED", "false"); err != nil {
		t.Fatal(err)
	}
	// un-comment a commented key
	if err := setConfigKey(path, "LOG_BODIES", "1"); err != nil {
		t.Fatal(err)
	}
	// append a missing key
	if err := setConfigKey(path, "PORT", "5000"); err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(path)
	v := config.FileValues(path)
	if v["ENABLED"] != "false" {
		t.Errorf("ENABLED=%q, want false", v["ENABLED"])
	}
	if v["LOG_BODIES"] != "1" {
		t.Errorf("LOG_BODIES=%q, want 1", v["LOG_BODIES"])
	}
	if v["PORT"] != "5000" {
		t.Errorf("PORT=%q, want 5000", v["PORT"])
	}
	// the untouched key and the header must survive
	if v["WORKER_API_BASE"] != "http://x/v1" {
		t.Errorf("WORKER_API_BASE clobbered: %q", v["WORKER_API_BASE"])
	}
	if !strings.Contains(string(got), "# header comment") {
		t.Error("header comment lost")
	}
	// no duplicate ENABLED line
	if n := strings.Count(string(got), "ENABLED="); n != 1 {
		t.Errorf("ENABLED appears %d times, want 1:\n%s", n, got)
	}
}

// TestEnableDisableGlobal verifies `enable`/`disable` flip the master switch in the
// global config.env (the default scope) and take effect via config.Load.
func TestEnableDisableGlobal(t *testing.T) {
	home := t.TempDir()
	proj := t.TempDir()
	t.Setenv("HOME", home)

	wd, _ := os.Getwd()
	if err := os.Chdir(proj); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	// A global install with a worker endpoint (enabled).
	if rc := cmdInit([]string{"--global", "--no-detect", "--backend", "ollama",
		"--worker-base", "http://localhost:11434/v1", "--worker-model", "m"}); rc != 0 {
		t.Fatalf("cmdInit rc=%d", rc)
	}
	if !config.Load().Enabled {
		t.Fatal("expected enabled after global init with worker")
	}

	if rc := cmdDisable(nil); rc != 0 {
		t.Fatalf("cmdDisable rc=%d", rc)
	}
	if config.Load().Enabled {
		t.Error("still enabled after disable")
	}

	if rc := cmdEnable(nil); rc != 0 {
		t.Fatalf("cmdEnable rc=%d", rc)
	}
	if !config.Load().Enabled {
		t.Error("still disabled after enable")
	}
}

// TestEnableLocalScope verifies --local targets the project's own config.env and
// overrides the global scope.
func TestEnableLocalScope(t *testing.T) {
	home := t.TempDir()
	proj := t.TempDir()
	t.Setenv("HOME", home)

	wd, _ := os.Getwd()
	if err := os.Chdir(proj); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	// global enabled, local disabled -> local wins.
	if rc := cmdInit([]string{"--global", "--no-detect", "--backend", "ollama",
		"--worker-base", "http://localhost:11434/v1", "--worker-model", "m"}); rc != 0 {
		t.Fatalf("global init rc=%d", rc)
	}
	if rc := cmdInit([]string{"--no-detect", "--backend", "ollama",
		"--worker-base", "http://localhost:11434/v1", "--worker-model", "m"}); rc != 0 {
		t.Fatalf("local init rc=%d", rc)
	}

	if rc := cmdDisable([]string{"--local"}); rc != 0 {
		t.Fatalf("cmdDisable --local rc=%d", rc)
	}
	if config.Load().Enabled {
		t.Error("local disable did not override global enabled")
	}
	// the global scope must be untouched.
	gv := config.FileValues(filepath.Join(config.GlobalDir(), config.ConfigFile))
	if gv["ENABLED"] != "true" {
		t.Errorf("global ENABLED changed by --local flip: %q", gv["ENABLED"])
	}
}

// TestEnableMissingConfig verifies a helpful non-zero exit when no config exists.
func TestEnableMissingConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if rc := cmdEnable(nil); rc == 0 {
		t.Error("expected non-zero exit when global config is absent")
	}
}

// TestConfigReport verifies `config` runs and reports the effective state without
// erroring when a global config exists.
func TestConfigReport(t *testing.T) {
	home := t.TempDir()
	proj := t.TempDir()
	t.Setenv("HOME", home)

	wd, _ := os.Getwd()
	if err := os.Chdir(proj); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	if rc := cmdInit([]string{"--global", "--no-detect", "--backend", "ollama",
		"--worker-base", "http://localhost:11434/v1", "--worker-model", "m"}); rc != 0 {
		t.Fatalf("cmdInit rc=%d", rc)
	}
	if rc := cmdConfig(nil); rc != 0 {
		t.Errorf("cmdConfig rc=%d", rc)
	}
}
