package translate

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// ============================================================================
// OpenAI streaming chunk shapes
// ============================================================================

// OpenAIStreamChunk is one `data:` frame of an OpenAI SSE stream.
type OpenAIStreamChunk struct {
	ID      string               `json:"id"`
	Choices []OpenAIStreamChoice `json:"choices"`
	Usage   *OpenAIUsage         `json:"usage,omitempty"`
	// Error is populated when the worker emits a mid-stream error frame instead
	// of a normal chunk.
	Error *OpenAIError `json:"error,omitempty"`
}

// OpenAIStreamChoice is one choice inside a streaming chunk.
type OpenAIStreamChoice struct {
	Index        int               `json:"index"`
	Delta        OpenAIStreamDelta `json:"delta"`
	FinishReason *string           `json:"finish_reason"`
	MatchedStop  json.RawMessage   `json:"matched_stop,omitempty"`
	StopReason   json.RawMessage   `json:"stop_reason,omitempty"`
}

// OpenAIStreamDelta is the incremental delta of a streaming choice.
//
// ReasoningContent is the streamed chain-of-thought of reasoning models; it maps
// to Anthropic thinking_delta events.
type OpenAIStreamDelta struct {
	Content          string                 `json:"content"`
	ReasoningContent string                 `json:"reasoning_content,omitempty"`
	ToolCalls        []OpenAIStreamToolCall `json:"tool_calls,omitempty"`
}

// OpenAIStreamToolCall is an incremental tool_call fragment.
type OpenAIStreamToolCall struct {
	Index    int                          `json:"index"`
	ID       string                       `json:"id"`
	Type     string                       `json:"type"`
	Function OpenAIStreamToolCallFunction `json:"function"`
}

// OpenAIStreamToolCallFunction is the function payload of a streamed tool call.
type OpenAIStreamToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ============================================================================
// SSE writer
// ============================================================================

// sseWriter emits Anthropic SSE events (`event:` + `data:` + blank line) and
// flushes after each when the underlying writer supports it.
type sseWriter struct {
	w       io.Writer
	flusher http.Flusher
	err     error
}

func (s *sseWriter) event(name string, data interface{}) {
	if s.err != nil {
		return
	}
	b, err := json.Marshal(data)
	if err != nil {
		s.err = err
		return
	}
	if _, err := io.WriteString(s.w, "event: "+name+"\ndata: "); err != nil {
		s.err = err
		return
	}
	if _, err := s.w.Write(b); err != nil {
		s.err = err
		return
	}
	if _, err := io.WriteString(s.w, "\n\n"); err != nil {
		s.err = err
		return
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// ============================================================================
// Streaming translation
// ============================================================================

// StreamUsage reports the token counts extracted from a worker SSE stream while
// translating it. Zero values mean the worker supplied no usage (many
// OpenAI-compatible workers omit it unless stream_options.include_usage is set).
type StreamUsage struct {
	InputTokens  int
	OutputTokens int
}

// StreamOpenAIToAnthropic translates a worker's OpenAI SSE stream to the
// Anthropic event sequence, discarding the usage counts. See
// StreamOpenAIToAnthropicUsage for the usage-reporting variant.
func StreamOpenAIToAnthropic(w io.Writer, body io.Reader, inboundModel string) error {
	_, err := StreamOpenAIToAnthropicUsage(w, body, inboundModel)
	return err
}

// StreamOpenAIToAnthropicUsage reads a worker's OpenAI SSE stream from body and writes
// the equivalent Anthropic Messages streaming event sequence to w. If w is an
// http.Flusher (e.g. http.ResponseWriter) each event is flushed immediately.
//
// The emitted sequence is:
//
//	message_start
//	ping
//	( for each content block, in order thinking -> text -> tool_use(s):
//	    content_block_start
//	    content_block_delta ...   (thinking_delta | text_delta | input_json_delta)
//	    content_block_stop )
//	message_delta   (stop_reason [+ stop_sequence] + usage.output_tokens)
//	message_stop
//
// Blocks are strictly sequential: the current block is closed with
// content_block_stop before the next block_start, matching the Anthropic contract.
// Parallel tool calls (distinct OpenAI tool indices) each become their own
// tool_use block. input_tokens is reported in message_delta usage when the worker
// supplies it (via stream_options.include_usage); message_start reports 0 because
// usage typically arrives on the final chunk.
//
// A mid-stream OpenAI error frame is translated into an Anthropic `error` event
// and terminates the stream. `[DONE]` and EOF both end the stream cleanly.
//
// It returns any write error encountered; read errors terminate the stream but
// are not treated as fatal (the partial output is already flushed).
func StreamOpenAIToAnthropicUsage(w io.Writer, body io.Reader, inboundModel string) (StreamUsage, error) {
	flusher, _ := w.(http.Flusher)
	sw := &sseWriter{w: w, flusher: flusher}

	msgID := "msg_rmll"

	sw.event("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         inboundModel,
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]interface{}{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	})
	// Optional ping right after message_start (supported by Anthropic clients).
	sw.event("ping", map[string]interface{}{"type": "ping"})

	st := &streamState{sw: sw}

	stopReason := "end_turn"
	var stopSequence interface{}
	inputTokens := 0
	outputTokens := 0

	reader := bufio.NewReader(body)
	for {
		line, readErr := reader.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")

		if strings.HasPrefix(trimmed, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if payload == "[DONE]" {
				break
			}
			if payload != "" {
				var chunk OpenAIStreamChunk
				if err := json.Unmarshal([]byte(payload), &chunk); err == nil {
					// Mid-stream error frame.
					if chunk.Error != nil {
						st.closeCurrent()
						sw.event("error", AnthropicErrorResponse{
							Type: "error",
							Error: AnthropicError{
								Type:    firstNonEmpty(mapOpenAIErrorType(chunk.Error.Type), "api_error"),
								Message: chunk.Error.Message,
							},
						})
						return StreamUsage{InputTokens: inputTokens, OutputTokens: outputTokens}, sw.err
					}
					if chunk.Usage != nil {
						outputTokens = chunk.Usage.CompletionTokens
						if chunk.Usage.PromptTokens > 0 {
							inputTokens = chunk.Usage.PromptTokens
						}
					}
					for _, choice := range chunk.Choices {
						d := choice.Delta
						if d.ReasoningContent != "" {
							st.thinkingDelta(d.ReasoningContent)
						}
						if d.Content != "" {
							st.textDelta(d.Content)
						}
						for _, tc := range d.ToolCalls {
							st.toolDelta(tc)
						}
						if choice.FinishReason != nil && *choice.FinishReason != "" {
							matched := matchedStopStreamString(choice)
							stopReason, stopSequence = MapStopReason(*choice.FinishReason, matched)
						}
					}
				}
			}
		}

		if readErr != nil {
			break // io.EOF or read error ends the stream.
		}
	}

	st.closeCurrent()

	usage := map[string]interface{}{"output_tokens": outputTokens}
	if inputTokens > 0 {
		usage["input_tokens"] = inputTokens
	}
	sw.event("message_delta", map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   stopReason,
			"stop_sequence": stopSequence,
		},
		"usage": usage,
	})
	sw.event("message_stop", map[string]interface{}{"type": "message_stop"})

	return StreamUsage{InputTokens: inputTokens, OutputTokens: outputTokens}, sw.err
}

