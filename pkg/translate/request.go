package translate

import (
	"encoding/json"
	"strings"
)

// RequestAnthropicToOpenAI rewrites an inbound Anthropic Messages request into an
// OpenAI Chat Completions request for a downstream worker.
//
// The workerModel argument overrides the model field so the request targets the
// worker's advertised model name rather than the Anthropic model the caller asked
// for. Anthropic-only knobs are handled as follows:
//
//   - system (string OR array of blocks) -> a leading {role:"system"} message.
//   - stop_sequences -> stop.
//   - stream -> stream, plus stream_options.include_usage so the worker reports
//     token usage on the final SSE chunk.
//   - top_k -> DROPPED. OpenAI Chat Completions has no standard top_k parameter.
//   - metadata / cache_control -> DROPPED, so strict servers accept the body.
//
// Message content blocks map as documented on translateMessage.
func RequestAnthropicToOpenAI(a AnthropicRequest, workerModel string) OpenAIRequest {
	o := OpenAIRequest{
		Model:       workerModel,
		MaxTokens:   a.MaxTokens,
		Temperature: a.Temperature,
		TopP:        a.TopP,
		Stop:        a.StopSequences,
		Stream:      a.Stream,
	}
	// top_k is intentionally dropped: no standard OpenAI Chat Completions field.
	// (a.TopK is read here only to make the drop explicit and greppable.)
	_ = a.TopK

	if a.Stream {
		o.StreamOptions = &StreamOptions{IncludeUsage: true}
	}

	// System prompt (string OR array of text blocks) -> leading system message.
	if sys := decodeAnthropicText(a.System); sys != "" {
		o.Messages = append(o.Messages, OpenAIMessage{Role: "system", Content: sys})
	}

	for _, m := range a.Messages {
		o.Messages = append(o.Messages, translateMessage(m)...)
	}

	for _, t := range a.Tools {
		o.Tools = append(o.Tools, OpenAITool{
			Type: "function",
			Function: OpenAIToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	o.ToolChoice = translateToolChoice(a.ToolChoice)

	return o
}

// translateMessage converts a single Anthropic message into one or more OpenAI
// messages.
//
// Block handling:
//
//   - text        -> merged into the message's textual content.
//   - image       -> a multimodal image_url part; the message content becomes an
//     array of parts. base64 sources become data: URLs, url sources pass through.
//   - tool_use    -> an assistant tool_calls entry (input JSON-stringified).
//   - tool_result -> a separate {role:"tool"} message. Its content may itself be
//     a string OR an array (text + image parts); an is_error:true flag is folded
//     into the text as an "[tool_error] " prefix so the worker sees the signal.
//   - thinking    -> DROPPED from history. OpenAI has no standard assistant
//     "thinking" slot, and replaying prior chain-of-thought as content pollutes
//     the context; see COVERAGE.md.
//
// tool_result blocks always split into their own role:"tool" messages, emitted in
// place so ordering relative to surrounding content is preserved.
func translateMessage(m AnthropicMessage) []OpenAIMessage {
	// content may be a bare string.
	if s, ok := decodeStringOnly(m.Content); ok {
		return []OpenAIMessage{{Role: m.Role, Content: s}}
	}

	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		// Neither string nor block array: fall back to raw text.
		return []OpenAIMessage{{Role: m.Role, Content: string(m.Content)}}
	}

	var out []OpenAIMessage
	var textParts []string
	var imageParts []OpenAIContentPart
	var toolCalls []OpenAIToolCall

	flushPrimary := func() {
		if len(textParts) == 0 && len(imageParts) == 0 && len(toolCalls) == 0 {
			return
		}
		msg := OpenAIMessage{Role: m.Role}
		switch {
		case len(imageParts) > 0:
			// Multimodal: content is an array of parts (text first, then images).
			parts := make([]OpenAIContentPart, 0, len(imageParts)+1)
			if joined := strings.Join(textParts, ""); joined != "" {
				parts = append(parts, OpenAIContentPart{Type: "text", Text: joined})
			}
			parts = append(parts, imageParts...)
			msg.Content = parts
		case len(textParts) > 0:
			msg.Content = strings.Join(textParts, "")
		default:
			// assistant message with only tool_calls -> content null.
			msg.Content = nil
		}
		if len(toolCalls) > 0 {
			msg.ToolCalls = toolCalls
		}
		out = append(out, msg)
		textParts = nil
		imageParts = nil
		toolCalls = nil
	}

	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "image":
			if part, ok := imagePart(b.Source); ok {
				imageParts = append(imageParts, part)
			}
		case "tool_use":
			args := "{}"
			if len(b.Input) > 0 {
				args = string(b.Input)
			}
			toolCalls = append(toolCalls, OpenAIToolCall{
				ID:   b.ID,
				Type: "function",
				Function: OpenAIToolCallFunction{
					Name:      b.Name,
					Arguments: args,
				},
			})
		case "tool_result":
			// Emit accumulated primary content first, then the tool message.
			flushPrimary()
			out = append(out, toolResultMessage(b))
		case "thinking", "redacted_thinking":
			// Dropped from history; see doc comment.
		default:
			if b.Text != "" {
				textParts = append(textParts, b.Text)
			}
		}
	}
	flushPrimary()

	if len(out) == 0 {
		out = append(out, OpenAIMessage{Role: m.Role, Content: ""})
	}
	return out
}

