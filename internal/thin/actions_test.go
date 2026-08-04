package thin

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The action log exists to answer one question from a second window: is the
// worker working, or is it stuck? Every assertion here is about that question.

// It is written ALWAYS. The run nobody thought to enable logging for is exactly
// the run that later needs explaining.
func TestActionLogIsNotOptIn(t *testing.T) {
	repo := gitRepo(t)
	stub := filepath.Join(t.TempDir(), "fake-claude")
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","tools":["Bash","Read","Edit"]}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/deep/nested/path/utils.py"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"def add(a,b): ..."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"./.venv/bin/python -m pytest -q"}}]}}`,
		// No backslash escapes in these stub payloads: /bin/sh's echo interprets
		// them, which splits a JSON line in half and silently drops the event.
		// gist's "read the verdict off the last line" behaviour is covered
		// directly by TestGistTakesTheVerdictNotTheBanner instead.
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":"6 passed in 0.01s"}]}}`,
		`{"type":"result","subtype":"success"}`,
	}, "'\necho '")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho '"+stream+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	thinEnv(t, stub, t.TempDir())

	// No env var is set to ask for it.
	out := Run(context.Background(), repo, "make normalize_ws exist")
	body, err := os.ReadFile(filepath.Join(out.LogDir, ActionsFile))
	if err != nil {
		t.Fatalf("no action log was written: %v", err)
	}
	got := string(body)

	// The header names the run without needing a second file opened.
	for _, want := range []string{"repo:", repo, "task:", "make normalize_ws exist", "started:"} {
		if !strings.Contains(got, want) {
			t.Errorf("header missing %q:\n%s", want, got)
		}
	}

	// One line per action, each identifying WHAT was touched.
	for _, want := range []string{"Read", "utils.py", "Bash", "pytest -q"} {
		if !strings.Contains(got, want) {
			t.Errorf("action log missing %q:\n%s", want, got)
		}
	}

	// A test run's verdict is the point of watching, and it is on the LAST line
	// of the output, not the first.
	if !strings.Contains(got, "6 passed") {
		t.Errorf("the Bash result's verdict is missing:\n%s", got)
	}

	// It closes with the outcome, which is also what lets `watch` stop waiting.
	if !strings.Contains(got, "──") || !strings.Contains(got, statusFinished) {
		t.Errorf("no closing line:\n%s", got)
	}

	// The raw stream stays next to it — this is a second log, not a replacement.
	if _, err := os.Stat(filepath.Join(out.LogDir, "stream.jsonl")); err != nil {
		t.Errorf("the raw stream is gone: %v", err)
	}
}

