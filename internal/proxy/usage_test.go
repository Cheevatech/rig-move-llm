package proxy

import "testing"

// TestMainUsageScannerSSE feeds an Anthropic streaming response in fragmented
// chunks (splitting across line boundaries) and checks token extraction:
// input_tokens from message_start, cumulative output_tokens from message_delta.
func TestMainUsageScannerSSE(t *testing.T) {
	s := newMainUsageScanner("text/event-stream; charset=utf-8")
	frames := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1200,\"output_tokens\":1}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n",
		"event: message_delta\nda", "ta: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":350}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	for _, f := range frames {
		s.feed([]byte(f))
	}
	in, cr, cw, out := s.close()
	if in != 1200 || out != 350 || cr != 0 || cw != 0 {
		t.Errorf("SSE usage = in %d cr %d cw %d out %d, want 1200/0/0/350", in, cr, cw, out)
	}
}

// TestMainUsageScannerNonStream parses a single non-stream JSON response body.
func TestMainUsageScannerNonStream(t *testing.T) {
	s := newMainUsageScanner("application/json")
	body := `{"type":"message","role":"assistant","usage":{"input_tokens":900,"output_tokens":120}}`
	s.feed([]byte(body[:40]))
	s.feed([]byte(body[40:]))
	in, cr, cw, out := s.close()
	if in != 900 || out != 120 || cr != 0 || cw != 0 {
		t.Errorf("non-stream usage = in %d cr %d cw %d out %d, want 900/0/0/120", in, cr, cw, out)
	}
}

// TestMainUsageScannerNoUsage tolerates a stream that never reports usage.
func TestMainUsageScannerNoUsage(t *testing.T) {
	s := newMainUsageScanner("text/event-stream")
	s.feed([]byte("event: ping\ndata: {\"type\":\"ping\"}\n\n"))
	if in, cr, cw, out := s.close(); in != 0 || cr != 0 || cw != 0 || out != 0 {
		t.Errorf("no-usage stream = %d/%d/%d/%d, want all 0", in, cr, cw, out)
	}
}

// TestMainUsageScannerCountsCacheTokens is the regression test for the bug this
// split exists to fix. The event body is the shape Claude Code actually receives
// on a warm session, taken from a real capture: input_tokens is 2 while the
// prompt that was really sent is 57,951 tokens, almost all of it a cache read.
// Reading only input_tokens made `rig stats` report 2.
func TestMainUsageScannerCountsCacheTokens(t *testing.T) {
	s := newMainUsageScanner("text/event-stream")
	s.feed([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":" +
		"{\"input_tokens\":2,\"cache_creation_input_tokens\":1072," +
		"\"cache_read_input_tokens\":56877,\"output_tokens\":997}}}\n\n"))
	in, cr, cw, out := s.close()
	if in != 2 || cr != 56877 || cw != 1072 || out != 997 {
		t.Fatalf("got in=%d cache_read=%d cache_write=%d out=%d, want 2/56877/1072/997", in, cr, cw, out)
	}
	if total := in + cr + cw; total != 57951 {
		t.Errorf("total input = %d, want 57951 — the number a user is trying to read", total)
	}
}
