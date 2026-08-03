package worker

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/Cheevatech/rig-move-llm/internal/gatestate"
)

// workerSession is one round's identity. The engine picks the id, registers it
// where the hook will look, and passes it to `claude --session-id`. Only that
// subprocess's hook payloads carry it — which is the whole point: RIG_AGENT_ID
// did the same job through the environment, and the environment is inherited by
// every process the worker launches, including the user's own test suite (#42).
type workerSession struct {
	id   string
	dirs []string // registries written; unregistered on close
}

// newWorkerSession registers a fresh session id. ok is false when nothing could
// be registered — the caller then keeps the RIG_AGENT_ID stamp, because a worker
// that cannot prove it is a worker is denied every tool and burns the round with
// an empty diff (#24). Losing the leak fix is cheap; losing the round is not.
func (e *Engine) newWorkerSession() (ws workerSession, ok bool) {
	id, err := newSessionID()
	if err != nil {
		return workerSession{}, false
	}
	// The subprocess inherits no RIG_* (ccChildEnv strips them), so its hook
	// resolves the state dir from config alone. When this process was started
	// with RIG_STATE_DIR pointing somewhere else, the two disagree — so register
	// in both and let whichever the child reads find it.
	for _, d := range dedupe([]string{e.stateDir(), e.cfg.DataDir}) {
		if gatestate.RegisterWorkerSession(d, id) == nil {
			ws.dirs = append(ws.dirs, d)
		}
	}
	if len(ws.dirs) == 0 {
		return workerSession{}, false
	}
	ws.id = id
	return ws, true
}

// close drops the registration. A round that is killed hard never reaches this;
// gatestate.WorkerSessionTTL is what bounds those leftovers.
func (ws workerSession) close() {
	for _, d := range ws.dirs {
		gatestate.UnregisterWorkerSession(d, ws.id)
	}
}

// newSessionID returns a random RFC-4122 v4 UUID. `claude --session-id` requires
// UUID form (measured: it rejects arbitrary strings), and stdlib-only means
// assembling it here rather than taking a dependency.
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32]), nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
