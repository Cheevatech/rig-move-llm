package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/internal/gatestate"
	"github.com/Cheevatech/rig-move-llm/internal/hook"
)

// cmdUserPrompt is the UserPromptSubmit hook. Once per user message it probes the
// worker endpoint (a plain HTTP GET — zero LLM tokens) and caches the verdict in
// health.json. The per-tool PreTool/PostTool hooks read that marker: healthy keeps
// the force-delegate hybrid, unhealthy flips to passthrough so a dead worker
// degrades to plain Claude Code automatically. On failure it also surfaces a
// systemMessage so the human sees the fallback in the process stream.
//
// It never blocks the prompt: every path returns 0, and any error is treated as
// "not healthy" (fail toward the local, always-available path).
func cmdUserPrompt(r io.Reader, w io.Writer) int {
	_, _ = io.Copy(io.Discard, r) // drain payload; the decision is config-driven

	// The cc worker runs as its own `claude -p` session in the same project, so it
	// fires these lifecycle hooks too — and everything below is MAIN-scoped. Left
	// unguarded, every successful delegation cleared the very per-turn state it
	// was supposed to be counted in: the triage decision, the Gate B window, and
	// the delegation budget (#27, measured — the budget could only ever fire when
	// two dispatches landed between two worker sessions). A worker session has no
	// intake of its own to reset and no reason to re-probe worker health.
	if inWorkerSession() {
		return 0
	}

	cfg := config.Load()
	marker := hook.HealthMarkerPath(stateDir(cfg))

	// A new user message is a new intake: drop the per-task triage decision and
	// any open Gate B repair window (the Stage-0 explore evidence is kept — it is
	// expensive to redo and can simply be re-triaged against).
	gatestate.ClearTurn(stateDir(cfg))

	// Nothing to gate when the hybrid is off or health-checking is disabled. Clear
	// any stale marker so a previous "down" can't linger and disable the hybrid.
	if !cfg.Enabled || hook.HealthDisabled(cfg.WorkerHealthPath) {
		hook.ClearHealthMarker(marker)
		return 0
	}

	u := hook.HealthURL(cfg.WorkerAPIBase, cfg.WorkerHealthPath)
	now := time.Now()

	// Cache: reuse a recent probe (avoids a probe on every rapid turn).
	if healthy, ts, ok := hook.ReadHealthMarker(marker); ok && cfg.HealthCacheSec > 0 &&
		now.Sub(ts) < time.Duration(cfg.HealthCacheSec)*time.Second {
		if !healthy {
			emitFallback(w, u)
		}
		return 0
	}

	healthy := hook.Probe(u, time.Duration(cfg.HealthTimeoutMs)*time.Millisecond, cfg.WorkerAPIKey)
	hook.WriteHealthMarker(marker, healthy, now)
	if !healthy {
		emitFallback(w, u)
	}
	return 0
}

func emitFallback(w io.Writer, u string) {
	msg := fmt.Sprintf(
		"⚠️ rig-move-llm: worker healthcheck failed (%s) — falling back to local Claude for this turn.", u)
	_ = json.NewEncoder(w).Encode(map[string]any{"systemMessage": msg})
}

// inWorkerSession reports whether this hook invocation belongs to a worker
// session rather than the paid MAIN leg. RIG_AGENT_ID is the identity stamp the
// cc engine puts on its subprocess (#24) and teammate-exec puts on a terminal
// teammate; MAIN never carries it.
//
// The rule this encodes, after two bugs of the same shape: anything a hook does
// on behalf of MAIN must first establish that it is running as MAIN.
func inWorkerSession() bool { return os.Getenv("RIG_AGENT_ID") != "" }

// stateDir resolves where the hook keeps its per-scope state (matching
// buildHookState): RIG_STATE_DIR overrides the config data dir.
func stateDir(cfg config.Config) string {
	if d := os.Getenv("RIG_STATE_DIR"); d != "" {
		return d
	}
	return cfg.DataDir
}