// A failure is never implied by what comes next, so it is always shown — while
// routine Read/Edit successes are not, or the feed drowns in noise.
func TestActionLogShowsFailuresAndSkipsRoutineSuccess(t *testing.T) {
	repo := gitRepo(t)
	stub := filepath.Join(t.TempDir(), "fake-claude")
	stream := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"e1","name":"Edit","input":{"file_path":"utils.py"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"e1","content":"applied"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"b1","name":"Bash","input":{"command":"pytest -q"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"b1","is_error":true,"content":"1 failed, 5 passed"}]}}`,
		`{"type":"result","subtype":"success"}`,
	}, "'\necho '")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho '"+stream+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	thinEnv(t, stub, t.TempDir())

	out := Run(context.Background(), repo, "t")
	body, _ := os.ReadFile(filepath.Join(out.LogDir, ActionsFile))
	got := string(body)

	if !strings.Contains(got, "FAILED") || !strings.Contains(got, "1 failed") {
		t.Errorf("a failing command must be visible:\n%s", got)
	}
	if strings.Contains(got, "applied") {
		t.Errorf("a routine Edit success was echoed; the feed must stay scannable:\n%s", got)
	}
}

// gist reads a verdict off the END of the output. A test runner prints its
// summary last, and taking the first line would report the banner forever.
func TestGistTakesTheVerdictNotTheBanner(t *testing.T) {
	body := "============ test session starts ============\nplatform darwin\ncollected 6 items\n\n......\n\n6 passed in 0.01s\n"
	if got := gist(body); got != "6 passed in 0.01s" {
		t.Errorf("gist = %q, want the trailing verdict", got)
	}
	if got := gist("   \n\n"); got != "" {
		t.Errorf("gist of nothing = %q, want empty", got)
	}
}

// Watch on a finished run prints it and returns instead of hanging — the closing
// line is the terminator.
func TestWatchReturnsOnAFinishedRun(t *testing.T) {
	root := t.TempDir()
	t.Setenv("RIG_THIN_LOG_ROOT", root)
	dir := filepath.Join(root, "20260804-170000-aaaaaa")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ActionsFile),
		[]byte("repo:    /r\ntask:    do a thing\n\n+00:01  Bash   pytest -q\n+00:09  ──     finished\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- Watch(&buf, "", true) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Watch: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch hung on a run that had already finished")
	}
	for _, want := range []string{"do a thing", "pytest -q", "finished"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("watch output missing %q:\n%s", want, buf.String())
		}
	}
}

// With no argument it follows the newest run, so "what is happening right now"
// needs no path typed at 2am.
func TestWatchPicksTheNewestRun(t *testing.T) {
	root := t.TempDir()
	t.Setenv("RIG_THIN_LOG_ROOT", root)
	for _, name := range []string{"20260804-100000-aaaaaa", "20260804-170000-bbbbbb", "20260804-120000-cccccc"} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ActionsFile),
			[]byte("task:    run "+name+"\n\n+00:00  ──     finished\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	latest, err := LatestRun()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(latest) != "20260804-170000-bbbbbb" {
		t.Errorf("newest run = %s, want the 17:00 one", filepath.Base(latest))
	}

	runs, err := ListRuns(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || filepath.Base(runs[0]) != "20260804-170000-bbbbbb" {
		t.Errorf("list is not newest-first: %v", runs)
	}
	if s := RunSummary(runs[0]); !strings.Contains(s, "finished") || !strings.Contains(s, "run 20260804-170000") {
		t.Errorf("summary = %q", s)
	}
}

// A run still in flight is summarized as running, not as finished-with-no-status.
func TestRunSummaryCallsAnUnfinishedRunRunning(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ActionsFile),
		[]byte("task:    still going\n\n+00:03  Bash   pytest -q\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s := RunSummary(dir); !strings.Contains(s, "running") {
		t.Errorf("summary = %q, want it marked running", s)
	}
}

// A run that never reaches a subprocess still has to explain itself. The return
// quotes the log directory's path (S1), so an empty directory there is the
// return pointing at nothing — and `rig watch`, the obvious next thing a human
// does after a failed run, answered with a filesystem error instead of a reason.
func TestFailedRunStillWritesItsLog(t *testing.T) {
	for _, tc := range []struct {
		name, wantIn string
		setup        func(t *testing.T)
	}{
		{
			name:   "refused for a missing base URL",
			wantIn: "RIG_CC_BASE_URL",
			setup:  func(t *testing.T) { t.Setenv("RIG_CC_BASE_URL", "") },
		},
		{
			name:   "the worker binary does not exist",
			wantIn: "launch",
			setup: func(t *testing.T) {
				t.Setenv("RIG_CC_BASE_URL", "http://127.0.0.1:9/")
				t.Setenv("RIG_CC_BIN", filepath.Join(t.TempDir(), "not-a-real-binary"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("RIG_THIN_LOG_ROOT", root)
			t.Setenv("RIG_THIN_SUPERVISOR_BIN", os.Args[0])
			tc.setup(t)

			out := Run(context.Background(), gitRepo(t), "a task that never runs")
			body, err := os.ReadFile(filepath.Join(out.LogDir, ActionsFile))
			if err != nil {
				t.Fatalf("a failed run left its log directory empty: %v", err)
			}
			got := string(body)
			if !strings.Contains(got, "a task that never runs") {
				t.Errorf("the log does not record the task:\n%s", got)
			}
			if !strings.Contains(got, "──") || !strings.Contains(got, tc.wantIn) {
				t.Errorf("the log does not close with the reason (want %q):\n%s", tc.wantIn, got)
			}
			// And what the log says must be what the caller was told.
			if !strings.Contains(out.Status, tc.wantIn) {
				t.Errorf("status = %q, want it to name %q too", out.Status, tc.wantIn)
			}

			// watch must therefore work on it rather than error out.
			var buf bytes.Buffer
			if err := Watch(&buf, "", true); err != nil {
				t.Fatalf("watch could not follow a failed run: %v", err)
			}
			if !strings.Contains(buf.String(), tc.wantIn) {
				t.Errorf("watch did not show the reason:\n%s", buf.String())
			}
		})
	}
}

// The quiet notice is the command's whole reason for existing: silence that is
// never mentioned looks exactly like a dead terminal, and telling those apart is
// what a 20-to-50-minute run needs. So it is tested, with the clock turned down.
func TestWatchAnnouncesSilence(t *testing.T) {
	oldPoll, oldNotice, oldRepeat := watchPoll, watchQuietNotice, watchQuietRepeats
	watchPoll, watchQuietNotice, watchQuietRepeats = 10*time.Millisecond, 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { watchPoll, watchQuietNotice, watchQuietRepeats = oldPoll, oldNotice, oldRepeat })

	root := t.TempDir()
	t.Setenv("RIG_THIN_LOG_ROOT", root)
	dir := filepath.Join(root, "20260804-170000-aaaaaa")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, ActionsFile)
	if err := os.WriteFile(logPath, []byte("task:    a slow one\n\n+00:01  Bash   pytest -q\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf syncBuffer
	done := make(chan error, 1)
	go func() { done <- Watch(&buf, "", true) }()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "no new action for") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(buf.String(), "no new action for") {
		t.Fatalf("watch never mentioned the silence:\n%s", buf.String())
	}
	// It names the guard, so a reader knows how long the quiet may still last.
	if !strings.Contains(buf.String(), "stall guard") {
		t.Errorf("the notice does not say when the guard acts:\n%s", buf.String())
	}

	f, _ := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("+00:20  ──     finished\n")
	f.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Watch: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not return after the run finished")
	}
}

// syncBuffer is a bytes.Buffer safe to read while Watch writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
