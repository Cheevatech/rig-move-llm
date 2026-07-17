package translate

import (
	"encoding/json"
	"strings"
)

// emptyString is used to emit "signature":"" on thinking blocks (a nil pointer
// would omit the field entirely).
var emptyString = ""

// MapStopReason maps an OpenAI finish_reason to an Anthropic stop_reason and,
// when applicable, the matched stop sequence.
//
// Mapping table:
//
//	OpenAI finish_reason   Anthropic stop_reason   notes
//	--------------------   ---------------------   ---------------------------------
//	stop                   end_turn                natural stop
//	stop (+ matched stop)  stop_sequence           stop_sequence set to the match
//	length                 max_tokens
//	tool_calls             tool_use
//	function_call          tool_use                legacy alias
//	content_filter         refusal                 model output was filtered
//	(other / empty)        end_turn                conservative default
//
// matched is the stop string the worker reports (via a choice-level "stop_reason"
// or "matched_stop" field); pass "" when unknown. When finish_reason is "stop"
// and matched is non-empty, the result is ("stop_sequence", matched).
func MapStopReason(finish, matched string) (reason string, stopSequence interface{}) {
	switch finish {
	case "stop":
		if matched != "" {
			return "stop_sequence", matched
		}
		return "end_turn", nil
	case "length":
		return "max_tokens", nil
	case "tool_calls", "function_call":
		return "tool_use", nil
	case "content_filter":
		// Anthropic has no dedicated content_filter reason; "refusal" is the
		// closest native stop_reason for filtered/declined output.
		return "refusal", nil
	default:
		return "end_turn", nil
	}
}

// ResponseOpenAIToAnthropic rewrites a non-streaming OpenAI Chat Completions
// response into an Anthropic Messages response.
//
// inboundModel is echoed into the Anthropic `model` field (the caller asked for
// an Anthropic model; the worker's model name is not leaked back).
//
// Behaviour:
//
//   - Only choices[0] is used. Anthropic messages are single-response; n>1 is not
//     represented in the Anthropic schema, so additional choices are discarded.
//   - Content block order is thinking (from reasoning_content) -> text (from
//     content) -> tool_use (from tool_calls).
//   - A message.refusal string is surfaced as a text block and forces
//     stop_reason "refusal".
//   - stop_reason follows MapStopReason; a matched stop sequence is honoured.
//   - usage maps prompt_tokens->input_tokens and completion_tokens->output_tokens;
//     prompt_tokens_details.cached_tokens->cache_read_input_tokens.
//     completion_tokens_details.reasoning_tokens has no Anthropic field and is
//     dropped (the reasoning text itself is preserved as a thinking block).
func ResponseOpenAIToAnthropic(o OpenAIResponse, inboundModel string) AnthropicResponse {
	resp := AnthropicResponse{
		ID:           o.ID,
		Type:         "message",
		Role:         "assistant",
		Model:        inboundModel,
		StopReason:   "end_turn",
		StopSequence: nil,
	}
	if resp.ID == "" {
		resp.ID = "msg_rigmovellm"
	}

	var content []AnthropicContentBlockOut

	if len(o.Choices) > 0 {
		ch := o.Choices[0]
		matched := matchedStopString(ch)
		resp.StopReason, resp.StopSequence = MapStopReason(ch.FinishReason, matched)

		// 1. thinking (reasoning_content), always first.
		if ch.Message.ReasoningContent != "" {
			content = append(content, AnthropicContentBlockOut{
				Type:      "thinking",
				Thinking:  ch.Message.ReasoningContent,
				Signature: &emptyString,
			})
		}

		// 2. refusal takes precedence over normal text; force stop_reason refusal.
		if ch.Message.Refusal != nil && *ch.Message.Refusal != "" {
			content = append(content, AnthropicContentBlockOut{
				Type: "text",
				Text: *ch.Message.Refusal,
			})
			resp.StopReason = "refusal"
			resp.StopSequence = nil
		} else if ch.Message.Content != "" {
			// 2. text.
			content = append(content, AnthropicContentBlockOut{
				Type: "text",
				Text: ch.Message.Content,
			})
		}

		// 3. tool_use.
		for _, tc := range ch.Message.ToolCalls {
			content = append(content, AnthropicContentBlockOut{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: parseJSONOrEmpty(tc.Function.Arguments),
			})
		}
	}

	if content == nil {
		content = []AnthropicContentBlockOut{}
	}
	resp.Content = content
	resp.Usage = mapUsage(o.Usage)
	return resp
}

