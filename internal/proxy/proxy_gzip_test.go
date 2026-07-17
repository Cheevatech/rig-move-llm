package proxy

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rigmovellm/rig-move-llm/internal/config"
	"github.com/rigmovellm/rig-move-llm/internal/stats"
)

// TestMainUsageSurvivesUpstreamCompression reproduces the live P6 parity gap:
// Claude Code advertises Accept-Encoding, the real Anthropic upstream replies
// gzip-compressed, and a verbatim header passthrough hands the usage scanner
// compressed bytes (ledger records 0/0). The proxy must strip the client's
// Accept-Encoding on scanned requests so the transport negotiates gzip itself
// and transparently decompresses for both the scanner and the client.
func TestMainUsageSurvivesUpstreamCompression(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":4751,\"output_tokens\":1}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":4}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	mainUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			t.Errorf("upstream saw Accept-Encoding %q, want gzip offered", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		io.WriteString(gz, sse)
		gz.Close()
	}))
	defer mainUp.Close()

	dir := t.TempDir()
	rec, err := stats.NewRecorder(dir, false)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	s := &Server{
		cfg: config.Config{MainUpstreamURL: mainUp.URL, DataDir: dir},
		rec: rec,
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-opus-4-8","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br") // what the Claude Code client really sends
	rw := httptest.NewRecorder()
	s.Handler().ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rw.Code, rw.Body.String())
	}
	if got := rw.Body.String(); got != sse {
		t.Errorf("client body not identity SSE:\n%q", got)
	}
	if ce := rw.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("client got Content-Encoding %q for a decompressed body", ce)
	}

	if err := rec.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	got := readLedger(t, dir)
	if got.MainIn != 4751 || got.MainOut != 4 || got.NMain != 1 {
		t.Errorf("main ledger = in %d out %d n %d, want 4751/4/1", got.MainIn, got.MainOut, got.NMain)
	}
}