// streamState tracks the currently-open Anthropic content block so that blocks
// are opened and closed in strict sequence.
type streamState struct {
	sw *sseWriter

	nextIndex int
	curKind   string // "" | "thinking" | "text" | "tool"
	curIndex  int
	curToolID int // OpenAI tool index backing the current tool block
}

// closeCurrent emits content_block_stop for the open block, if any.
func (s *streamState) closeCurrent() {
	if s.curKind == "" {
		return
	}
	s.sw.event("content_block_stop", map[string]interface{}{
		"type":  "content_block_stop",
		"index": s.curIndex,
	})
	s.curKind = ""
}

func (s *streamState) thinkingDelta(text string) {
	if s.curKind != "thinking" {
		s.closeCurrent()
		s.curIndex = s.nextIndex
		s.nextIndex++
		s.curKind = "thinking"
		s.sw.event("content_block_start", map[string]interface{}{
			"type":  "content_block_start",
			"index": s.curIndex,
			"content_block": map[string]interface{}{
				"type":      "thinking",
				"thinking":  "",
				"signature": "",
			},
		})
	}
	s.sw.event("content_block_delta", map[string]interface{}{
		"type":  "content_block_delta",
		"index": s.curIndex,
		"delta": map[string]interface{}{
			"type":     "thinking_delta",
			"thinking": text,
		},
	})
}

func (s *streamState) textDelta(text string) {
	if s.curKind != "text" {
		s.closeCurrent()
		s.curIndex = s.nextIndex
		s.nextIndex++
		s.curKind = "text"
		s.sw.event("content_block_start", map[string]interface{}{
			"type":          "content_block_start",
			"index":         s.curIndex,
			"content_block": map[string]interface{}{"type": "text", "text": ""},
		})
	}
	s.sw.event("content_block_delta", map[string]interface{}{
		"type":  "content_block_delta",
		"index": s.curIndex,
		"delta": map[string]interface{}{
			"type": "text_delta",
			"text": text,
		},
	})
}

func (s *streamState) toolDelta(tc OpenAIStreamToolCall) {
	if s.curKind != "tool" || s.curToolID != tc.Index {
		s.closeCurrent()
		s.curIndex = s.nextIndex
		s.nextIndex++
		s.curKind = "tool"
		s.curToolID = tc.Index
		s.sw.event("content_block_start", map[string]interface{}{
			"type":  "content_block_start",
			"index": s.curIndex,
			"content_block": map[string]interface{}{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": map[string]interface{}{},
			},
		})
	}
	if tc.Function.Arguments != "" {
		s.sw.event("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": s.curIndex,
			"delta": map[string]interface{}{
				"type":         "input_json_delta",
				"partial_json": tc.Function.Arguments,
			},
		})
	}
}

// matchedStopStreamString mirrors matchedStopString for the streaming choice shape.
func matchedStopStreamString(ch OpenAIStreamChoice) string {
	if s, ok := decodeStringOnly(ch.MatchedStop); ok {
		return s
	}
	if s, ok := decodeStringOnly(ch.StopReason); ok {
		return s
	}
	return ""
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
