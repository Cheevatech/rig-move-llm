package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/config"
)

// ccTestRepo builds a throwaway git repo with one committed file, so the fake
// worker's edit shows up in collectDiff.
func ccTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	run := func(args ...string) {
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "init")
	return repo
}

// ccFakeBin writes a stand-in for `claude` that records its argv and env, makes
// one edit in the repo (its cwd), and plays back the given stream-json bytes.
func ccFakeBin(t *testing.T, dir, stream string) (bin, outDir string) {
	t.Helper()
	outDir = t.TempDir()
	streamFile := filepath.Join(outDir, "stream.jsonl")
	if err := os.WriteFile(streamFile, []byte(stream), 0o644); err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(dir, "fake-claude")
	script := "#!/bin/bash\n" +
		"printf '%s\\n' \"$@\" > '" + outDir + "/args.txt'\n" +
		"echo \"$ANTHROPIC_BASE_URL $ANTHROPIC_API_KEY\" > '" + outDir + "/env.txt'\n" +
		"echo edited-by-worker >> file.txt\n" +
		"cat '" + streamFile + "'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, outDir
}

const ccHappyStream = `{"type":"system","subtype":"init","session_id":"s"}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"git diff"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"NOT-A-GATE"}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"p1","name":"Bash","input":{"command":"python -m pytest -q rig_proof_test.py"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"p1","is_error":true,"content":"1 failed in 0.01s"}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"p2","name":"Bash","input":{"command":"python -m pytest -q rig_proof_test.py"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"p2","content":"1 passed in 0.01s"}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"python -m pytest tests/test_x.py -q"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":[{"type":"text","text":"1 passed in 0.01s"}]}]}}
{"type":"result","subtype":"success","result":"Fixed the condition and verified.","num_turns":4,"usage":{"input_tokens":111,"output_tokens":22},"total_cost_usd":0}
`

// ccNoProofStream is ccHappyStream minus the proof-of-flip pair: the worker
// claims done, gate green, but never demonstrated red->green (V6 false-accept
// shape).
const ccNoProofStream = `{"type":"system","subtype":"init","session_id":"s"}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"python -m pytest tests/test_x.py -q"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":[{"type":"text","text":"1 passed in 0.01s"}]}]}}
{"type":"result","subtype":"success","result":"Fixed the condition and verified.","num_turns":4,"usage":{"input_tokens":111,"output_tokens":22},"total_cost_usd":0}
`

func ccEnv(t *testing.T, bin string) {
	t.Helper()
	t.Setenv("RIG_WORKER_ENGINE", "cc")
	t.Setenv("RIG_CC_BIN", bin)
	t.Setenv("RIG_CC_BASE_URL", "http://127.0.0.1:9/worker-leg")
}

func TestCCImplementHappyPath(t *testing.T) {
	repo := ccTestRepo(t)
	bin, outDir := ccFakeBin(t, t.TempDir(), ccHappyStream)
	ccEnv(t, bin)
	archive := filepath.Join(outDir, "prompt.txt")
	t.Setenv("RIG_CC_PROMPT_ARCHIVE", archive)

	e := NewEngine(config.Config{})
	res := e.Implement(context.Background(), repo, "fix the bug", "")

	if res.Stopped != "done" || res.Err != "" {
		t.Fatalf("stopped=%q err=%q", res.Stopped, res.Err)
	}
	if res.Summary != "Fixed the condition and verified." {
		t.Errorf("summary=%q", res.Summary)
	}
	if res.Iterations != 4 || res.InputTokens != 111 || res.OutputTokens != 22 {
		t.Errorf("iters=%d in=%d out=%d", res.Iterations, res.InputTokens, res.OutputTokens)
	}
	// Gate pick: the pytest output, never the `git diff` inspection result.
	if res.LastTest != "1 passed in 0.01s" {
		t.Errorf("last_test=%q", res.LastTest)
	}
	if !strings.Contains(res.Diff, "edited-by-worker") || len(res.FilesChanged) != 1 {
		t.Errorf("diff did not capture the worker's edit: files=%v", res.FilesChanged)
	}

	argv, err := os.ReadFile(filepath.Join(outDir, "args.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"--disallowedTools\nWebFetch,WebSearch", // frozen map9 posture on the worker leg
		"--strict-mcp-config",
		"--dangerously-skip-permissions",
		"--model\nhaiku",
		"--output-format\nstream-json",
		"-p\nfix the bug",
	} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("argv missing %q:\n%s", want, argv)
		}
	}
	env, _ := os.ReadFile(filepath.Join(outDir, "env.txt"))
	if strings.TrimSpace(string(env)) != "http://127.0.0.1:9/worker-leg sk-rig-cc-worker-local" {
		t.Errorf("subprocess base/key = %q", env)
	}
	arch, err := os.ReadFile(archive)
	if err != nil || !strings.Contains(string(arch), "fix the bug") {
		t.Errorf("prompt archive missing or incomplete: %v", err)
	}
}

func TestCCRefusesWithoutBaseURL(t *testing.T) {
	repo := ccTestRepo(t)
	bin, _ := ccFakeBin(t, t.TempDir(), ccHappyStream)
	ccEnv(t, bin)
	t.Setenv("RIG_CC_BASE_URL", "")

	res := NewEngine(config.Config{}).Implement(context.Background(), repo, "fix", "")
	if res.Stopped != "error" || !strings.Contains(res.Err, "RIG_CC_BASE_URL") {
		t.Fatalf("expected money-safety refusal, got stopped=%q err=%q", res.Stopped, res.Err)
	}
	// Refusal must happen before any launch: the repo stays untouched.
	if res.Diff != "" {
		t.Errorf("worker ran despite refusal: diff=%q", res.Diff)
	}
}

