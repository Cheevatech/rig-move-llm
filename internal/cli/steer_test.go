package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The steer lands in the user's project — and the worker runs `claude -p` in that
// same project, where CLAUDE.md is auto-discovered. So this file is read by BOTH
// legs, and everything below is about the consequences of that.

// A1 measured the old steer's cost: the worker read "Delegate ALL code changes;
// do not edit files yourself", took it personally, and tried to hand its own job
// to a subagent in 10–23% of runs. The new steer has to disarm against the one
// fact that separates the readers — MAIN has the implement tool, the worker does
// not — and it has to do that BEFORE it says anything a worker could obey.
func TestSteerDisarmsItselfForTheWorker(t *testing.T) {
	disarm := strings.Index(steerMD, "If you do not have a tool called")
	if disarm < 0 {
		t.Fatal("the steer never tells a reader without the tool that it is not the audience")
	}
	if !strings.Contains(steerMD[disarm:disarm+240], "the rest of this\nfile is not about you") {
		t.Errorf("the disarming clause does not say to stop reading:\n%s", steerMD[disarm:disarm+240])
	}

	// Every instruction a worker could mistake for its own orders must come after
	// the disarm, or it is read before the reader learns it is not the audience.
	for _, instruction := range []string{"Hand the doing over", "mcp__worker__implement", "Think here"} {
		if at := strings.Index(steerMD, instruction); at < disarm {
			t.Errorf("%q appears at %d, before the disarming clause at %d — a worker reads its orders first",
				instruction, at, disarm)
		}
	}

	// It must also tell the worker what it SHOULD do, not merely that the rest is
	// irrelevant: "this is not about you" alone leaves a worker with no instruction.
	if !strings.Contains(steerMD, "read and edit the files") {
		t.Error("the steer excuses the worker but never tells it to do the work itself")
	}
}

// The enforcement is gone (S4); the text must not claim otherwise. A steer that
// describes a hook that no longer denies anything is #22 in prose — everything
// reads correct while nothing is actually happening.
func TestSteerDoesNotClaimEnforcement(t *testing.T) {
	lower := strings.ToLower(steerMD)
	for _, claim := range []string{"hook", "denies", "denied", "blocked", "not allowed", "forbidden", "you must not"} {
		if strings.Contains(lower, claim) {
			t.Errorf("the steer claims enforcement (%q) that S4 deleted", claim)
		}
	}
	// And it must say the opposite out loud, so a reader knows it may still act.
	for _, want := range []string{"Nothing prevents you from editing files yourself", "a default, not a"} {
		if !strings.Contains(steerMD, want) {
			t.Errorf("the steer does not make clear it is guidance: missing %q", want)
		}
	}
}

// The switch is described as a default worth taking, not as a certification step:
// MAIN is not asked to vouch for the diff, because in this map the human reads it.
func TestSteerDoesNotAskMainToCertify(t *testing.T) {
	if !strings.Contains(steerMD, "not being asked to certify") {
		t.Error("the steer does not say MAIN is not the one certifying the result")
	}
	// Word boundaries matter here: "delegate" contains "gate", and matching that
	// as a contract-layer relapse is how a test starts lying about the text.
	lower := strings.ToLower(steerMD)
	for _, gone := range []string{"gate", "proof", "verify the diff", "confirm the tests"} {
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(gone) + `\b`).MatchString(lower) {
			t.Errorf("the steer still asks for %q — that is the contract layer coming back as prose", gone)
		}
	}
}

// Both files rig writes must be recognisable as ours, or uninstall cannot tell
// them from something the user wrote.
func TestRigAuthoredFilesCarryTheSentinel(t *testing.T) {
	if !strings.Contains(steerMD, steerSentinel) {
		t.Error("steer has no sentinel; uninstall would have to guess")
	}
	if !strings.Contains(qwenCommandMD, steerSentinel) {
		t.Error("/qwen command has no sentinel; uninstall would have to guess")
	}
	if !strings.Contains(qwenCommandMD, "$ARGUMENTS") {
		t.Error("/qwen takes no arguments, so it cannot carry a task")
	}
}

// init writes the steer and the button; uninstall takes both back.
func TestInitWritesSteerAndCommandAndUninstallReversesThem(t *testing.T) {
	home := t.TempDir()
	proj := t.TempDir()
	t.Setenv("HOME", home)

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(proj); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	if rc := cmdInit([]string{"--no-detect", "--backend", "ollama",
		"--worker-base", "http://localhost:11434/v1", "--worker-model", "m"}); rc != 0 {
		t.Fatalf("cmdInit rc=%d", rc)
	}

	steerPath := filepath.Join(proj, ".claude", "CLAUDE.md")
	cmdPath := filepath.Join(proj, ".claude", "commands", "qwen.md")
	for _, p := range []string{steerPath, cmdPath} {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s not written: %v", p, err)
		}
		if !strings.Contains(string(data), "mcp__worker__implement") {
			t.Errorf("%s does not name the tool it is about", p)
		}
	}

	if rc := cmdUninstall(nil); rc != 0 {
		t.Fatalf("cmdUninstall rc=%d", rc)
	}
	for _, p := range []string{steerPath, cmdPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived uninstall", p)
		}
	}
}

// A user's own CLAUDE.md is never overwritten — but one WE wrote is, or an
// upgrade would leave the enforcement-era text in place forever.
func TestInitReplacesOurSteerButNotTheUsers(t *testing.T) {
	t.Run("a user's file is left alone", func(t *testing.T) {
		proj := initInto(t, "my own notes about this repo\n")
		got, _ := os.ReadFile(filepath.Join(proj, ".claude", "CLAUDE.md"))
		if string(got) != "my own notes about this repo\n" {
			t.Errorf("rig overwrote a user's CLAUDE.md:\n%s", got)
		}
	})

	t.Run("an older rig-authored file is upgraded", func(t *testing.T) {
		stale := steerSentinel + "\nDelegate ALL code changes. Do not edit files yourself — a PreToolUse hook denies them.\n"
		proj := initInto(t, stale)
		got, _ := os.ReadFile(filepath.Join(proj, ".claude", "CLAUDE.md"))
		if strings.Contains(string(got), "hook denies") {
			t.Errorf("the enforcement-era steer survived an upgrade:\n%s", got)
		}
		if !strings.Contains(string(got), "not being asked to certify") {
			t.Error("the new steer was not written over the old one")
		}
	})
}

// initInto runs init in a fresh project that already has the given CLAUDE.md.
func initInto(t *testing.T, existing string) string {
	t.Helper()
	home := t.TempDir()
	proj := t.TempDir()
	t.Setenv("HOME", home)

	if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, ".claude", "CLAUDE.md"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(proj); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	if rc := cmdInit([]string{"--no-detect", "--backend", "ollama",
		"--worker-base", "http://localhost:11434/v1", "--worker-model", "m"}); rc != 0 {
		t.Fatalf("cmdInit rc=%d", rc)
	}
	return proj
}
