package worker

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// mkExec writes an executable script into dir.
func mkExec(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// #59: rig cannot carry a table of every ecosystem's test command, but it does
// not have to — the worker just ran one that works in THIS repo. Re-running it
// is not taking the worker's word: the engine runs it itself, with RIG_*
// scrubbed, and reads the exit code itself.
func TestRunRepoGateFallsBackToTheWorkersGateCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture gates are shell scripts")
	}

	t.Run("an unrecognised repo is gated by the command the worker ran", func(t *testing.T) {
		repo := t.TempDir() // no go.mod, no package.json, no marker of any kind
		mkExec(t, repo, "runtests.sh", "#!/bin/sh\necho '3 examples, 0 failures'\n")

		o := runRepoGate(repo, "sh runtests.sh")

		if !o.Ran {
			t.Fatalf("gate did not run: %s", o.NotRunReason)
		}
		if o.Verdict != "pass" {
			t.Errorf("verdict = %q, want pass", o.Verdict)
		}
		if o.Source != "worker-observed" {
			t.Errorf("source = %q — the caller must be able to tell this gate from a repo-shape one", o.Source)
		}
	})

	t.Run("the engine reads the exit code itself, so a lying worker is still caught", func(t *testing.T) {
		repo := t.TempDir()
		mkExec(t, repo, "runtests.sh", "#!/bin/sh\necho '1 failing'\nexit 1\n")

		o := runRepoGate(repo, "sh runtests.sh")

		if !o.Ran || o.Verdict != "fail" {
			t.Fatalf("ran=%v verdict=%q, want a measured fail", o.Ran, o.Verdict)
		}
	})

	t.Run("repo shape still wins over the worker's pick", func(t *testing.T) {
		repo := t.TempDir()
		// A Makefile with a test target IS a recognised shape, so the engine uses
		// it rather than whatever the worker happened to run — the repo's own
		// declaration outranks the worker's choice.
		if err := os.WriteFile(filepath.Join(repo, "Makefile"), []byte("test:\n\t@echo MAKE_RAN\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mkExec(t, repo, "runtests.sh", "#!/bin/sh\necho WORKER_SCRIPT_RAN\n")

		o := runRepoGate(repo, "sh runtests.sh")

		if !o.Ran {
			t.Fatalf("gate did not run: %s", o.NotRunReason)
		}
		if o.Source != "repo-shape" || !strings.Contains(o.Output, "MAKE_RAN") {
			t.Errorf("source=%q output=%q — the repo's own declaration must win", o.Source, o.Output)
		}
	})

	t.Run("an inspection command is not accepted as a gate", func(t *testing.T) {
		repo := t.TempDir()
		// The worker's last bash call was `git diff`. That reports state, it does
		// not verify it — accepting it would manufacture a green verdict out of a
		// command that ran no code at all.
		o := runRepoGate(repo, "git diff")
		if o.Ran {
			t.Errorf("ran `git diff` as a gate and returned %q", o.Verdict)
		}
		if !strings.Contains(o.NotRunReason, "no recognised repo shape") {
			t.Errorf("reason = %q, want the unrecognised-shape diagnosis", o.NotRunReason)
		}
	})

	t.Run("the proof test is never the fallback gate", func(t *testing.T) {
		repo := t.TempDir()
		// implementCC deletes rig_proof_test.py before the gate runs, so
		// re-running the worker's proof command cannot do anything but fail —
		// it would report a false fail on every round whose last gate happened
		// to be the proof run, blaming the change for a file rig itself removed.
		o := runRepoGate(repo, "python -m pytest -q "+ccProofFile)
		if o.Ran {
			t.Errorf("re-ran the proof test as the gate and reported %q", o.Verdict)
		}
	})

	t.Run("no shape and no worker command is still an honest nothing", func(t *testing.T) {
		if o := runRepoGate(t.TempDir(), ""); o.Ran {
			t.Errorf("invented a gate and reported %q", o.Verdict)
		}
	})
}

// A wrong verdict is worse than a missing one — the reasoning already written
// into gateEnv. A gate command that does not APPLY exits non-zero just like a
// failing test suite, and reporting that as `fail` blames a change that was
// never actually tested.
func TestGateInapplicableIsNotAFailingGate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture gates are shell scripts")
	}

	t.Run("npm with no test script", func(t *testing.T) {
		repo := t.TempDir()
		mkExec(t, repo, "gate.sh", "#!/bin/sh\necho 'npm ERR! Missing script: test'\nexit 1\n")

		o := runRepoGate(repo, "sh gate.sh")

		if o.Ran {
			t.Fatalf("verdict %q — a missing test script is the gate not applying, not the code failing", o.Verdict)
		}
		if !strings.Contains(o.NotRunReason, "no test script") {
			t.Errorf("reason = %q, want it to name the real cause", o.NotRunReason)
		}
	})

	t.Run("make with no test target", func(t *testing.T) {
		repo := t.TempDir()
		mkExec(t, repo, "gate.sh", "#!/bin/sh\necho \"make: *** No rule to make target 'test'.  Stop.\"\nexit 2\n")

		if o := runRepoGate(repo, "sh gate.sh"); o.Ran {
			t.Errorf("verdict %q, want not-run", o.Verdict)
		}
	})

	t.Run("a real test failure is still a fail", func(t *testing.T) {
		repo := t.TempDir()
		// The dangerous direction: this must NOT be swallowed as "not run".
		mkExec(t, repo, "gate.sh", "#!/bin/sh\necho 'FAILED tests/test_x.py::test_y - assert 1 == 2'\nexit 1\n")

		o := runRepoGate(repo, "sh gate.sh")

		if !o.Ran || o.Verdict != "fail" {
			t.Fatalf("ran=%v verdict=%q reason=%q — a real failure must survive as a fail",
				o.Ran, o.Verdict, o.NotRunReason)
		}
	})
}

