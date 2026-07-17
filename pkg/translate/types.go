// Package translate converts between the Anthropic Messages API and the OpenAI
// Chat Completions API.
//
// # Direction
//
// The library implements the "Anthropic in, OpenAI worker" direction end to end:
//
//   - RequestAnthropicToOpenAI  — an inbound Anthropic Messages request is
//     rewritten into an OpenAI Chat Completions request for a downstream worker
//     (llama.cpp, vLLM, ollama, or any OpenAI-compatible server).
//   - ResponseOpenAIToAnthropic — the worker's non-streaming Chat Completions
//     response is rewritten back into an Anthropic Messages response.
//   - StreamOpenAIToAnthropic   — the worker's OpenAI SSE stream is re-emitted as
//     the Anthropic Messages streaming event sequence.
//   - TranslateError            — an OpenAI (or generic HTTP) error body is
//     rewritten into the Anthropic error envelope.
//
// The reverse direction (OpenAI client in, Anthropic worker out) is intentionally
// out of scope; see COVERAGE.md.
//
// # Design notes
//
// Fields that are polymorphic on the wire — Anthropic `system` and message
// `content` (string OR array), tool_result `content` (string OR array), and
// `tool_choice` (string OR object) — are captured as json.RawMessage and decoded
// manually. This keeps the exported structs faithful to the JSON without forcing
// callers to construct discriminated unions.
//
// # Reasoning models
//
// Reasoning models (Qwen3.x, QwQ, DeepSeek-R1, o1-style) return their
// chain-of-thought in a non-standard `reasoning_content` field with `content`
// often empty. This library maps `reasoning_content` to an Anthropic `thinking`
// content block, in both the non-streaming and streaming paths. Thinking blocks
// are always ordered before text and tool_use blocks. Signatures are emitted as
// empty strings because OpenAI-compatible workers do not produce them.
//
// The package depends only on the Go standard library.
package translate

import "encoding/json"

// ============================================================================
// Anthropic request types (inbound)
// ============================================================================

// AnthropicRequest models the Anthropic Messages API request we consume.
//
// System and message Content are polymorphic (string OR array), so they are held
// as json.RawMessage and decoded manually. ToolChoice is likewise string-or-object.
type AnthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        json.RawMessage    `json:"system,omitempty"`
	Messages      []AnthropicMessage `json:"messages"`
	Tools         []AnthropicTool    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	TopK          *int               `json:"top_k,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
}

// AnthropicMessage is a single conversation turn. Content is string OR an array
// of AnthropicContentBlock.
type AnthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// AnthropicTool is a tool definition in the Anthropic schema.
type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// AnthropicContentBlock is one block inside a message's content array. It is a
// superset covering every block kind we read: text, image, tool_use, tool_result
// and thinking.
type AnthropicContentBlock struct {
	Type string `json:"type"`

	// text / thinking
	Text      string `json:"text,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // string OR array of blocks
	IsError   *bool           `json:"is_error,omitempty"`

	// image
	Source *AnthropicImageSource `json:"source,omitempty"`
}

// AnthropicImageSource is the `source` object of an Anthropic image block. It is
// either a base64 payload (Type=="base64") or a URL reference (Type=="url").
type AnthropicImageSource struct {
	Type      string `json:"type"`                 // "base64" | "url"
	MediaType string `json:"media_type,omitempty"` // e.g. "image/png" (base64 only)
	Data      string `json:"data,omitempty"`       // base64 bytes (base64 only)
	URL       string `json:"url,omitempty"`        // http(s) URL (url only)
}

// ============================================================================
// OpenAI request types (outbound to worker)
// ============================================================================

// OpenAIRequest is the Chat Completions request we emit to the worker.
type OpenAIRequest struct {
	Model         string          `json:"model"`
	Messages      []OpenAIMessage `json:"messages"`
	MaxTokens     int             `json:"max_tokens,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	Stop          []string        `json:"stop,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	StreamOptions *StreamOptions  `json:"stream_options,omitempty"`
	Tools         []OpenAITool    `json:"tools,omitempty"`
	ToolChoice    interface{}     `json:"tool_choice,omitempty"`
}

