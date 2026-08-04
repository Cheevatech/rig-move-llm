package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A binary with no switch configuration resolves the keys empty — and the switch
// itself refuses to launch on an empty base URL rather than bill the worker leg
// to the paid account, so "empty" is a safe default rather than a broken one.
func TestSwitchKeysDefaultEmpty(t *testing.T) {
	c := LoadFrom(t.TempDir())
	if c.CCBaseURL != "" || c.CCModel != "" {
		t.Errorf("switch keys must default empty, got base=%q model=%q", c.CCBaseURL, c.CCModel)
	}
}

func TestSwitchKeysFromConfigFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, DirName), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "RIG_CC_BASE_URL=http://localhost:4001/\nRIG_CC_MODEL=haiku\n"
	if err := os.WriteFile(filepath.Join(dir, DirName, ConfigFile), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	c := LoadFrom(dir)
	if c.CCBaseURL != "http://localhost:4001" {
		t.Errorf("base should be trimmed of the trailing slash, got %q", c.CCBaseURL)
	}
	if c.CCModel != "haiku" {
		t.Errorf("model = %q, want haiku", c.CCModel)
	}
}

func TestSwitchKeysEnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, DirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, DirName, ConfigFile),
		[]byte("RIG_CC_MODEL=fromfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIG_CC_MODEL", "fromenv")

	if c := LoadFrom(dir); c.CCModel != "fromenv" {
		t.Errorf("env must override the file, got %q", c.CCModel)
	}
}
