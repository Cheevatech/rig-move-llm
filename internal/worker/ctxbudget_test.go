package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/pkg/translate"
)

func TestOverCtxBudget(t *testing.T) {
	cases := []struct {
		prompt, limit int
		want          bool
	}{
		{47999, 48000, false},
		{48000, 48000, true},
		{48001, 48000, true},
		{999999, 0, false}, // limit<=0 disables the watcher
		{0, 48000, false},
	}
	for _, c := range cases {
		if got := overCtxBudget(c.prompt, c.limit); got != c.want {
			t.Errorf("overCtxBudget(%d,%d)=%v want %v", c.prompt, c.limit, got, c.want)
		}
	}
}

func TestCtxLimitEnv(t *testing.T) {
	if got := ctxLimit(); got != defaultCtxLimit {
		t.Fatalf("default ctxLimit=%d want %d", got, defaultCtxLimit)
	}
	t.Setenv("RIG_WORKER_CTX_LIMIT", "12345")
	if got := ctxLimit(); got != 12345 {
		t.Fatalf("env ctxLimit=%d want 12345", got)
	}
}

// withUsage overrides the prompt-token usage a scripted response reports, so a
// test can trip (or not trip) the context budget on chosen turns.
func withUsage(r translate.OpenAIResponse, promptTokens int) translate.OpenAIResponse {
	r.Usage = &translate.OpenAIUsage{PromptTokens: promptTokens, CompletionTokens: 5}
	return r
}

// recordingBackend is fakeBackend plus a capture of every request's messages, so
// a test can assert what conversation the engine actually sent each turn.
func recordingBackend(t *testing.T, script []translate.OpenAIResponse) (*httptest.Server, *[][]translate.OpenAIMessage) {
	t.Helper()
	var seen [][]translate.OpenAIMessage
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req translate.OpenAIRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		seen = append(seen, req.Messages)
		if i >= len(script) {
			t.Fatalf("backend called more than scripted (%d)", len(script))
		}
		resp := script[i]
		i++
		_ = json.NewEncoder(w).Encode(resp)
	}))
	return srv, &seen
}

func msgText(m translate.OpenAIMessage) string { return fmt.Sprint(m.Content) }

// TestImplement_CtxCheckpointAccumulates drives the implement loop across THREE
// context checkpoints and asserts the option-B invariants: on each trip the
// conversation is reset to exactly [system, rig-assembled digest]; the digest
// carries the ACCUMULATED git diff from disk (work from before earlier
// checkpoints is still visible after later ones); the files-read record survives
// resets; and the final Result reports the full diff plus the checkpoint count.
func TestImplement_CtxCheckpointAccumulates(t *testing.T) {
	repo := gitRepo(t)
	t.Setenv("RIG_WORKER_CTX_LIMIT", "100")

	over := 200 // > limit -> checkpoint after the turn's tools run
	srv, seen := recordingBackend(t, []translate.OpenAIResponse{
		// turn 1: read the file; budget trips -> checkpoint #1 (no diff yet)
		withUsage(toolCallResp("c1", "read_file", `{"path":"app.py"}`), over),
		// turn 2: first edit; budget trips -> checkpoint #2 (diff: return 2)
		withUsage(toolCallResp("c2", "write_file", `{"path":"app.py","content":"def f():\n    return 2\n"}`), over),
		// turn 3: second edit on top; budget trips -> checkpoint #3 (diff: both edits)
		withUsage(toolCallResp("c3", "write_file", `{"path":"app.py","content":"def f():\n    return 2\n\ndef g():\n    return 3\n"}`), over),
		// turn 4: verify; small usage -> no checkpoint
		withUsage(toolCallResp("c4", "run_bash", `{"command":"grep -q 'return 3' app.py && echo PASS"}`), 10),
		finalResp("Applied both edits."),
	})
	defer srv.Close()

	cfg := config.Config{WorkerAPIBase: srv.URL, WorkerModel: "test"}
	res := NewEngine(cfg).Implement(context.Background(), repo, "make f return 2 and add g", "")

	if res.Stopped != "done" {
		t.Fatalf("stopped=%q err=%q", res.Stopped, res.Err)
	}
	if res.Checkpoints != 3 {
		t.Fatalf("checkpoints=%d want 3", res.Checkpoints)
	}
	if !strings.Contains(res.Diff, "return 2") || !strings.Contains(res.Diff, "return 3") {
		t.Fatalf("final diff must carry both edits:\n%s", res.Diff)
	}
	if !strings.Contains(res.LastTest, "PASS") {
		t.Fatalf("last_test=%q", res.LastTest)
	}

	reqs := *seen
	if len(reqs) != 5 {
		t.Fatalf("expected 5 requests, got %d", len(reqs))
	}
	// After each checkpoint the next request must be a fresh 2-message
	// conversation: [system, rig-assembled digest].
	for turn := 1; turn <= 3; turn++ {
		msgs := reqs[turn]
		if len(msgs) != 2 || msgs[0].Role != "system" || msgs[1].Role != "user" {
			t.Fatalf("request %d after checkpoint: want [system,user], got %d msgs", turn+1, len(msgs))
		}
		if !strings.Contains(msgText(msgs[1]), "CONTEXT CHECKPOINT") {
			t.Fatalf("request %d digest missing checkpoint preamble: %s", turn+1, msgText(msgs[1]))
		}
	}
	// Checkpoint #1 fired before any edit: digest must say so, and must already
	// carry the files-read record.
	d1 := msgText(reqs[1][1])
	if !strings.Contains(d1, "no changes written to disk yet") {
		t.Fatalf("digest 1 should report empty diff: %s", d1)
	}
	if !strings.Contains(d1, "FILES ALREADY READ") || !strings.Contains(d1, "app.py") {
		t.Fatalf("digest 1 missing files-read record: %s", d1)
	}
	// Checkpoint #2: diff shows the first edit.
	d2 := msgText(reqs[2][1])
	if !strings.Contains(d2, "return 2") {
		t.Fatalf("digest 2 missing first edit: %s", d2)
	}
	// Checkpoint #3: diff has ACCUMULATED both edits, and the read record —
	// captured before checkpoint #1 — still survives.
	d3 := msgText(reqs[3][1])
	if !strings.Contains(d3, "return 2") || !strings.Contains(d3, "return 3") {
		t.Fatalf("digest 3 must accumulate both edits: %s", d3)
	}
	if !strings.Contains(d3, "FILES ALREADY READ") || !strings.Contains(d3, "app.py") {
		t.Fatalf("digest 3 lost the files-read record: %s", d3)
	}
	// Turn 4 stayed under budget, so the final request continues the SAME
	// conversation (digest + assistant + tool result + …), not another reset.
	if len(reqs[4]) <= 2 {
		t.Fatalf("no checkpoint after under-budget turn: want a grown conversation, got %d msgs", len(reqs[4]))
	}
}

