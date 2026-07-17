package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
)

// anthUsage is the usage sub-object carried by Anthropic message events and the
// non-stream response.
type anthUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// anthEvent is the subset of an Anthropic streaming event (or non-stream
// response body) that carries token usage. message_start nests usage under
// "message"; message_delta and the non-stream response carry it at top level.
type anthEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Usage *anthUsage `json:"usage"`
	} `json:"message"`
	Usage *anthUsage `json:"usage"`
}

// mainUsageScanner extracts input/output token counts from a MAIN-leg upstream
// response as its bytes flow past, without buffering a streaming response. For
// SSE it scans complete `data:` lines and drops them; for a non-stream JSON
// response it accumulates the (single, small) body and parses it on close.
type mainUsageScanner struct {
	stream bool
	buf    []byte
	in     int
	out    int
}

func newMainUsageScanner(contentType string) *mainUsageScanner {
	return &mainUsageScanner{
		stream: strings.Contains(strings.ToLower(contentType), "event-stream"),
	}
}

// feed consumes a chunk of upstream bytes. For an SSE response it parses and
// discards each completed line, keeping only a partial-line tail buffered.
func (m *mainUsageScanner) feed(p []byte) {
	m.buf = append(m.buf, p...)
	if !m.stream {
		return // non-stream: accumulate, parse once on close
	}
	for {
		i := bytes.IndexByte(m.buf, '\n')
		if i < 0 {
			break
		}
		line := m.buf[:i]
		m.buf = m.buf[i+1:]
		m.parseLine(line)
	}
}

func (m *mainUsageScanner) parseLine(line []byte) {
	line = bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("data:")))
	if len(line) == 0 || line[0] != '{' {
		return
	}
	var ev anthEvent
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	m.apply(ev)
}

// apply folds any usage counts present in the event into the running totals.
// input_tokens arrives on message_start; output_tokens is cumulative and last
// reported on message_delta, so later non-zero values overwrite earlier ones.
func (m *mainUsageScanner) apply(ev anthEvent) {
	for _, u := range []*anthUsage{usageOf(ev.Message), ev.Usage} {
		if u == nil {
			continue
		}
		if u.InputTokens > 0 {
			m.in = u.InputTokens
		}
		if u.OutputTokens > 0 {
			m.out = u.OutputTokens
		}
	}
}

func usageOf(msg *struct {
	Usage *anthUsage `json:"usage"`
}) *anthUsage {
	if msg == nil {
		return nil
	}
	return msg.Usage
}

// close finalizes parsing and returns the extracted counts. A non-stream body is
// a single JSON object parsed here; an SSE stream was parsed incrementally.
func (m *mainUsageScanner) close() (in, out int) {
	if !m.stream {
		var ev anthEvent
		if json.Unmarshal(bytes.TrimSpace(m.buf), &ev) == nil {
			m.apply(ev)
		}
	}
	return m.in, m.out
}
