package hook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// health.go implements the zero-token worker liveness check. A probe is a plain
// HTTP GET (no LLM inference) fired once per user message from the
// UserPromptSubmit hook; its result is cached in a small marker file that the
// per-tool PreTool/PostTool hooks read to decide whether to force-delegate or
// pass through. When the worker endpoint is unreachable the hybrid degrades to
// plain Claude Code (Enabled=false behaviour) automatically instead of blocking.

// healthMarker is the on-disk cache of the last probe (health.json), shared by the
// UserPromptSubmit writer (CLI) and the per-tool readers (PreTool/PostTool).
type healthMarker struct {
	TS      time.Time `json:"ts"`
	Healthy bool      `json:"healthy"`
}

// HealthDisabled reports whether pre-flight health-checking is turned off. The
// call-time error fallback still applies; this only governs the proactive probe.
func HealthDisabled(path string) bool {
	switch strings.ToLower(strings.TrimSpace(path)) {
	case "", "off", "none", "-", "false", "0":
		return true
	}
	return false
}

// HealthURL derives the probe URL from the worker API base and a configured path.
// The base already includes a path suffix (typically /v1), so we probe against
// its origin (scheme://host[:port]) joined with the health path. A path that is
// itself an absolute URL is used verbatim.
func HealthURL(workerAPIBase, path string) string {
	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	u, err := url.Parse(workerAPIBase)
	if err != nil || u.Scheme == "" || u.Host == "" {
		// Best effort: fall back to trimming the base and appending the path.
		return strings.TrimRight(workerAPIBase, "/") + "/" + strings.TrimLeft(path, "/")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return u.Scheme + "://" + u.Host + path
}

// Probe issues a GET to url and reports whether the endpoint answered with a 2xx.
// Any transport error or non-2xx status is "not healthy" (caller degrades).
func Probe(u string, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode/100 == 2
}

// WriteHealthMarker atomically persists a probe result (temp file + rename).
func WriteHealthMarker(path string, healthy bool, now time.Time) {
	if path == "" {
		return
	}
	data, err := json.Marshal(healthMarker{TS: now, Healthy: healthy})
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// ReadHealthMarker returns the cached (healthy, ts). ok is false when the marker is
// absent or unreadable.
func ReadHealthMarker(path string) (healthy bool, ts time.Time, ok bool) {
	if path == "" {
		return false, time.Time{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, time.Time{}, false
	}
	var m healthMarker
	if json.Unmarshal(data, &m) != nil {
		return false, time.Time{}, false
	}
	return m.Healthy, m.TS, true
}

// ClearHealthMarker removes the marker (used when health-checking is disabled or
// the master switch is off, so a stale "down" can never linger).
func ClearHealthMarker(path string) {
	if path != "" {
		_ = os.Remove(path)
	}
}

// HealthMarkerPath resolves the marker file (health.json) inside a state dir.
func HealthMarkerPath(dir string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "health.json")
}
