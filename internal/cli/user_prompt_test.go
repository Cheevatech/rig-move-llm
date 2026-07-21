package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/hook"
)

// runUserPrompt invokes the UserPromptSubmit hook with an empty payload and
// returns its stdout.
func runUserPrompt(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	if code := cmdUserPrompt(strings.NewReader("{}"), &b); code != 0 {
		t.Fatalf("cmdUserPrompt returned %d, want 0", code)
	}
	return b.String()
}

func TestUserPromptHealthy(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	state := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RIG_STATE_DIR", state)
	t.Setenv("WORKER_API_BASE", up.URL)
	t.Setenv("ENABLED", "true")

	if out := runUserPrompt(t); out != "" {
		t.Errorf("healthy worker should be silent; got %q", out)
	}
	if healthy, _, ok := hook.ReadHealthMarker(hook.HealthMarkerPath(state)); !ok || !healthy {
		t.Errorf("marker = (%v,%v), want healthy", healthy, ok)
	}
}

func TestUserPromptDownEmitsFallback(t *testing.T) {
	state := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RIG_STATE_DIR", state)
	t.Setenv("WORKER_API_BASE", "http://127.0.0.1:1") // nothing listening
	t.Setenv("ENABLED", "true")
	t.Setenv("WORKER_HEALTH_TIMEOUT_MS", "300")

	out := runUserPrompt(t)
	if !strings.Contains(out, "systemMessage") || !strings.Contains(out, "healthcheck failed") {
		t.Errorf("worker down should emit a systemMessage fallback; got %q", out)
	}
	if healthy, _, ok := hook.ReadHealthMarker(hook.HealthMarkerPath(state)); !ok || healthy {
		t.Errorf("marker = (%v,%v), want down", healthy, ok)
	}
}

func TestUserPromptDisabledClearsMarker(t *testing.T) {
	state := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RIG_STATE_DIR", state)
	t.Setenv("WORKER_API_BASE", "http://127.0.0.1:1")
	t.Setenv("ENABLED", "true")

	// Seed a stale down marker, then disable health-checking: it must be cleared so
	// the per-tool hooks don't stay in fallback forever.
	marker := hook.HealthMarkerPath(state)
	hook.WriteHealthMarker(marker, false, time.Now())
	t.Setenv("WORKER_HEALTH_PATH", "off")

	if out := runUserPrompt(t); out != "" {
		t.Errorf("disabled health-check should be silent; got %q", out)
	}
	if _, _, ok := hook.ReadHealthMarker(marker); ok {
		t.Error("marker should be cleared when health-checking is disabled")
	}
}
