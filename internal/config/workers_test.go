package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeWorkers(t *testing.T, dir, content string) {
	t.Helper()
	scope := filepath.Join(dir, DirName)
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, WorkersFile), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestLoadWorkersResolveAndSort covers backend-default bases, trailing-slash
// trim, priority sort (stable), passthrough entries, and dropping entries that
// have neither a base nor a backend default.
func TestLoadWorkersResolveAndSort(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate from any real global workers.json
	proj := t.TempDir()
	writeWorkers(t, proj, `{"endpoints":[
		{"name":"remote","base":"http://b:9000/v1/","priority":2,"key":"sk-x"},
		{"name":"local","backend":"ollama","priority":1},
		{"passthrough":true,"priority":9},
		{"name":"broken","backend":"generic"}
	]}`)

	cfg := LoadFrom(proj)
	if len(cfg.Workers) != 3 {
		t.Fatalf("want 3 endpoints, got %d: %+v", len(cfg.Workers), cfg.Workers)
	}
	if cfg.Workers[0].Name != "local" || cfg.Workers[0].Base != "http://localhost:11434/v1" {
		t.Errorf("first endpoint = %+v, want ollama default base", cfg.Workers[0])
	}
	if cfg.Workers[1].Name != "remote" || cfg.Workers[1].Base != "http://b:9000/v1" {
		t.Errorf("second endpoint = %+v, want trimmed base", cfg.Workers[1])
	}
	if !cfg.Workers[2].Passthrough || cfg.Workers[2].Label() != "passthrough" {
		t.Errorf("third endpoint = %+v, want passthrough", cfg.Workers[2])
	}
}

// TestLoadWorkersLocalWinsWholesale: a local workers.json replaces the global
// one entirely (no merge), and without a project dir the global chain applies.
func TestLoadWorkersLocalWinsWholesale(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeWorkers(t, home, `{"endpoints":[{"name":"global-ep","base":"http://g/v1"}]}`)
	proj := t.TempDir()
	writeWorkers(t, proj, `{"endpoints":[{"name":"local-ep","base":"http://l/v1"}]}`)

	if got := LoadFrom(proj).Workers; len(got) != 1 || got[0].Name != "local-ep" {
		t.Errorf("local scope: got %+v, want single local-ep", got)
	}
	if got := LoadFrom("").Workers; len(got) != 1 || got[0].Name != "global-ep" {
		t.Errorf("global scope: got %+v, want single global-ep", got)
	}
}

// TestLoadWorkersMalformedFallsBack: a broken workers.json must not take the
// worker leg down — the chain is empty so the scalar config applies.
func TestLoadWorkersMalformedFallsBack(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	proj := t.TempDir()
	writeWorkers(t, proj, `{"endpoints":[`)

	if got := LoadFrom(proj).Workers; len(got) != 0 {
		t.Errorf("got %+v, want empty chain (scalar fallback)", got)
	}
}
