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

// TestServeRecordsMainLeg drives the real routing handler end to end against a
// stub Anthropic upstream and asserts the recorder scraped the MAIN-leg usage
// from the streamed SSE into the ledger (the P6 "counters match summed log"
// validation). Offload no longer traverses the proxy (worker leg removed in
// P10-B), so there is a single billable leg to account for.
func TestServeRecordsMainLeg(t *testing.T) {
	// MAIN upstream: a minimal Anthropic streaming response with usage.
	mainUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":500,\"output_tokens\":1}}}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":75}}\n\n")
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer mainUp.Close()

	dir := t.TempDir()
	rec, err := stats.NewRecorder(dir, false)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	s := &Server{
		cfg: config.Config{
			MainUpstreamURL: mainUp.URL,
			DataDir:         dir,
		},
		rec: rec,
	}
	h := s.Handler()

	// MAIN leg: a normal model passes through and is scraped for usage.
	post(t, h, `{"model":"claude-opus-4-8","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if err := rec.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got := readLedger(t, dir)
	if got.MainIn != 500 || got.MainOut != 75 || got.NMain != 1 {
		t.Errorf("main ledger = in %d out %d n %d, want 500/75/1", got.MainIn, got.MainOut, got.NMain)
	}
	if got.WorkerIn != 0 || got.WorkerOut != 0 || got.NWorker != 0 {
		t.Errorf("worker ledger = in %d out %d n %d, want all 0 (worker leg removed)", got.WorkerIn, got.WorkerOut, got.NWorker)
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
