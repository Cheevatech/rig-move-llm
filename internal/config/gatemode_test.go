package config

import "testing"

func TestGateModeDefaultsHard(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if c := LoadFrom(t.TempDir()); c.GateMode != "hard" {
		t.Errorf("default GateMode = %q, want hard", c.GateMode)
	}
	t.Setenv("GATE_MODE", "nonsense")
	if c := LoadFrom(t.TempDir()); c.GateMode != "hard" {
		t.Errorf("unknown GATE_MODE must fall back to hard")
	}
}

func TestGateModeSoft(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GATE_MODE", "SOFT")
	if c := LoadFrom(t.TempDir()); c.GateMode != "soft" {
		t.Errorf("GateMode = %q, want soft", c.GateMode)
	}
}
