package thin

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The surface is the decision S1 made, so it is pinned here. Every assertion in
// this file is something that grew back last time.

func TestSurfaceIsOneTool(t *testing.T) {
	tools := toolList()
	if len(tools) != 1 {
		var names []string
		for _, tool := range tools {
			names = append(names, tool["name"].(string))
		}
		t.Fatalf("surface has %d tools (%s); S1 decided on one: implement",
			len(tools), strings.Join(names, ", "))
	}
	if tools[0]["name"] != "implement" {
		t.Fatalf("the one tool is %q, want implement", tools[0]["name"])
	}

	schema := tools[0]["inputSchema"].(map[string]any)
	props := schema["properties"].(map[string]any)
	for _, banned := range []string{"gate_dir", "max_turns", "timeout", "wall", "rounds", "budget"} {
		if _, ok := props[banned]; ok {
			t.Errorf("implement accepts %q — ceilings and gates are film's policy, not a caller's parameter", banned)
		}
	}
	if _, ok := props["task"]; !ok {
		t.Error("implement has no task parameter")
	}
	if _, ok := props["repo"]; !ok {
		t.Error("implement has no repo parameter — under the product wiring the cwd is rig's own checkout (B6 run 2)")
	}
}

// The return is text with four parts, and no field a machine has to believe.
func TestRenderShape(t *testing.T) {
	text := Render(Outcome{
		Status:      "finished",
		LogDir:      "/runs/x",
		Diff:        "diff --git a/a.txt b/a.txt\n+hello\n",
		LastCommand: "$ go test ./...\nok",
	})
	for _, want := range []string{"status: finished", "log:    /runs/x", "--- diff ---", "+hello", "--- last command ---", "go test ./..."} {
		if !strings.Contains(text, want) {
			t.Errorf("return is missing %q:\n%s", want, text)
		}
	}
	for _, banned := range []string{"verdict", "unproductive", "tier", "granularity", "proof", "flip", "passed:"} {
		if strings.Contains(strings.ToLower(text), banned) {
			t.Errorf("return contains %q — nothing here may be a claim a machine acts on", banned)
		}
	}
	if json.Valid([]byte(strings.TrimSpace(text))) {
		t.Error("the return parsed as JSON; it is meant to be read, not unpacked")
	}
}

// Over the threshold the bytes are not pasted — but they are the same bytes, at
// a path a plain Read opens. No drill tool exists to reach them.
func TestRenderPointsAtLargeDiffs(t *testing.T) {
	t.Setenv("RIG_THIN_DIFF_INLINE_BYTES", "64")
	big := strings.Repeat("+a line of diff\n", 100)
	text := Render(Outcome{
		Status:   "finished",
		Diff:     big,
		DiffStat: " a.txt | 100 +++",
		DiffPath: "/runs/x/diff.patch",
	})
	if strings.Contains(text, big) {
		t.Error("a diff over the threshold was inlined anyway")
	}
	for _, want := range []string{" a.txt | 100 +++", "/runs/x/diff.patch"} {
		if !strings.Contains(text, want) {
			t.Errorf("large-diff return is missing %q:\n%s", want, text)
		}
	}
}

func TestRenderSaysWhenNothingChanged(t *testing.T) {
	text := Render(Outcome{Status: "finished"})
	if !strings.Contains(text, "working tree is unchanged") {
		t.Errorf("an empty diff must say so plainly:\n%s", text)
	}
}

// A1's finding, pinned: the worker must not inherit MAIN's output style (which
// tells it not to edit files) and must not keep the tools it used to comply.
func TestWorkerArgvDoesNotInheritMainsPosture(t *testing.T) {
	args := workerArgv("claude", "do the thing")
	argv := strings.Join(args, " ")
	if !strings.Contains(argv, `--settings {"outputStyle":"default"}`) {
		t.Errorf("argv does not reset the output style — the worker inherits MAIN's 'do not edit files' instruction:\n%s", argv)
	}
	// --setting-sources with an empty value is the flag that stops MAIN's hooks
	// and output style loading at all; joining the argv would hide an empty
	// value, so this looks at the pair directly.
	var sawEmptySources bool
	for i, a := range args {
		if a == "--setting-sources" && i+1 < len(args) && args[i+1] == "" {
			sawEmptySources = true
		}
	}
	if !sawEmptySources {
		t.Errorf("argv does not pass --setting-sources \"\" — MAIN's hooks fire inside the worker session:\n%q", args)
	}
	for _, escape := range []string{"Task", "Workflow", "Skill", "CronCreate"} {
		if !strings.Contains(argv, escape) {
			t.Errorf("%s is not denied — the worker can hand its own job on", escape)
		}
	}
	for _, required := range []string{"--strict-mcp-config", "--dangerously-skip-permissions", "--output-format stream-json"} {
		if !strings.Contains(argv, required) {
			t.Errorf("argv is missing %s:\n%s", required, argv)
		}
	}
}

// #42/#24: rig's own identity must not travel down, or it comes back as tool
// denials the worker cannot diagnose.
func TestChildEnvStripsRigVariables(t *testing.T) {
	t.Setenv("RIG_AGENT_ID", "main")
	t.Setenv("RIG_STATE_DIR", "/somewhere")
	t.Setenv("PATH", os.Getenv("PATH"))

	var base, key string
	for _, kv := range childEnv("http://127.0.0.1:4010/") {
		if strings.HasPrefix(kv, "RIG_") {
			t.Errorf("%s leaked into the worker environment", kv)
		}
		if v, ok := strings.CutPrefix(kv, "ANTHROPIC_BASE_URL="); ok {
			base = v
		}
		if v, ok := strings.CutPrefix(kv, "ANTHROPIC_API_KEY="); ok {
			key = v
		}
	}
	if base != "http://127.0.0.1:4010/" {
		t.Errorf("ANTHROPIC_BASE_URL = %q", base)
	}
	if key == "" {
		t.Error("ANTHROPIC_API_KEY is unset; headless claude refuses to start")
	}
}

// Without a local endpoint the run would bill Anthropic from the leg that exists
// to avoid exactly that. It is refused, not attempted.
func TestRunRefusesWithoutALocalBaseURL(t *testing.T) {
	t.Setenv("RIG_CC_BASE_URL", "")
	t.Setenv("RIG_THIN_LOG_ROOT", t.TempDir())
	out := Run(context.Background(), t.TempDir(), "anything")
	if !strings.Contains(out.Status, "RIG_CC_BASE_URL is empty") {
		t.Fatalf("status = %q, want a refusal naming RIG_CC_BASE_URL", out.Status)
	}
}

// A cancellation names the call it cancels; a second run must not die because
// the first one was cancelled.
func TestIDKeyMatchesAcrossWhitespace(t *testing.T) {
	if idKey(json.RawMessage(" 2 ")) != idKey(json.RawMessage("2")) {
		t.Error("the same numeric id did not match across whitespace")
	}
	if idKey(json.RawMessage(`"abc"`)) == idKey(json.RawMessage("2")) {
		t.Error("different ids matched")
	}
}
