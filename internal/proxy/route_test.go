package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/internal/stats"
)

// TestRouteAllToWorkerStreaming drives the route-cc-on-qwen prototype leg: an
// inbound Anthropic streaming request is translated to OpenAI, sent to a stub qwen
// endpoint, and the OpenAI SSE reply is translated back to the Anthropic event
// sequence CC expects. The paid upstream is never touched, and usage lands on the
// WORKER ledger.
func TestRouteAllToWorkerStreaming(t *testing.T) {
	var gotBody []byte
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("worker path = %s, want .../chat/completions", r.URL.Path)
		}
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":40,\"completion_tokens\":5}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer worker.Close()

	dir := t.TempDir()
	rec, err := stats.NewRecorder(dir, false)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	s := &Server{
		cfg: config.Config{
			MainUpstreamURL:  "http://must-not-be-called.invalid",
			WorkerAPIBase:    worker.URL,
			WorkerModel:      "qwen-coder",
			RouteAllToWorker: true,
			Enabled:          true,
			DataDir:          dir,
		},
		rec: rec,
	}
	h := s.Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-opus-4-8","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rw.Code, rw.Body.String())
	}
	// Response must be the Anthropic event sequence, not raw OpenAI.
	if !strings.Contains(rw.Body.String(), "event: message_start") ||
		!strings.Contains(rw.Body.String(), "event: message_stop") {
		t.Errorf("response is not an Anthropic SSE sequence:\n%s", rw.Body.String())
	}
	// Outbound worker request must carry the worker model, not the Anthropic one.
	var oreq struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(gotBody, &oreq)
	if oreq.Model != "qwen-coder" {
		t.Errorf("worker model = %q, want qwen-coder", oreq.Model)
	}

	if err := rec.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	got := readLedger(t, dir)
	if got.WorkerIn != 40 || got.WorkerOut != 5 || got.NWorker != 1 {
		t.Errorf("worker ledger = in %d out %d n %d, want 40/5/1", got.WorkerIn, got.WorkerOut, got.NWorker)
	}
	if got.NMain != 0 {
		t.Errorf("main ledger n = %d, want 0 (paid upstream must not be touched)", got.NMain)
	}
}

// TestRoutePrefixLegOverride verifies the per-invocation /r/<leg> path segment
// overrides the RouteAllToWorker flag in both directions: /r/main reaches the paid
// upstream even when the flag is globally on (so a cascade escalation is never
// trapped on qwen), and /r/worker reaches qwen even when the flag is off.
func TestRoutePrefixLegOverride(t *testing.T) {
	var workerHit, mainHit bool
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workerHit = true
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer worker.Close()
	main := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mainHit = true
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","type":"message","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer main.Close()

	newServer := func(routeAll bool) http.Handler {
		dir := t.TempDir()
		rec, _ := stats.NewRecorder(dir, false)
		return (&Server{cfg: config.Config{
			MainUpstreamURL:  main.URL,
			WorkerAPIBase:    worker.URL,
			WorkerModel:      "qwen",
			RouteAllToWorker: routeAll,
			Enabled:          true,
			DataDir:          dir,
		}, rec: rec}).Handler()
	}

	body := `{"model":"claude-opus-4-8","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`

	// /r/main with the flag ON must still reach the paid upstream.
	workerHit, mainHit = false, false
	if code := postPath(t, newServer(true), "/r/main/v1/messages", body); code != http.StatusOK {
		t.Fatalf("/r/main status %d", code)
	}
	if mainHit != true || workerHit != false {
		t.Errorf("/r/main (flag on): mainHit=%v workerHit=%v, want main only", mainHit, workerHit)
	}

	// /r/worker with the flag OFF must still reach qwen.
	workerHit, mainHit = false, false
	if code := postPath(t, newServer(false), "/r/worker/v1/messages", body); code != http.StatusOK {
		t.Fatalf("/r/worker status %d", code)
	}
	if workerHit != true || mainHit != false {
		t.Errorf("/r/worker (flag off): workerHit=%v mainHit=%v, want worker only", workerHit, mainHit)
	}
}

// TestRouteAllToWorkerCountTokens verifies the count_tokens shim answers locally
// (no upstream call) with a positive estimate when routing to the worker.
func TestRouteAllToWorkerCountTokens(t *testing.T) {
	s := &Server{cfg: config.Config{RouteAllToWorker: true, Enabled: true, WorkerAPIBase: "http://x"}}
	h := s.Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"count me"}]}`))
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rw.Code, rw.Body.String())
	}
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.InputTokens < 1 {
		t.Errorf("input_tokens = %d, want >= 1", out.InputTokens)
	}
}

// TestEnabledFalsePinsEveryRequestToMain is the regression test for the bug this
// gate exists to prevent: ENABLED was read by `config` and `doctor` and by nothing
// in the routing path, so `rig disable` printed "Claude Code runs normally" while
// every turn kept going to the worker. The master switch has to outrank BOTH the
// flag and an explicit /r/worker, or the word "disable" means nothing.
func TestEnabledFalsePinsEveryRequestToMain(t *testing.T) {
	var workerHit, mainHit bool
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workerHit = true
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer worker.Close()
	main := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mainHit = true
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","type":"message","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer main.Close()

	dir := t.TempDir()
	rec, _ := stats.NewRecorder(dir, false)
	h := (&Server{cfg: config.Config{
		MainUpstreamURL: main.URL,
		WorkerAPIBase:   worker.URL,
		WorkerModel:     "qwen",
		// The switch is ON and the master switch is OFF — the exact state a user
		// lands in by running `rig qwen on` and then `rig disable`.
		RouteAllToWorker: true,
		Enabled:          false,
		DataDir:          dir,
	}, rec: rec}).Handler()

	body := `{"model":"claude-opus-4-8","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`

	for _, path := range []string{"/v1/messages", "/r/worker/v1/messages"} {
		workerHit, mainHit = false, false
		if code := postPath(t, h, path, body); code != http.StatusOK {
			t.Fatalf("%s status %d", path, code)
		}
		if workerHit || !mainHit {
			t.Errorf("%s with ENABLED=false: workerHit=%v mainHit=%v, want main only",
				path, workerHit, mainHit)
		}
	}
}
