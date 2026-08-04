package thin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests are the in-repo half of G3's acceptance: the scratchpad harness
// proves it end to end against a built binary, and these keep it from silently
// regressing afterwards. They exercise the kill path ONLY — `claude` is a shell
// script that sleeps and spawns a child, so there is no model, no qwen, and no
// token anywhere in this file.
//
// The four ways a client can try to stop a run, each measured on the same
// question G1 asked: is the process tree actually gone?
//
//	A. notifications/cancelled  -> TestCancelledKillsWorkerTree
//	B. SIGTERM the server       -> covered by ctx cancellation (same path as A's ctx)
//	C. SIGKILL the server       -> TestSupervisorKillsTreeWhenOrphaned
//	D. stdin EOF (client hangs up) -> TestStdinEOFKillsWorkerTree

// TestMain doubles as the supervisor entry point: when the test binary is
// re-executed as the supervisor (see RIG_THIN_SUPERVISOR_BIN), it must behave
// like `rig thin-supervise` rather than run tests.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "thin-supervise" {
		os.Exit(Supervise(os.Args[2:]))
	}
	// A stand-in for the MCP server process: it starts one run and then only
	// exists to be killed. It has to be a real separate process, because the case
	// it demonstrates is the one where the server gets no chance to react.
	if len(os.Args) > 2 && os.Args[1] == "thin-run-helper" {
		Run(context.Background(), os.Args[2], "orphan probe")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeClaude writes a stub that records its pid and its child's, then sleeps
// well past any test's patience. It stands in for `claude -p` spawning a test
// runner, so the kill path is measured through a whole tree.
func fakeClaude(t *testing.T, pidFile string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/sh\n" +
		"echo \"CLAUDE_PID $$\" >> " + pidFile + "\n" +
		"( sleep 900 ) &\n" +
		"echo \"GRANDCHILD_PID $!\" >> " + pidFile + "\n" +
		"sleep 900\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// awaitPids waits for the stub to report both pids.
func awaitPids(t *testing.T, pidFile string) (child, grandchild int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		pids := map[string]int{}
		if b, err := os.ReadFile(pidFile); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				f := strings.Fields(line)
				if len(f) == 2 {
					n, _ := strconv.Atoi(f[1])
					pids[f[0]] = n
				}
			}
		}
		if pids["CLAUDE_PID"] != 0 && pids["GRANDCHILD_PID"] != 0 {
			return pids["CLAUDE_PID"], pids["GRANDCHILD_PID"]
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("stub never reported its pids (%s)", pidFile)
	return 0, 0
}

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// requireDeadWithin is the acceptance bound from the ticket: gone in five
// seconds, tree and all.
func requireDeadWithin(t *testing.T, what string, limit time.Duration, pids ...int) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		allDead := true
		for _, p := range pids {
			if alive(p) {
				allDead = false
			}
		}
		if allDead {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, p := range pids {
		if alive(p) {
			_ = syscall.Kill(p, syscall.SIGKILL)
		}
	}
	t.Fatalf("%s: process tree still alive after %s", what, limit)
}

// thinEnv points a run at the stub and keeps its logs inside the test's tempdir.
func thinEnv(t *testing.T, stub, logRoot string) {
	t.Helper()
	t.Setenv("RIG_CC_BASE_URL", "http://127.0.0.1:9/") // non-empty: unlocks the run
	t.Setenv("RIG_CC_BIN", stub)
	t.Setenv("RIG_THIN_LOG_ROOT", logRoot)
	t.Setenv("RIG_THIN_SUPERVISOR_BIN", os.Args[0]) // supervise with the test binary
}

// gitRepo makes an empty checkout so diff collection has something to read.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

// A. The MCP-spec abort. G1's finding was that this notification was never even
// read while a call was running; the assertion here is that it is read, matched
// to its request id, and reaches the whole process tree.
func TestCancelledKillsWorkerTree(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pids")
	stub := fakeClaude(t, pidFile)
	thinEnv(t, stub, t.TempDir())
	repo := gitRepo(t)

	client := startServer(t)
	client.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`)
	client.readReply(t)
	client.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	client.send(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"implement","arguments":{"task":"kill path probe","repo":%q}}}`, repo))

	child, grandchild := awaitPids(t, pidFile)

	client.send(t, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2,"reason":"user pressed Esc"}}`)
	requireDeadWithin(t, "cancelled", 5*time.Second, child, grandchild)

	// A cancelled call still answers: the caller must learn what happened, and
	// any work already on disk still belongs to them.
	reply := client.readReply(t)
	text := replyText(t, reply)
	if !strings.Contains(text, "status: killed(") {
		t.Fatalf("cancelled call did not report itself killed:\n%s", text)
	}
}

// D. The client hangs up. Closing stdin used to leave the tree running with
// nobody to report to.
func TestStdinEOFKillsWorkerTree(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pids")
	stub := fakeClaude(t, pidFile)
	thinEnv(t, stub, t.TempDir())
	repo := gitRepo(t)

	client := startServer(t)
	client.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`)
	client.readReply(t)
	client.send(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"implement","arguments":{"task":"kill path probe","repo":%q}}}`, repo))

	child, grandchild := awaitPids(t, pidFile)
	client.closeStdin()
	requireDeadWithin(t, "stdin EOF", 5*time.Second, child, grandchild)
}

// B/C together: the server is gone without a chance to clean up. Nothing inside
// the server can help here, so the assertion is on the supervisor — the one
// process left that can notice it has been orphaned.
func TestSupervisorKillsTreeWhenOrphaned(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pids")
	stub := fakeClaude(t, pidFile)
	thinEnv(t, stub, t.TempDir())
	repo := gitRepo(t)

	server := exec.Command(os.Args[0], "thin-run-helper", repo)
	server.Env = os.Environ()
	server.Stdout, server.Stderr = io.Discard, io.Discard
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = server.Process.Kill()
		_ = server.Wait()
	})

	child, grandchild := awaitPids(t, pidFile)

	// SIGKILL: no handler runs, no defer fires, nothing in the server gets a say.
	// Whatever cleans up now is the supervisor noticing it has been orphaned.
	if err := server.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	requireDeadWithin(t, "server SIGKILL", 5*time.Second, child, grandchild)
}

// The stall guard is the other half of "stop burning": nobody pressed anything,
// the worker simply went quiet.
func TestStallGuardKillsSilentWorker(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pids")
	stub := fakeClaude(t, pidFile)
	thinEnv(t, stub, t.TempDir())
	t.Setenv("RIG_THIN_STALL", "1")
	repo := gitRepo(t)

	done := make(chan Outcome, 1)
	go func() { done <- Run(context.Background(), repo, "silent worker") }()

	child, grandchild := awaitPids(t, pidFile)
	requireDeadWithin(t, "stall guard", 10*time.Second, child, grandchild)

	select {
	case out := <-done:
		if !strings.HasPrefix(out.Status, "killed(no output for") {
			t.Fatalf("status = %q, want killed(no output for ...)", out.Status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the stall guard fired")
	}
}

// A killed round must still hand back what reached the tree. A1 recorded 48
// minutes of work discarded because it did not.
func TestKilledRunStillReturnsTheDiff(t *testing.T) {
	repo := gitRepo(t)
	tracked := filepath.Join(repo, "kept.txt")
	if err := os.WriteFile(tracked, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	// A stub that edits the tree, then goes silent forever.
	pidFile := filepath.Join(t.TempDir(), "pids")
	stub := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/sh\n" +
		"echo \"CLAUDE_PID $$\" >> " + pidFile + "\n" +
		"( sleep 900 ) &\n" +
		"echo \"GRANDCHILD_PID $!\" >> " + pidFile + "\n" +
		"echo after > " + tracked + "\n" +
		"echo created > " + filepath.Join(repo, "new.txt") + "\n" +
		"sleep 900\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	thinEnv(t, stub, t.TempDir())
	t.Setenv("RIG_THIN_STALL", "1")

	out := Run(context.Background(), repo, "edit then hang")
	if !strings.HasPrefix(out.Status, "killed(") {
		t.Fatalf("status = %q, want killed(...)", out.Status)
	}
	if !strings.Contains(out.Diff, "after") {
		t.Errorf("the killed round lost the edit it had already written:\n%s", out.Diff)
	}
	if !strings.Contains(out.Diff, "new.txt") {
		t.Errorf("the killed round lost the file it had already created:\n%s", out.Diff)
	}
	if b, err := os.ReadFile(out.DiffPath); err != nil || len(b) == 0 {
		t.Errorf("diff was not written to the run log at %s (err=%v)", out.DiffPath, err)
	}
}

// --- a minimal MCP client over the in-process server -------------------------

type testClient struct {
	stdinW  *os.File
	stdoutR *os.File
	dec     *json.Decoder
	closed  bool
}

func startServer(t *testing.T) *testClient {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = Serve(context.Background(), inR, outW)
		outW.Close()
	}()
	c := &testClient{stdinW: inW, stdoutR: outR, dec: json.NewDecoder(outR)}
	t.Cleanup(func() {
		c.closeStdin()
		select {
		case <-served:
		case <-time.After(10 * time.Second):
			t.Error("Serve did not return after stdin closed")
		}
		outR.Close()
	})
	return c
}

func (c *testClient) send(t *testing.T, line string) {
	t.Helper()
	if _, err := c.stdinW.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

func (c *testClient) closeStdin() {
	if !c.closed {
		c.closed = true
		c.stdinW.Close()
	}
}

func (c *testClient) readReply(t *testing.T) map[string]any {
	t.Helper()
	var msg map[string]any
	if err := c.dec.Decode(&msg); err != nil {
		t.Fatalf("reading reply: %v", err)
	}
	return msg
}

func replyText(t *testing.T, reply map[string]any) string {
	t.Helper()
	result, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("reply has no result: %v", reply)
	}
	blocks, ok := result["content"].([]any)
	if !ok || len(blocks) == 0 {
		t.Fatalf("reply has no content: %v", result)
	}
	first, _ := blocks[0].(map[string]any)
	text, _ := first["text"].(string)
	return text
}
