package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The default engine is the built-in loop: absent keys resolve to empty, so a
// binary with no cc configuration behaves exactly like the pre-B5 product.
func TestWorkerEngineDefaultsEmpty(t *testing.T) {
	c := LoadFrom(t.TempDir())
	if c.WorkerEngine != "" || c.CCBaseURL != "" || c.CCModel != "" {
		t.Errorf("engine keys must default empty, got engine=%q base=%q model=%q",
			c.WorkerEngine, c.CCBaseURL, c.CCModel)
	}
}

func TestWorkerEngineFromConfigFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, DirName), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "RIG_WORKER_ENGINE=CC\nRIG_CC_BASE_URL=http://localhost:4001/\nRIG_CC_MODEL=haiku\n"
	if err := os.WriteFile(filepath.Join(dir, DirName, ConfigFile), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	c := LoadFrom(dir)
	if c.WorkerEngine != "cc" {
		t.Errorf("engine should normalize to lowercase cc, got %q", c.WorkerEngine)
	}
	if c.CCBaseURL != "http://localhost:4001" {
		t.Errorf("base should be trimmed of the trailing slash, got %q", c.CCBaseURL)
	}
	if c.CCModel != "haiku" {
		t.Errorf("model = %q, want haiku", c.CCModel)
	}
}

func TestWorkerEngineEnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, DirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, DirName, ConfigFile),
		[]byte("RIG_WORKER_ENGINE=cc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIG_WORKER_ENGINE", "loop")

	if c := LoadFrom(dir); c.WorkerEngine != "loop" {
		t.Errorf("env must override the file, got %q", c.WorkerEngine)
	}
}
