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
	in, out := s.close()
	if in != 1200 || out != 350 {
		t.Errorf("SSE usage = in %d out %d, want 1200/350", in, out)
	}
}

// TestMainUsageScannerNonStream parses a single non-stream JSON response body.
func TestMainUsageScannerNonStream(t *testing.T) {
	s := newMainUsageScanner("application/json")
	body := `{"type":"message","role":"assistant","usage":{"input_tokens":900,"output_tokens":120}}`
	s.feed([]byte(body[:40]))
	s.feed([]byte(body[40:]))
	in, out := s.close()
	if in != 900 || out != 120 {
		t.Errorf("non-stream usage = in %d out %d, want 900/120", in, out)
	}
}

// TestMainUsageScannerNoUsage tolerates a stream that never reports usage.
func TestMainUsageScannerNoUsage(t *testing.T) {
	s := newMainUsageScanner("text/event-stream")
	s.feed([]byte("event: ping\ndata: {\"type\":\"ping\"}\n\n"))
	if in, out := s.close(); in != 0 || out != 0 {
		t.Errorf("no-usage stream = %d/%d, want 0/0", in, out)
	}
}