// mapUsage converts OpenAI usage to Anthropic usage, passing through cache detail
// when present.
func mapUsage(u *OpenAIUsage) AnthropicUsage {
	if u == nil {
		return AnthropicUsage{}
	}
	out := AnthropicUsage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
	}
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0 {
		cached := u.PromptTokensDetails.CachedTokens
		out.CacheReadInputTokens = &cached
	}
	// completion_tokens_details.reasoning_tokens has no Anthropic counterpart and
	// is intentionally not mapped; the reasoning text is preserved as a thinking
	// block instead.
	return out
}

// matchedStopString returns the matched stop sequence a worker reports, if any.
// vLLM/llama.cpp expose it via a choice-level "stop_reason" or "matched_stop"
// that is a JSON string when a textual stop sequence was hit (an integer token id
// is ignored).
func matchedStopString(ch OpenAIChoice) string {
	if s, ok := decodeStringOnly(ch.MatchedStop); ok {
		return s
	}
	if s, ok := decodeStringOnly(ch.StopReason); ok {
		return s
	}
	return ""
}

// parseJSONOrEmpty parses a JSON string into a generic value; on failure returns
// an empty object so Anthropic's tool_use `input` is always an object.
func parseJSONOrEmpty(s string) interface{} {
	s = strings.TrimSpace(s)
	if s == "" {
		return map[string]interface{}{}
	}
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return map[string]interface{}{}
	}
	return v
}

// ============================================================================
// Error translation
// ============================================================================

// TranslateError rewrites a worker error into the Anthropic error envelope.
//
// It accepts the HTTP status code and the raw response body. If the body is a
// standard OpenAI error ({"error":{message,type,code}}) its message is preserved
// and its type is honoured when recognisable; otherwise the body text is used as
// the message. The Anthropic error type is derived from the OpenAI error type
// when known, falling back to the HTTP status:
//
//	400 invalid_request_error   401 authentication_error
//	403 permission_error        404 not_found_error
//	429 rate_limit_error        500 api_error
//	503 overloaded_error        other -> api_error
func TranslateError(status int, body []byte) AnthropicErrorResponse {
	msg := strings.TrimSpace(string(body))
	var errType string

	var env OpenAIErrorEnvelope
	if err := json.Unmarshal(body, &env); err == nil && env.Error != nil {
		if env.Error.Message != "" {
			msg = env.Error.Message
		}
		errType = mapOpenAIErrorType(env.Error.Type)
	}
	if errType == "" {
		errType = mapStatusToErrorType(status)
	}
	if msg == "" {
		msg = "upstream worker error"
	}

	return AnthropicErrorResponse{
		Type: "error",
		Error: AnthropicError{
			Type:    errType,
			Message: msg,
		},
	}
}

// mapOpenAIErrorType maps a known OpenAI error.type to an Anthropic error type.
// Unknown/empty types return "" so the caller falls back to the HTTP status.
func mapOpenAIErrorType(t string) string {
	switch t {
	case "invalid_request_error":
		return "invalid_request_error"
	case "authentication_error":
		return "authentication_error"
	case "permission_error", "permission_denied":
		return "permission_error"
	case "not_found_error":
		return "not_found_error"
	case "rate_limit_error", "rate_limit_exceeded", "insufficient_quota":
		return "rate_limit_error"
	case "server_error", "api_error", "internal_error":
		return "api_error"
	case "overloaded_error":
		return "overloaded_error"
	default:
		return ""
	}
}

// mapStatusToErrorType maps an HTTP status to an Anthropic error type.
func mapStatusToErrorType(status int) string {
	switch status {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 429:
		return "rate_limit_error"
	case 500:
		return "api_error"
	case 503:
		return "overloaded_error"
	default:
		return "api_error"
	}
}
