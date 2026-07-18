package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/internal/stats"
)

// TestServeRecordsBothLegs drives the real routing handler end to end against
// stub upstreams and asserts the recorder folded both legs into the ledger:
// MAIN usage scraped from the Anthropic SSE, WORKER usage from the translated
// OpenAI response. This is the P6 validation ("counters match summed log").
func TestServeRecordsBothLegs(t *testing.T) {
	// MAIN upstream: a minimal Anthropic streaming response with usage.
	mainUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":500,\"output_tokens\":1}}}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":75}}\n\n")
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer mainUp.Close()

	// WORKER upstream: a non-stream OpenAI chat completion with usage.
	workerUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":300,"completion_tokens":20}}`)
	}))
	defer workerUp.Close()

	dir := t.TempDir()
	rec, err := stats.NewRecorder(dir, false)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	s := &Server{
		cfg: config.Config{
			MainUpstreamURL: mainUp.URL,
			WorkerAPIBase:   workerUp.URL + "/v1",
			DataDir:         dir,
		},
		rec: rec,
	}
	h := s.Handler()

	// WORKER leg: an inbound haiku request routes to the worker.
	post(t, h, `{"model":"claude-haiku-4-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	// MAIN leg: a non-haiku model passes through and is scraped for usage.
	post(t, h, `{"model":"claude-opus-4-8","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if err := rec.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got := readLedger(t, dir)
	if got.WorkerIn != 300 || got.WorkerOut != 20 || got.NWorker != 1 {
		t.Errorf("worker ledger = in %d out %d n %d, want 300/20/1", got.WorkerIn, got.WorkerOut, got.NWorker)
	}
	if got.MainIn != 500 || got.MainOut != 75 || got.NMain != 1 {
		t.Errorf("main ledger = in %d out %d n %d, want 500/75/1", got.MainIn, got.MainOut, got.NMain)
	}
}

func post(t *testing.T, h http.Handler, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d for %s: %s", rw.Code, body, rw.Body.String())
	}
}
