package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Cheevatech/rig-move-llm/pkg/translate"
)

// toolSchema is the fixed set of tools exposed to the worker model. Kept minimal
// on purpose: a file read, a file write, and a shell — enough to reproduce, edit,
// and verify, and nothing that needs bespoke server-side state.
func toolSchema() []translate.OpenAITool {
	return []translate.OpenAITool{
		{Type: "function", Function: translate.OpenAIToolFunction{
			Name:        "read_file",
			Description: "Read a UTF-8 text file from the repo. path is relative to the repo root.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}},
		{Type: "function", Function: translate.OpenAIToolFunction{
			Name:        "write_file",
			Description: "Create or overwrite a text file in the repo with the given full content. path is relative to the repo root.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
		}},
		{Type: "function", Function: translate.OpenAIToolFunction{
			Name:        "run_bash",
			Description: "Run a bash command from the repo root and return combined stdout+stderr and the exit code. Use it to run the failing test and verify the fix.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
		}},
	}
}

// execTool dispatches one tool call against the repo. It never returns an error
// value: tool failures are reported back to the model as text so it can react,
// which is how OpenAI-style tool loops recover.
func (e *Engine) execTool(ctx context.Context, repo string, tc translate.OpenAIToolCall) string {
	switch tc.Function.Name {
	case "read_file":
		var a struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &a); err != nil {
			return "error: bad arguments: " + err.Error()
		}
		p, err := safeJoin(repo, a.Path)
		if err != nil {
			return "error: " + err.Error()
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return "error: " + err.Error()
		}
		return string(data)

	case "write_file":
		var a struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &a); err != nil {
			return "error: bad arguments: " + err.Error()
		}
		p, err := safeJoin(repo, a.Path)
		if err != nil {
			return "error: " + err.Error()
		}
		if isGatePath(repo, p) {
			return "error: refusing to write under the frozen gate contract (.gate/)"
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "error: " + err.Error()
		}
		if err := os.WriteFile(p, []byte(a.Content), 0o644); err != nil {
			return "error: " + err.Error()
		}
		return fmt.Sprintf("wrote %s (%d bytes)", a.Path, len(a.Content))

	case "run_bash":
		var a struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &a); err != nil {
			return "error: bad arguments: " + err.Error()
		}
		return e.runBash(ctx, repo, a.Command)

	default:
		return "error: unknown tool " + tc.Function.Name
	}
}

func (e *Engine) runBash(ctx context.Context, repo, command string) string {
	cctx, cancel := context.WithTimeout(ctx, e.bashTO)
	defer cancel()
	cmd := exec.CommandContext(cctx, "bash", "-lc", command)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	body := string(out)
	if cctx.Err() == context.DeadlineExceeded {
		return body + fmt.Sprintf("\n[killed: exceeded %s timeout]", e.bashTO)
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return body + fmt.Sprintf("\n[exit %d]", ee.ExitCode())
		}
		return body + "\n[run error: " + err.Error() + "]"
	}
	return body + "\n[exit 0]"
}

// safeJoin resolves rel against repo and refuses any path that escapes the repo
// (via .. or an absolute path). It returns the cleaned absolute path.
func safeJoin(repo, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("empty path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("path must be relative to the repo root: %s", rel)
	}
	p := filepath.Clean(filepath.Join(repo, rel))
	if p != repo && !strings.HasPrefix(p, repo+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes the repo root: %s", rel)
	}
	return p, nil
}

// isGatePath reports whether abs is inside the repo's frozen gate contract dirs.
func isGatePath(repo, abs string) bool {
	rel, err := filepath.Rel(repo, abs)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	return rel == ".gate" || strings.HasPrefix(rel, ".gate/") ||
		rel == ".gate.frozen" || strings.HasPrefix(rel, ".gate.frozen/")
}