// toolResultMessage builds the {role:"tool"} message for a tool_result block.
//
// The tool_result content may be a string OR an array of blocks (text + images).
// When images are present the content becomes an array of parts; otherwise it is
// a string. An is_error:true flag is surfaced as a leading "[tool_error] " marker
// so the worker is not blind to the failure signal (OpenAI has no is_error field).
func toolResultMessage(b AnthropicContentBlock) OpenAIMessage {
	msg := OpenAIMessage{Role: "tool", ToolCallID: b.ToolUseID}

	text, images := decodeToolResultContent(b.Content)
	if b.IsError != nil && *b.IsError {
		text = "[tool_error] " + text
	}

	if len(images) > 0 {
		parts := make([]OpenAIContentPart, 0, len(images)+1)
		if text != "" {
			parts = append(parts, OpenAIContentPart{Type: "text", Text: text})
		}
		parts = append(parts, images...)
		msg.Content = parts
	} else {
		msg.Content = text
	}
	return msg
}

// imagePart converts an Anthropic image source to an OpenAI image_url part.
func imagePart(src *AnthropicImageSource) (OpenAIContentPart, bool) {
	if src == nil {
		return OpenAIContentPart{}, false
	}
	switch src.Type {
	case "base64":
		if src.Data == "" {
			return OpenAIContentPart{}, false
		}
		url := "data:" + src.MediaType + ";base64," + src.Data
		return OpenAIContentPart{Type: "image_url", ImageURL: &OpenAIImageURL{URL: url}}, true
	case "url":
		if src.URL == "" {
			return OpenAIContentPart{}, false
		}
		return OpenAIContentPart{Type: "image_url", ImageURL: &OpenAIImageURL{URL: src.URL}}, true
	default:
		return OpenAIContentPart{}, false
	}
}

// translateToolChoice maps Anthropic tool_choice to the OpenAI equivalent:
//
//	{type:"auto"}        -> "auto"
//	{type:"any"}         -> "required"
//	{type:"tool",name:X} -> {type:"function",function:{name:X}}
//	{type:"none"}        -> "none"
//	absent               -> nil (field omitted)
func translateToolChoice(raw json.RawMessage) interface{} {
	if len(raw) == 0 {
		return nil
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil
	}
	switch tc.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		return map[string]interface{}{
			"type":     "function",
			"function": map[string]string{"name": tc.Name},
		}
	default:
		return nil
	}
}

// decodeAnthropicText extracts plain text from a value that is EITHER a JSON
// string OR an array of {type:"text",text:...} blocks. Used for system prompts.
// Non-text blocks and cache_control markers are ignored.
func decodeAnthropicText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if s, ok := decodeStringOnly(raw); ok {
		return s
	}
	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "")
}

// decodeToolResultContent extracts the text and any image parts from a
// tool_result content value that is EITHER a JSON string OR an array of blocks.
func decodeToolResultContent(raw json.RawMessage) (string, []OpenAIContentPart) {
	if len(raw) == 0 {
		return "", nil
	}
	if s, ok := decodeStringOnly(raw); ok {
		return s, nil
	}
	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", nil
	}
	var parts []string
	var images []OpenAIContentPart
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			if p, ok := imagePart(b.Source); ok {
				images = append(images, p)
			}
		default:
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
	}
	return strings.Join(parts, ""), images
}

// decodeStringOnly returns (s,true) if raw is a JSON string.
func decodeStringOnly(raw json.RawMessage) (string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}
