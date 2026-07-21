package hook

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestHealthDisabled(t *testing.T) {
	for _, p := range []string{"", "off", "OFF", " none ", "-", "false", "0"} {
		if !HealthDisabled(p) {
			t.Errorf("HealthDisabled(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"/v1/models", "/health", "/ping"} {
		if HealthDisabled(p) {
			t.Errorf("HealthDisabled(%q) = true, want false", p)
		}
	}
}

func TestHealthURL(t *testing.T) {
	cases := []struct {
		base, path, want string
	}{
		{"http://host:8100/v1", "/v1/models", "http://host:8100/v1/models"},
		{"http://host:8100/v1", "/health", "http://host:8100/health"},
		{"http://host:8100/v1", "models", "http://host:8100/models"},
		{"https://api.example.com/v1", "/v1/models", "https://api.example.com/v1/models"},
		{"http://host:8100/v1", "http://other:9/healthz", "http://other:9/healthz"},
	}
	for _, c := range cases {
		if got := HealthURL(c.base, c.path); got != c.want {
			t.Errorf("HealthURL(%q,%q) = %q, want %q", c.base, c.path, got, c.want)
		}
	}
}

func TestProbe(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	if !Probe(up.URL, time.Second) {
		t.Error("Probe on a 200 server = false, want true")
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()
	if Probe(down.URL, time.Second) {
		t.Error("Probe on a 503 server = true, want false")
	}

	// Unreachable endpoint (nothing listening) -> not healthy, not a panic.
	if Probe("http://127.0.0.1:1/nope", 200*time.Millisecond) {
		t.Error("Probe on an unreachable endpoint = true, want false")
	}
}

func TestHealthMarkerRoundtrip(t *testing.T) {
	p := HealthMarkerPath(t.TempDir())
	now := time.Now()

	WriteHealthMarker(p, false, now)
	healthy, ts, ok := ReadHealthMarker(p)
	if !ok || healthy || ts.Unix() != now.Unix() {
		t.Fatalf("read back = (%v,%v,%v), want (false, %v, true)", healthy, ts, ok, now)
	}

	ClearHealthMarker(p)
	if _, _, ok := ReadHealthMarker(p); ok {
		t.Error("marker still readable after ClearHealthMarker")
	}
}

func TestHealthDownActive(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "health.json")
	s := &State{Enabled: true, HealthMarker: marker, HealthTTL: 10 * time.Minute}

	// No marker -> treated as up.
	if s.healthDownActive() {
		t.Error("no marker should be up")
	}
	// Healthy marker -> up.
	WriteHealthMarker(marker, true, time.Now())
	if s.healthDownActive() {
		t.Error("healthy marker should be up")
	}
	// Fresh down marker -> down.
	WriteHealthMarker(marker, false, time.Now())
	if !s.healthDownActive() {
		t.Error("fresh down marker should be down")
	}
	// Stale down marker -> fail safe to up.
	WriteHealthMarker(marker, false, time.Now().Add(-20*time.Minute))
	if s.healthDownActive() {
		t.Error("stale down marker should fail safe to up")
	}
}

// TestHealthDownFlipsPreTool asserts the automatic fallback: a fresh down marker
// makes the force-delegate PreTool pass a MAIN Edit through (Claude Code runs
// locally) even though Enabled=true.
func TestHealthDownFlipsPreTool(t *testing.T) {
	marker := HealthMarkerPath(t.TempDir())
	s := &State{Enabled: true, HealthMarker: marker, HealthTTL: 10 * time.Minute}
	edit := `{"tool_name":"Edit","tool_input":{"file_path":"/repo/main.go"}}`

	// Healthy: MAIN Edit is denied (force-delegate).
	WriteHealthMarker(marker, true, time.Now())
	if denied, _ := preDecision(t, s, edit); !denied {
		t.Error("healthy worker: MAIN Edit should be denied (force-delegate)")
	}
	// Down: MAIN Edit passes through (local fallback).
	WriteHealthMarker(marker, false, time.Now())
	if denied, out := preDecision(t, s, edit); denied {
		t.Errorf("worker down: MAIN Edit should pass through; got deny out=%q", out)
	}
}