// StreamOptions carries OpenAI stream_options (we always request usage).
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// OpenAIMessage is a Chat Completions message. Content is string OR an array of
// OpenAIContentPart (used when images are present) OR nil (assistant message that
// carries only tool_calls).
type OpenAIMessage struct {
	Role       string           `json:"role"`
	Content    interface{}      `json:"content"`
	Name       string           `json:"name,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
}

// OpenAIContentPart is one part of a multimodal content array.
type OpenAIContentPart struct {
	Type     string          `json:"type"` // "text" | "image_url"
	Text     string          `json:"text,omitempty"`
	ImageURL *OpenAIImageURL `json:"image_url,omitempty"`
}

// OpenAIImageURL is the image_url object of a multimodal content part.
type OpenAIImageURL struct {
	URL string `json:"url"`
}

// OpenAIToolCall is a function tool call in the OpenAI schema.
type OpenAIToolCall struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Function OpenAIToolCallFunction `json:"function"`
}

// OpenAIToolCallFunction is the function payload of a tool call. Arguments is a
// JSON string.
type OpenAIToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// OpenAITool is a tool definition in the OpenAI schema.
type OpenAITool struct {
	Type     string             `json:"type"`
	Function OpenAIToolFunction `json:"function"`
}

// OpenAIToolFunction is the function definition of an OpenAI tool.
type OpenAIToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ============================================================================
// OpenAI response types (non-streaming, inbound from worker)
// ============================================================================

// OpenAIResponse is a non-streaming Chat Completions response.
type OpenAIResponse struct {
	ID      string         `json:"id"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   *OpenAIUsage   `json:"usage,omitempty"`
}

// OpenAIChoice is one completion choice.
//
// MatchedStop captures a matched stop string when the worker reports one. vLLM
// and some llama.cpp builds echo the matched stop under a choice-level
// "stop_reason" (string) or "matched_stop"; both are consulted so a `stop`
// finish caused by a stop sequence maps to Anthropic stop_reason "stop_sequence".
type OpenAIChoice struct {
	Index        int                   `json:"index"`
	Message      OpenAIResponseMessage `json:"message"`
	FinishReason string                `json:"finish_reason"`
	MatchedStop  json.RawMessage       `json:"matched_stop,omitempty"`
	StopReason   json.RawMessage       `json:"stop_reason,omitempty"`
}

// OpenAIResponseMessage is the assistant message of a completion choice.
//
// ReasoningContent is the non-standard chain-of-thought field returned by
// reasoning models; it maps to an Anthropic thinking block. Refusal is the
// OpenAI safety-refusal string.
type OpenAIResponseMessage struct {
	Role             string           `json:"role"`
	Content          string           `json:"content"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	Refusal          *string          `json:"refusal,omitempty"`
	ToolCalls        []OpenAIToolCall `json:"tool_calls,omitempty"`
}

// OpenAIUsage is Chat Completions token accounting, including the optional detail
// objects some workers provide.
type OpenAIUsage struct {
	PromptTokens            int                            `json:"prompt_tokens"`
	CompletionTokens        int                            `json:"completion_tokens"`
	TotalTokens             int                            `json:"total_tokens,omitempty"`
	PromptTokensDetails     *OpenAIPromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *OpenAICompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

// OpenAIPromptTokensDetails carries prompt-side token detail (cache hits).
type OpenAIPromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

// OpenAICompletionTokensDetails carries completion-side token detail (reasoning).
type OpenAICompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// OpenAIErrorEnvelope is the standard OpenAI error body: {"error":{...}}.
type OpenAIErrorEnvelope struct {
	Error *OpenAIError `json:"error"`
}

// OpenAIError is the inner OpenAI error object.
type OpenAIError struct {
	Message string      `json:"message"`
	Type    string      `json:"type,omitempty"`
	Code    interface{} `json:"code,omitempty"`
	Param   interface{} `json:"param,omitempty"`
}

// ============================================================================
// Anthropic response types (outbound, non-streaming)
// ============================================================================

// AnthropicResponse is a non-streaming Anthropic Messages response.
type AnthropicResponse struct {
	ID           string                     `json:"id"`
	Type         string                     `json:"type"`
	Role         string                     `json:"role"`
	Model        string                     `json:"model"`
	Content      []AnthropicContentBlockOut `json:"content"`
	StopReason   string                     `json:"stop_reason"`
	StopSequence interface{}                `json:"stop_sequence"`
	Usage        AnthropicUsage             `json:"usage"`
}

// AnthropicContentBlockOut is one emitted content block. A nil Signature is
// omitted; a thinking block sets it to a pointer so "signature":"" is emitted.
type AnthropicContentBlockOut struct {
	Type      string      `json:"type"`
	Text      string      `json:"text,omitempty"`
	Thinking  string      `json:"thinking,omitempty"`
	Signature *string     `json:"signature,omitempty"`
	ID        string      `json:"id,omitempty"`
	Name      string      `json:"name,omitempty"`
	Input     interface{} `json:"input,omitempty"`
}

// AnthropicUsage is Anthropic token accounting. Cache/detail fields are pointers
// so they are omitted unless the worker provided the underlying detail.
type AnthropicUsage struct {
	InputTokens              int  `json:"input_tokens"`
	OutputTokens             int  `json:"output_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens,omitempty"`
}

// AnthropicErrorResponse is the Anthropic error envelope.
type AnthropicErrorResponse struct {
	Type  string         `json:"type"` // always "error"
	Error AnthropicError `json:"error"`
}

// AnthropicError is the inner Anthropic error object.
type AnthropicError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}
