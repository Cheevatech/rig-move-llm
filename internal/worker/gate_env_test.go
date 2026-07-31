package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestGateEnvScrubsRigVars is #42: the engine-run gate must not carry rig's
// configuration into the user's build. RIG_AGENT_ID is the one that actually
// broke a measured round — this repo's own tests read it — so it is asserted by
// name as well as by prefix.
func TestGateEnvScrubsRigVars(t *testing.T) {
	t.Setenv("RIG_AGENT_ID", "cc-worker")
	t.Setenv("RIG_CC_BASE_URL", "http://127.0.0.1:4010")
	t.Setenv("PATH", os.Getenv("PATH"))

	env := gateEnv()
	for _, kv := range env {
		if strings.HasPrefix(kv, "RIG_") {
			t.Errorf("gate env carries a rig variable: %q", kv)
		}
	}

	// Scrubbing must not empty the environment: the gate needs PATH to find the
	// toolchain doctor's rung 6 just proved was there.
	var sawPath bool
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			sawPath = true
		}
	}
	if !sawPath {
		t.Error("gate env has no PATH — the gate could not find its toolchain")
	}
}

// TestGateEnvReachesTheProcess proves the scrub at the only place that matters:
// a child process spawned the way runEngineGate spawns its gate does not see
// RIG_AGENT_ID.
func TestGateEnvReachesTheProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script probe is POSIX")
	}
	t.Setenv("RIG_AGENT_ID", "cc-worker")

	probe := filepath.Join(t.TempDir(), "probe.sh")
	if err := os.WriteFile(probe, []byte("#!/bin/sh\nprintf '%s' \"${RIG_AGENT_ID-unset}\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	shell, flag := gateShell()
	cmd := exec.Command(shell, flag, probe)
	cmd.Env = gateEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got := string(out); got != "unset" {
		t.Errorf("child saw RIG_AGENT_ID=%q, want it unset", got)
	}
}
