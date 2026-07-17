package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestProjectIDRoundtrip covers the bijective transport encoding, including
// non-ASCII paths, and the fail-closed rejections for malformed ids.
func TestProjectIDRoundtrip(t *testing.T) {
	for _, p := range []string{"/Users/x/proj", "/tmp/โปรเจกต์ violin", "/a b/c+d"} {
		got, err := DecodeProjectID(EncodeProjectID(p))
		if err != nil || got != p {
			t.Errorf("roundtrip %q = %q, %v", p, got, err)
		}
	}
	if _, err := DecodeProjectID("!!!not-base64"); err == nil {
		t.Error("malformed base64 accepted")
	}
	if _, err := DecodeProjectID(EncodeProjectID("relative/path")); err == nil {
		t.Error("relative path accepted")
	}
}

// TestProjectRegistryLifecycle exercises register/allowed/unregister against a
// scratch HOME, including the fail-closed default when projects.json is absent.
func TestProjectRegistryLifecycle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	proj := "/tmp/some/project"

	if ProjectAllowed(proj) {
		t.Fatal("allowed with no projects.json (must fail closed)")
	}
	if err := RegisterProject(proj); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := RegisterProject(proj); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if got := LoadProjects(); len(got) != 1 || got[0] != proj {
		t.Fatalf("projects = %v, want exactly one %q", got, proj)
	}
	if !ProjectAllowed(proj) {
		t.Fatal("registered project not allowed")
	}
	if err := UnregisterProject(proj); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	if ProjectAllowed(proj) {
		t.Fatal("still allowed after unregister")
	}
}

// TestLoadFromLayersFresh verifies LoadFrom layers a project dir's local config
// over the global one and re-reads both on every call (no cache).
func TestLoadFromLayersFresh(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, DirName), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, DirName, ConfigFile), "WORKER_MODEL=global-model\nWORKER_API_BASE=http://global:1/v1\n")

	proj := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proj, DirName), 0o755); err != nil {
		t.Fatal(err)
	}
	localCfg := filepath.Join(proj, DirName, ConfigFile)
	writeFile(t, localCfg, "WORKER_MODEL=local-model\n")

	cfg := LoadFrom(proj)
	if cfg.WorkerModel != "local-model" {
		t.Errorf("WorkerModel = %q, want local override", cfg.WorkerModel)
	}
	if cfg.WorkerAPIBase != "http://global:1/v1" {
		t.Errorf("WorkerAPIBase = %q, want global fallthrough", cfg.WorkerAPIBase)
	}
	if cfg.LogMaxMB != 50 {
		t.Errorf("LogMaxMB = %d, want default 50", cfg.LogMaxMB)
	}

	// An edit must be visible on the very next Load — the daemon never caches.
	writeFile(t, localCfg, "WORKER_MODEL=edited-model\nLOG_MAX_MB=7\n")
	cfg = LoadFrom(proj)
	if cfg.WorkerModel != "edited-model" || cfg.LogMaxMB != 7 {
		t.Errorf("edit not picked up fresh: model=%q logMaxMB=%d", cfg.WorkerModel, cfg.LogMaxMB)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