// The note is how MAIN learns which kind of gate it got. A worker-chosen gate is
// measured evidence, but narrower evidence, and saying so is the difference
// between a caller that can calibrate and one that cannot.
func TestEngineGateNoteNamesAWorkerChosenGate(t *testing.T) {
	o := gateOutcome{Ran: true, Verdict: "pass", Cmd: "sh runtests.sh", Source: "worker-observed"}
	note := engineGateNote(o, "pass")
	if !strings.Contains(note, "the gate command the WORKER used") {
		t.Errorf("note does not disclose where the gate came from:\n%s", note)
	}

	shape := engineGateNote(gateOutcome{Ran: true, Verdict: "pass", Cmd: "go test ./...", Source: "repo-shape"}, "pass")
	if strings.Contains(shape, "the WORKER used") {
		t.Errorf("a repo-shape gate must not be labelled as the worker's pick:\n%s", shape)
	}
}

// The tiered return exists so that nothing MAIN re-caches on every later turn
// arrives raw. The ENGINE gate's log arrived after that contract was written
// (#41) and was copied into the payload untouched; nobody noticed because the
// engine gate almost never ran on a repo rig did not recognise. #59 made it run
// on nearly everything, which turned a latent leak into a live one.
func TestTieredReturnDoesNotShipTheEngineGateLogRaw(t *testing.T) {
	repo := t.TempDir()
	// A real gate log: bulk, then the one line that is actually the verdict.
	huge := strings.Repeat("a very long engine gate line MAIN would re-cache forever\n", 400) +
		"ok  \tgithub.com/x/y\t0.512s\n"
	res := Result{
		Stopped:          "done",
		Summary:          "did the thing",
		EngineGateCmd:    "go test ./...",
		EngineGateOutput: huge,
		GateVerdict:      "pass",
		GateSource:       "engine",
	}

	out := TierResult(res, repo, 2000)

	body, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("re-cache forever")) {
		t.Errorf("the raw engine gate log reached the payload (%d bytes)", len(body))
	}
	if len(body) > len(huge)/4 {
		t.Errorf("payload is %d bytes against a %d-byte log — it is not being tiered", len(body), len(huge))
	}
	if out.EngineVerify == nil {
		t.Fatal("the engine gate's log must still be reported, tiered")
	}
	if out.EngineVerify.LogBytes != len(huge) {
		t.Errorf("LogBytes = %d, want the real size %d", out.EngineVerify.LogBytes, len(huge))
	}
	// Parked separately from the worker's own log: a round where the two verdicts
	// disagree is exactly the round where both logs must survive to be read.
	if out.EngineVerify.Drill == nil {
		t.Fatal("the raw engine log must be parked for drilling")
	}
	if out.EngineVerify.Drill.File == testLogName {
		t.Error("the engine log overwrote the worker's log — they are two different facts")
	}
	if _, err := os.Stat(filepath.Join(repo, out.EngineVerify.Drill.File)); err != nil {
		t.Errorf("parked engine log is not on disk: %v", err)
	}
}
