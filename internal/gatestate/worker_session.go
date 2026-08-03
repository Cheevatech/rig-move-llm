package gatestate

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The worker's identity used to travel in RIG_AGENT_ID, an environment variable
// the engine stamped onto the `claude -p` subprocess. Environment variables are
// inherited, so that stamp reached every process the worker ever launched —
// including the user's own `go test` (#42), which then saw itself as a rig
// worker. rig's own suite unsets it in TestMain, which is a patch on the
// symptom and not a fix.
//
// A session id does not inherit: `claude --session-id <uuid>` puts that exact
// id in the hook payload of that session and nowhere else (measured 2026-08-03:
// every PreToolUse/SessionStart/UserPromptSubmit payload carried the id that was
// passed on the command line, and nothing else did). So the engine registers the
// id it is about to spawn, and the hook asks this registry "is the session I am
// running inside one of rig's workers?".
//
// One file per session under <dataDir>/worker_sessions/. A file, not a single
// JSON list, because two rounds can overlap and neither may clobber the other.
const workerSessionsDir = "worker_sessions"

// WorkerSessionTTL bounds how long a registration is believed. A round that is
// killed hard (the #18 shape) leaves its file behind; without a TTL that stale
// file would hand worker privileges to a later unrelated session that happened
// to be handed the same id. Rounds are wall-bounded well under this.
const WorkerSessionTTL = 12 * time.Hour

// RegisterWorkerSession records that sessionID belongs to a rig worker.
func RegisterWorkerSession(dir, sessionID string) error {
	if dir == "" || !validSessionID(sessionID) {
		return os.ErrInvalid
	}
	d := filepath.Join(dir, workerSessionsDir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(d, sessionID), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
}

// UnregisterWorkerSession drops the registration when the round is over.
func UnregisterWorkerSession(dir, sessionID string) {
	if dir == "" || !validSessionID(sessionID) {
		return
	}
	_ = os.Remove(filepath.Join(dir, workerSessionsDir, sessionID))
}

// IsWorkerSession reports whether sessionID is a live rig worker session.
func IsWorkerSession(dir, sessionID string) bool {
	if dir == "" || !validSessionID(sessionID) {
		return false
	}
	fi, err := os.Stat(filepath.Join(dir, workerSessionsDir, sessionID))
	if err != nil {
		return false
	}
	return time.Since(fi.ModTime()) <= WorkerSessionTTL
}

// validSessionID keeps the id usable as a single path element: the value comes
// from a hook payload, so a "session id" of "../../etc/passwd" must not be able
// to make the hook stat something else.
func validSessionID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	if strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
