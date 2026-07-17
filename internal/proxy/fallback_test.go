package proxy

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rigmovellm/rig-move-llm/internal/config"
	"github.com/rigmovellm/rig-move-llm/internal/stats"
)

const openAIOK = `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":300,"completion_tokens":20}}`

func newFallbackServer(t *testing.T, cfg config.Config) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	rec, err := stats.NewRecorder(dir, false)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	cfg.DataDir = dir
	return &Server{cfg: cfg, rec: rec}, dir
}

func postWorker(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-haiku-4-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	return rw
}

func lastLogLines(t *testing.T, dataDir string) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join(dataDir, "logs", "requests.jsonl"))
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err == nil {
			out = append(out, m)
		}
	}
	return out
}

// TestWorkerFallbackAndHealthGate: the first endpoint 503s, the second serves.
// The failure puts the first endpoint in cooldown, so the next request skips it
// entirely; once the cooldown expires it is retried again.
func TestWorkerFallbackAndHealthGate(t *testing.T) {
	var hits1, hits2 atomic.Int64
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits1.Add(1)
		http.Error(w, `{"error":{"message":"qwen is temporarily unavailable"}}`, http.StatusServiceUnavailable)
	}))
	defer down.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits2.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, openAIOK)
	}))
	defer up.Close()

	s, dataDir := newFallbackServer(t, config.Config{
		Workers: []config.WorkerEndpoint{
			{Name: "primary", Base: down.URL + "/v1", Priority: 1},
			{Name: "backup", Base: up.URL + "/v1", Priority: 2},
		},
	})
	h := s.Handler()

	if rw := postWorker(t, h); rw.Code != http.StatusOK {
		t.Fatalf("first request: status %d: %s", rw.Code, rw.Body.String())
	}
	if hits1.Load() != 1 || hits2.Load() != 1 {
		t.Fatalf("after 1st: hits primary=%d backup=%d, want 1/1", hits1.Load(), hits2.Load())
	}

	// Health gate: primary is cooling down, so it is not retried.
	if rw := postWorker(t, h); rw.Code != http.StatusOK {
		t.Fatalf("second request: status %d", rw.Code)
	}
	if hits1.Load() != 1 || hits2.Load() != 2 {
		t.Fatalf("after 2nd: hits primary=%d backup=%d, want 1/2 (health gate)", hits1.Load(), hits2.Load())
	}

	// Cooldown expiry: primary is tried (and fails) again.
	old := healthCooldown
	healthCooldown = time.Nanosecond
	defer func() { healthCooldown = old }()
	if rw := postWorker(t, h); rw.Code != http.StatusOK {
		t.Fatalf("third request: status %d", rw.Code)
	}
	if hits1.Load() != 2 || hits2.Load() != 3 {
		t.Fatalf("after 3rd: hits primary=%d backup=%d, want 2/3 (cooldown expired)", hits1.Load(), hits2.Load())
	}

	if err := s.rec.Close(); err != nil {
		t.Fatal(err)
	}
	lines := lastLogLines(t, dataDir)
	if len(lines) != 3 {
		t.Fatalf("want 3 log lines, got %d", len(lines))
	}
	for _, l := range lines {
		if l["endpoint"] != "backup" || l["routed"] != stats.RoutedWorker {
			t.Errorf("log line = %v, want endpoint=backup routed=worker", l)
		}
	}
}

// TestWorkerDivertPassthrough: with every worker dead, a passthrough entry
// sends the request to the paid main upstream, billed on MAIN but tagged
// routed=diverted with the endpoint label.
func TestWorkerDivertPassthrough(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close() // connection refused

	mainUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"m","type":"message","usage":{"input_tokens":10,"output_tokens":5}}`)
	}))
	defer mainUp.Close()

	s, dataDir := newFallbackServer(t, config.Config{
		MainUpstreamURL: mainUp.URL,
		Workers: []config.WorkerEndpoint{
			{Name: "primary", Base: dead.URL + "/v1", Priority: 1},
			{Passthrough: true, Priority: 2},
		},
	})

	if rw := postWorker(t, s.Handler()); rw.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rw.Code, rw.Body.String())
	}
	if err := s.rec.Close(); err != nil {
		t.Fatal(err)
	}

	lines := lastLogLines(t, dataDir)
	if len(lines) != 1 {
		t.Fatalf("want 1 log line, got %d", len(lines))
	}
	l := lines[0]
	if l["leg"] != string(stats.LegMain) || l["routed"] != stats.RoutedDiverted || l["endpoint"] != "passthrough" {
		t.Errorf("log line = %v, want leg=MAIN routed=diverted endpoint=passthrough", l)
	}
	if l["in_tok"] != float64(10) || l["out_tok"] != float64(5) {
		t.Errorf("log line tokens = %v, want 10/5", l)
	}
}

// TestWorkerNonRetryableStops: a 400 is a request problem, not availability —
// the error is surfaced immediately and no fallback is attempted.
func TestWorkerNonRetryableStops(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"bad request"}}`, http.StatusBadRequest)
	}))
	defer bad.Close()
	var backupHits atomic.Int64
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits.Add(1)
	}))
	defer backup.Close()

	s, _ := newFallbackServer(t, config.Config{
		Workers: []config.WorkerEndpoint{
			{Name: "primary", Base: bad.URL + "/v1", Priority: 1},
			{Name: "backup", Base: backup.URL + "/v1", Priority: 2},
		},
	})

	rw := postWorker(t, s.Handler())
	if rw.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502 (translated worker error)", rw.Code)
	}
	if backupHits.Load() != 0 {
		t.Errorf("backup was tried %d times, want 0", backupHits.Load())
	}
}

// TestWorkerChainExhausted: every endpoint failing yields a single Anthropic-
// shaped error, not a hang.
func TestWorkerChainExhausted(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no model", http.StatusServiceUnavailable)
	}))
	defer down.Close()

	s, _ := newFallbackServer(t, config.Config{
		Workers: []config.WorkerEndpoint{
			{Name: "a", Base: down.URL + "/v1", Priority: 1},
			{Name: "b", Base: down.URL + "/v1", Priority: 2},
		},
	})

	rw := postWorker(t, s.Handler())
	if rw.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", rw.Code)
	}
	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &env); err != nil || env.Type != "error" {
		t.Errorf("body %q, want Anthropic error envelope", rw.Body.String())
	}
}
