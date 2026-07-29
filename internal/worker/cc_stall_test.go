package worker

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/config"
)

// The invariant #18 was filed against: the engine's own wall must expire BEFORE
// the calling client's MCP progress watchdog (Claude Code aborts a silent tool
// call at 1800s). If this fails, a long round is killed by the client again and
// rig never gets to return a diagnosis.
func TestRunTimeoutStaysUnderClientWatchdog(t *testing.T) {
	const clientWatchdog = 1800 * time.Second
	if got := runTimeout(); got >= clientWatchdog {
		t.Fatalf("runTimeout()=%s must stay below the %s client MCP watchdog", got, clientWatchdog)
	}
}

func TestCCTimeoutDiagnosis(t *testing.T) {
	t.Run("neither guard fired", func(t *testing.T) {
		msg, stopped := ccTimeoutDiagnosis(context.Background(), newCCActivity())
		if msg != "" || stopped != "" {
			t.Fatalf("want no diagnosis, got stopped=%q msg=%q", stopped, msg)
		}
	})

	t.Run("stall guard names what it was last doing", func(t *testing.T) {
		act := newCCActivity()
		act.touch("Bash ./.venv/bin/python -m pytest -q")
		act.markStalled()
		msg, stopped := ccTimeoutDiagnosis(context.Background(), act)
		if stopped != "timeout" {
			t.Fatalf("stopped = %q, want timeout", stopped)
		}
		for _, want := range []string{"stall guard", "pytest", "Do NOT simply re-delegate"} {
			if !strings.Contains(msg, want) {
				t.Errorf("diagnosis missing %q:\n%s", want, msg)
			}
		}
	})

	t.Run("wall guard fires on an expired context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		msg, stopped := ccTimeoutDiagnosis(ctx, newCCActivity())
		if stopped != "timeout" {
			t.Fatalf("stopped = %q, want timeout", stopped)
		}
		if !strings.Contains(msg, "wall guard") || !strings.Contains(msg, "no stream output at all") {
			t.Errorf("diagnosis does not describe the wall stall:\n%s", msg)
		}
	})
}

func TestCCToolDesc(t *testing.T) {
	cases := []struct {
		name  string
		block ccBlock
		want  string
	}{
		{"bash carries the command", ccBlock{Name: "Bash", Input: []byte(`{"command":"pytest  -q\n"}`)}, "Bash pytest -q"},
		{"edit carries the path", ccBlock{Name: "Edit", Input: []byte(`{"file_path":"README.md"}`)}, "Edit README.md"},
		{"no argument leaves the name", ccBlock{Name: "TodoWrite", Input: []byte(`{}`)}, "TodoWrite"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ccToolDesc(c.block); got != c.want {
				t.Fatalf("ccToolDesc = %q, want %q", got, c.want)
			}
		})
	}
}

// The end-to-end shape of #18: a worker that emits some stream, then goes silent
// forever. Before the guard this call blocked until the client gave up at 1800s
// with nothing to show; now the engine kills it and returns a diagnosis.
func TestRunCCOnceKillsAStalledWorker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake worker is a shell script")
	}
	repo := t.TempDir()
	bin := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/sh\n" +
		`echo '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"1","name":"Edit","input":{"file_path":"README.md"}}]}}'` + "\n" +
		"sleep 120\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RIG_CC_STALL_TIMEOUT", "1")
	e := NewEngine(config.Config{})
	var res Result
	start := time.Now()
	sawResult := e.runCCOnce(context.Background(), bin, "http://127.0.0.1:1/v1", repo, "task", &res, &ccProof{}, true)

	if sawResult {
		t.Fatal("a stalled worker produced no result event; sawResult must be false")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("stall guard did not fire: call took %s", elapsed)
	}
	if res.Stopped != "timeout" {
		t.Fatalf("stopped = %q (err=%q), want timeout", res.Stopped, res.Err)
	}
	if !strings.Contains(res.Err, "stall guard") || !strings.Contains(res.Err, "Edit README.md") {
		t.Fatalf("diagnosis must name the guard and the last activity, got:\n%s", res.Err)
	}
	if !res.Failed() {
		t.Error("a timed-out round must be reported to the caller as a failure")
	}
}

// A worker that streams normally must not be touched by the guard.
func TestRunCCOnceLetsALiveWorkerFinish(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake worker is a shell script")
	}
	repo := t.TempDir()
	bin := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/sh\n" +
		"i=0\n" +
		"while [ $i -lt 6 ]; do\n" +
		`  echo '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"1","name":"Bash","input":{"command":"pytest -q"}}]}}'` + "\n" +
		"  sleep 0.2\n  i=$((i+1))\n" +
		"done\n" +
		`echo '{"type":"result","subtype":"success","result":"fixed it","num_turns":3}'` + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RIG_CC_STALL_TIMEOUT", "1")
	e := NewEngine(config.Config{})
	var res Result
	if !e.runCCOnce(context.Background(), bin, "http://127.0.0.1:1/v1", repo, "task", &res, &ccProof{}, true) {
		t.Fatal("a streaming worker must reach its result event")
	}
	if res.Stopped != "done" || res.Summary != "fixed it" {
		t.Fatalf("stopped=%q summary=%q, want done/fixed it (err=%q)", res.Stopped, res.Summary, res.Err)
	}
}
