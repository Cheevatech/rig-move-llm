package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeLocalEnv(t *testing.T, dir, content string) {
	t.Helper()
	d := filepath.Join(dir, DirName)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, ConfigFile), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestEnabledResolution covers the master-switch defaulting rule: absent ENABLED
// tracks whether a worker endpoint is set; an explicit ENABLED overrides.
func TestEnabledResolution(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate the global scope from the dev machine

	cases := []struct {
		name string
		env  string
		want bool
	}{
		{"worker set, no ENABLED -> enabled", "WORKER_API_BASE=http://w:8000/v1\n", true},
		{"no worker, no ENABLED -> disabled", "PORT=4000\n", false},
		{"explicit false overrides a set worker", "WORKER_API_BASE=http://w:8000/v1\nENABLED=false\n", false},
		{"explicit true with no worker", "ENABLED=true\n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeLocalEnv(t, dir, c.env)
			if got := LoadFrom(dir).Enabled; got != c.want {
				t.Errorf("Enabled=%v, want %v", got, c.want)
			}
		})
	}
}
