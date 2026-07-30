package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The guards that kill a round hand collectDiff a context that is ALREADY done —
// that is the whole point of the timeout path, and it is where MAIN needs the
// partial work most. Measured 2026-07-30 (task11): a killed round returned
// files=[] with two real files on disk, MAIN read "nothing happened" and
// re-delegated into the same wall.
func TestCollectDiffSurvivesADeadContext(t *testing.T) {
	repo := untrackedRepo(t)
	write(t, repo, "tracked.py", "x = 2\n")                   // an edit to a tracked file
	write(t, repo, "internal/cli/doctor.go", "package cli\n") // and a created one

	for _, tc := range []struct {
		name string
		ctx  func() context.Context
	}{
		{"canceled", func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}},
		{"deadline exceeded", func() context.Context {
			ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
			t.Cleanup(cancel)
			time.Sleep(time.Millisecond)
			return ctx
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			diff, files := (&Engine{}).collectDiff(tc.ctx(), repo)

			if len(files) != 2 {
				t.Fatalf("files_changed = %v, want the edited AND the created file", files)
			}
			for _, want := range []string{"-x = 1", "+x = 2", "new file mode", "+package cli"} {
				if !strings.Contains(diff, want) {
					t.Errorf("diff missing %q:\n%s", want, diff)
				}
			}
		})
	}
}

// A live round must not be silently granted the rescue window instead of its own
// deadline: the diff of a healthy run stays inside the round's budget.
func TestDiffCtxKeepsALiveContextUnchanged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got, release := diffCtx(ctx)
	defer release()

	if got != ctx {
		t.Errorf("diffCtx replaced a live context; want it used as-is")
	}
}

// The rescue is bounded — it reads what is on disk, it does not let a killed
// round keep working.
func TestDiffCtxRescueIsBounded(t *testing.T) {
	dead, cancel := context.WithCancel(context.Background())
	cancel()

	got, release := diffCtx(dead)
	defer release()

	if got.Err() != nil {
		t.Fatalf("rescue context already done: %v", got.Err())
	}
	dl, ok := got.Deadline()
	if !ok {
		t.Fatal("rescue context has no deadline")
	}
	if d := time.Until(dl); d <= 0 || d > diffRescueTimeout {
		t.Errorf("rescue window = %v, want (0, %v]", d, diffRescueTimeout)
	}
}

// Belt-and-braces on the real failure: the file the worker created is reported
// even when the repo has an untracked directory rig itself owns.
func TestCollectDiffDeadContextSkipsRigsOwnWiring(t *testing.T) {
	repo := untrackedRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, repo, ".claude/settings.json", "{}\n")
	write(t, repo, "doctor.go", "package main\n")

	dead, cancel := context.WithCancel(context.Background())
	cancel()

	_, files := (&Engine{}).collectDiff(dead, repo)

	if len(files) != 1 || files[0] != "doctor.go" {
		t.Fatalf("files_changed = %v, want only the worker's file", files)
	}
}
