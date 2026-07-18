package cli

import (
	"reflect"
	"testing"
)

// A representative teammate-spawn argv as claude appends it (from the live smoke).
var spawnArgv = []string{
	"--agent-id", "smokey2@session-e9d91a12",
	"--agent-name", "smokey2",
	"--team-name", "session-e9d91a12",
	"--parent-session-id", "e9d91a12-7371-4afd-8ccc-c1a99b2a505b",
	"--agent-type", "general-purpose",
	"--effort", "medium",
	"--model", "sonnet",
}

func TestLooksLikeTeammateSpawn(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"real spawn", spawnArgv, true},
		{"agent-id with equals", []string{"--agent-id=x", "--model", "sonnet"}, true},
		{"serve subcommand", []string{"serve", "--port", "4000"}, false},
		{"version flag", []string{"--version"}, false},
		{"help flag", []string{"--help"}, false},
		{"empty", nil, false},
		{"bare flag no agent-id", []string{"--debug"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := looksLikeTeammateSpawn(c.args); got != c.want {
				t.Fatalf("looksLikeTeammateSpawn(%v)=%v want %v", c.args, got, c.want)
			}
		})
	}
}

func TestFlagValue(t *testing.T) {
	if got := flagValue(spawnArgv, "--agent-id"); got != "smokey2@session-e9d91a12" {
		t.Fatalf("--agent-id = %q", got)
	}
	if got := flagValue([]string{"--agent-id=abc"}, "--agent-id"); got != "abc" {
		t.Fatalf("--agent-id= form = %q", got)
	}
	if got := flagValue(spawnArgv, "--missing"); got != "" {
		t.Fatalf("missing flag = %q, want empty", got)
	}
}

func TestSetFlag(t *testing.T) {
	// replace existing `--model sonnet` -> haiku, in place, nothing else touched
	got := setFlag(spawnArgv, "--model", "haiku")
	if flagValue(got, "--model") != "haiku" {
		t.Fatalf("--model not set to haiku: %v", got)
	}
	if len(got) != len(spawnArgv) {
		t.Fatalf("length changed on replace: got %d want %d", len(got), len(spawnArgv))
	}
	if flagValue(got, "--agent-id") != "smokey2@session-e9d91a12" {
		t.Fatal("replacing --model disturbed --agent-id")
	}
	// equals form
	got2 := setFlag([]string{"--model=sonnet", "--effort", "low"}, "--model", "haiku")
	if !reflect.DeepEqual(got2, []string{"--model=haiku", "--effort", "low"}) {
		t.Fatalf("equals-form replace = %v", got2)
	}
	// append when absent
	got3 := setFlag([]string{"--effort", "low"}, "--model", "haiku")
	if flagValue(got3, "--model") != "haiku" || len(got3) != 4 {
		t.Fatalf("append-when-absent = %v", got3)
	}
}

func TestTeammatePlan(t *testing.T) {
	// default: pin to worker tier (haiku), identity extracted
	id, args := teammatePlan(spawnArgv, "")
	if id != "smokey2@session-e9d91a12" {
		t.Fatalf("agentID = %q", id)
	}
	if flagValue(args, "--model") != "haiku" {
		t.Fatalf("default plan did not pin --model=haiku: %v", args)
	}

	// explicit override
	_, args = teammatePlan(spawnArgv, "opus")
	if flagValue(args, "--model") != "opus" {
		t.Fatalf("override plan --model = %q", flagValue(args, "--model"))
	}

	// inherit: leave the lead's requested model untouched
	_, args = teammatePlan(spawnArgv, "inherit")
	if flagValue(args, "--model") != "sonnet" {
		t.Fatalf("inherit plan should keep sonnet, got %q", flagValue(args, "--model"))
	}
}
