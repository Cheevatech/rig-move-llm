package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/pkg/translate"
)

// fakeBackend serves a scripted sequence of Chat Completions responses, one per
// call, so a test can drive the agentic loop deterministically.
func fakeBackend(t *testing.T, script []translate.OpenAIResponse) *httptest.Server {
	t.Helper()
	i := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if i >= len(script) {
			t.Fatalf("backend called more than scripted (%d)", len(script))
		}
		resp := script[i]
		i++
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func toolCallResp(id, name, args string) translate.OpenAIResponse {
	return translate.OpenAIResponse{
		Choices: []translate.OpenAIChoice{{
			Message: translate.OpenAIResponseMessage{
				Role: "assistant",
				ToolCalls: []translate.OpenAIToolCall{{
					ID: id, Type: "function",
					Function: translate.OpenAIToolCallFunction{Name: name, Arguments: args},
				}},
			},
			FinishReason: "tool_calls",
		}},
		Usage: &translate.OpenAIUsage{PromptTokens: 10, CompletionTokens: 5},
	}
}

func finalResp(text string) translate.OpenAIResponse {
	return translate.OpenAIResponse{
		Choices: []translate.OpenAIChoice{{
			Message:      translate.OpenAIResponseMessage{Role: "assistant", Content: text},
			FinishReason: "stop",
		}},
		Usage: &translate.OpenAIUsage{PromptTokens: 12, CompletionTokens: 3},
	}
}

// gitRepo makes a temp git repo with one committed file so `git diff` reflects
// the worker's edits.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("def f():\n    return 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "base")
	return dir
}

func TestImplement_FullCycle(t *testing.T) {
	repo := gitRepo(t)
	be := fakeBackend(t, []translate.OpenAIResponse{
		toolCallResp("c1", "read_file", `{"path":"app.py"}`),
		toolCallResp("c2", "write_file", `{"path":"app.py","content":"def f():\n    return 2\n"}`),
		toolCallResp("c3", "run_bash", `{"command":"grep -q 'return 2' app.py && echo PASS"}`),
		finalResp("Changed f() to return 2."),
	})
	defer be.Close()

	cfg := config.Config{WorkerAPIBase: be.URL, WorkerModel: "test"}
	res := NewEngine(cfg).Implement(context.Background(), repo, "make f return 2", "")

	if res.Stopped != "done" {
		t.Fatalf("stopped=%q err=%q", res.Stopped, res.Err)
	}
	if res.Summary != "Changed f() to return 2." {
		t.Errorf("summary=%q", res.Summary)
	}
	got, _ := os.ReadFile(filepath.Join(repo, "app.py"))
	if !strings.Contains(string(got), "return 2") {
		t.Errorf("file not edited: %s", got)
	}
	if len(res.FilesChanged) != 1 || res.FilesChanged[0] != "app.py" {
		t.Errorf("files_changed=%v", res.FilesChanged)
	}
	if res.Diff == "" {
		t.Error("expected non-empty diff")
	}
	if !strings.Contains(res.LastTest, "PASS") {
		t.Errorf("last_test=%q", res.LastTest)
	}
	if res.InputTokens == 0 || res.OutputTokens == 0 {
		t.Errorf("token accounting empty in=%d out=%d", res.InputTokens, res.OutputTokens)
	}
}

func TestImplement_RejectsGateWrite(t *testing.T) {
	repo := gitRepo(t)
	be := fakeBackend(t, []translate.OpenAIResponse{
		toolCallResp("c1", "write_file", `{"path":".gate/repro.py","content":"x"}`),
		finalResp("done"),
	})
	defer be.Close()

	cfg := config.Config{WorkerAPIBase: be.URL, WorkerModel: "test"}
	res := NewEngine(cfg).Implement(context.Background(), repo, "task", "")
	if res.Stopped != "done" {
		t.Fatalf("stopped=%q", res.Stopped)
	}
	if _, err := os.Stat(filepath.Join(repo, ".gate", "repro.py")); !os.IsNotExist(err) {
		t.Error("gate file should not have been written")
	}
}

func TestSafeJoin_Escape(t *testing.T) {
	repo := t.TempDir()
	for _, bad := range []string{"../etc/passwd", "/etc/passwd", "a/../../b"} {
		if _, err := safeJoin(repo, bad); err == nil {
			t.Errorf("safeJoin allowed escape: %q", bad)
		}
	}
	if _, err := safeJoin(repo, "sub/ok.txt"); err != nil {
		t.Errorf("safeJoin rejected valid path: %v", err)
	}
}

func TestMCP_Handshake_and_ToolsList(t *testing.T) {
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}` + "\n" +
			`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n")
	var out bytes.Buffer
	if err := Serve(config.Config{}, in, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 responses (init, tools/list), got %d: %s", len(lines), out.String())
	}
	var initR rpcResponse
	if err := json.Unmarshal([]byte(lines[0]), &initR); err != nil {
		t.Fatal(err)
	}
	if initR.Error != nil {
		t.Fatalf("initialize error: %+v", initR.Error)
	}
	if !strings.Contains(lines[1], `"implement"`) {
		t.Errorf("tools/list missing implement: %s", lines[1])
	}
}