// TestImplement_NoCheckpointUnderBudget: with the default 48k limit and tiny
// scripted usage, the loop must never reset.
func TestImplement_NoCheckpointUnderBudget(t *testing.T) {
	repo := gitRepo(t)
	srv, seen := recordingBackend(t, []translate.OpenAIResponse{
		toolCallResp("c1", "write_file", `{"path":"app.py","content":"def f():\n    return 2\n"}`),
		finalResp("done"),
	})
	defer srv.Close()
	res := NewEngine(config.Config{WorkerAPIBase: srv.URL, WorkerModel: "test"}).
		Implement(context.Background(), repo, "task", "")
	if res.Checkpoints != 0 {
		t.Fatalf("checkpoints=%d want 0", res.Checkpoints)
	}
	if res.Stopped != "done" {
		t.Fatalf("stopped=%q", res.Stopped)
	}
	if got := len((*seen)[1]); got != 4 { // system, user, assistant, tool
		t.Fatalf("conversation should grow normally, got %d msgs", got)
	}
}

// TestExploreTokenBudgetEndsRoundEarly: the explore round's PRIMARY trigger is
// the real context size, not the iteration count — with a tiny token budget the
// checkpoint prompt must appear on the very next turn even though the iteration
// cap (default 14) is nowhere near.
func TestExploreTokenBudgetEndsRoundEarly(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("RIG_STATE_DIR", stateDir)
	t.Setenv("RIG_WORKER_CTX_LIMIT", "100")
	repo := exploreRepo(t)

	srv, seen := recordingBackend(t, []translate.OpenAIResponse{
		withUsage(toolCallResp("c1", "read_file", `{"path":"util.py"}`), 200), // trips the budget
		finalResp(goodReportJSON),
	})
	defer srv.Close()
	e := NewEngine(config.Config{WorkerAPIBase: srv.URL})
	res := e.Explore(context.Background(), repo, exploreTask)

	if res.Stopped != "done" {
		t.Fatalf("stopped=%s err=%s", res.Stopped, res.Err)
	}
	if res.Iterations >= roundIters {
		t.Fatalf("token budget should have ended the round long before the iteration cap (iters=%d)", res.Iterations)
	}
	reqs := *seen
	if len(reqs) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(reqs))
	}
	last := reqs[1][len(reqs[1])-1]
	if last.Role != "user" || !strings.Contains(msgText(last), "CHECKPOINT") {
		t.Fatalf("budget trip must inject the round-checkpoint prompt, got role=%s content=%s", last.Role, msgText(last))
	}
	if _, err := os.Stat(filepath.Join(stateDir, "explore_last.json")); err != nil {
		t.Fatalf("explore_last.json debug dump missing: %v", err)
	}
}