func TestCCStreamWithoutResultEventIsAnError(t *testing.T) {
	repo := ccTestRepo(t)
	bin, _ := ccFakeBin(t, t.TempDir(), `{"type":"system","subtype":"init"}`+"\n")
	ccEnv(t, bin)

	res := NewEngine(config.Config{}).Implement(context.Background(), repo, "fix", "")
	if res.Stopped != "error" || !strings.Contains(res.Err, "without a result event") {
		t.Fatalf("stopped=%q err=%q", res.Stopped, res.Err)
	}
	// The diff is still collected — whatever landed on disk is evidence.
	if !strings.Contains(res.Diff, "edited-by-worker") {
		t.Errorf("diff not collected on error path")
	}
}

func TestCCMaxTurnsMapsToIterationCap(t *testing.T) {
	repo := ccTestRepo(t)
	stream := `{"type":"result","subtype":"error_max_turns","result":"","num_turns":30,"usage":{"input_tokens":1,"output_tokens":1}}` + "\n"
	bin, _ := ccFakeBin(t, t.TempDir(), stream)
	ccEnv(t, bin)

	res := NewEngine(config.Config{}).Implement(context.Background(), repo, "fix", "")
	if res.Stopped != "max_iters" || !res.HitIterationCap {
		t.Fatalf("stopped=%q cap=%v", res.Stopped, res.HitIterationCap)
	}
}

func TestCCEngineOffKeepsLoop(t *testing.T) {
	// With the switch unset AND no cc base URL, Implement must NOT reach for the
	// CC binary: it runs the 3-tool loop, which here fails fast on an
	// unreachable endpoint.
	repo := ccTestRepo(t)
	t.Setenv("RIG_WORKER_ENGINE", "")
	t.Setenv("RIG_CC_BASE_URL", "")
	t.Setenv("RIG_CC_BIN", "/nonexistent/definitely-not-run")
	e := NewEngine(config.Config{WorkerAPIBase: "http://127.0.0.1:1/v1"})
	res := e.Implement(context.Background(), repo, "fix", "")
	if !strings.Contains(res.Err, "chat:") {
		t.Fatalf("expected the 3-tool loop's chat error, got %q", res.Err)
	}
}

// The P4 auto-default: with the switch UNSET but a cc base URL configured, the
// cc engine runs — the user who wired the prerequisites gets the engine that
// won the flip gate without touching RIG_WORKER_ENGINE.
func TestCCAutoDefaultWithBaseURL(t *testing.T) {
	repo := ccTestRepo(t)
	bin, _ := ccFakeBin(t, t.TempDir(), ccHappyStream)
	t.Setenv("RIG_WORKER_ENGINE", "")
	t.Setenv("RIG_CC_BIN", bin)
	t.Setenv("RIG_CC_BASE_URL", "http://127.0.0.1:9/worker-leg")

	res := NewEngine(config.Config{}).Implement(context.Background(), repo, "fix", "")
	if res.Stopped != "done" || !strings.Contains(res.Summary, "Fixed the condition") {
		t.Fatalf("auto-default did not run the cc engine: stopped=%q summary=%q", res.Stopped, res.Summary)
	}
}

// An explicit non-cc value always forces the loop, even with a base URL set —
// the documented off switch after the auto-default flip.
func TestCCExplicitLoopBeatsAutoDefault(t *testing.T) {
	repo := ccTestRepo(t)
	t.Setenv("RIG_WORKER_ENGINE", "loop")
	t.Setenv("RIG_CC_BASE_URL", "http://127.0.0.1:9/worker-leg")
	t.Setenv("RIG_CC_BIN", "/nonexistent/definitely-not-run")
	e := NewEngine(config.Config{WorkerAPIBase: "http://127.0.0.1:1/v1"})
	res := e.Implement(context.Background(), repo, "fix", "")
	if !strings.Contains(res.Err, "chat:") {
		t.Fatalf("explicit loop must beat the auto-default, got %q", res.Err)
	}
}

func TestCCIsGateCommand(t *testing.T) {
	cases := map[string]bool{
		"git diff":                               false,
		"git apply /tmp/x.patch":                 false,
		"ls -la && cat foo":                      false,
		"python -m pytest -q":                    true,
		"cd sympy && python -m pytest":           true,
		"PYTHONPATH=. pytest -q | tail -5":       true,
		"./.venv/bin/python -m pytest -q":        true,
		"grep -q PASS out && echo PASS":          false,
		"./tests/runtests.py i18n --verbosity=0": true,
	}
	for cmd, want := range cases {
		if got := ccIsGateCommand(cmd); got != want {
			t.Errorf("ccIsGateCommand(%q)=%v want %v", cmd, got, want)
		}
	}
}

// Version-skew guard (map10 P2): a claude CLI whose output is not stream-json —
// an older binary that does not know --output-format, a changed format — must
// surface as a diagnosis naming the suspected skew, never as a silent empty
// return or a bare "no result event".
func TestCCNonJSONOutputDiagnosesVersionSkew(t *testing.T) {
	repo := ccTestRepo(t)
	bin, _ := ccFakeBin(t, t.TempDir(),
		"error: unknown option '--output-format'\nUsage: claude [options] [command]\n")
	ccEnv(t, bin)

	res := NewEngine(config.Config{}).Implement(context.Background(), repo, "fix", "")
	if res.Stopped != "error" || !strings.Contains(res.Err, "version skew") {
		t.Fatalf("stopped=%q err=%q", res.Stopped, res.Err)
	}
	if !strings.Contains(res.Err, "stream-json") {
		t.Errorf("the error should name the expected format, got %q", res.Err)
	}
}
