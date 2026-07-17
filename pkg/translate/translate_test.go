package translate

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// mustJSON marshals v or fails the test.
func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// decodeReq parses a raw Anthropic request body.
func decodeReq(t *testing.T, body string) AnthropicRequest {
	t.Helper()
	var a AnthropicRequest
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return a
}

// ---------------------------------------------------------------------------
// Request translation
// ---------------------------------------------------------------------------

func TestRequestParamsAndTextRoundtrip(t *testing.T) {
	temp := 0.7
	topP := 0.9
	topK := 40
	a := AnthropicRequest{
		Model:         "claude-3-5-sonnet",
		MaxTokens:     512,
		Temperature:   &temp,
		TopP:          &topP,
		TopK:          &topK,
		StopSequences: []string{"STOP", "END"},
		Stream:        true,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"hello world"`)},
		},
	}
	o := RequestAnthropicToOpenAI(a, "qwen-27b-coder")

	if o.Model != "qwen-27b-coder" {
		t.Errorf("model override: got %q", o.Model)
	}
	if o.MaxTokens != 512 || o.Temperature == nil || *o.Temperature != 0.7 || o.TopP == nil {
		t.Errorf("param passthrough failed: %+v", o)
	}
	if len(o.Stop) != 2 || o.Stop[0] != "STOP" {
		t.Errorf("stop_sequences->stop failed: %v", o.Stop)
	}
	if !o.Stream || o.StreamOptions == nil || !o.StreamOptions.IncludeUsage {
		t.Errorf("stream_options.include_usage not set")
	}
	if len(o.Messages) != 1 || o.Messages[0].Content != "hello world" {
		t.Errorf("text roundtrip failed: %+v", o.Messages)
	}
}

func TestSystemArray(t *testing.T) {
	body := `{
		"model":"claude","max_tokens":10,
		"system":[
			{"type":"text","text":"You are helpful.","cache_control":{"type":"ephemeral"}},
			{"type":"text","text":" Be concise."}
		],
		"messages":[{"role":"user","content":"hi"}]
	}`
	o := RequestAnthropicToOpenAI(decodeReq(t, body), "w")
	if len(o.Messages) != 2 {
		t.Fatalf("expected system + user, got %d", len(o.Messages))
	}
	if o.Messages[0].Role != "system" || o.Messages[0].Content != "You are helpful. Be concise." {
		t.Errorf("system array merge failed: %+v", o.Messages[0])
	}
}

func TestSystemString(t *testing.T) {
	body := `{"model":"c","max_tokens":10,"system":"sys","messages":[{"role":"user","content":"hi"}]}`
	o := RequestAnthropicToOpenAI(decodeReq(t, body), "w")
	if o.Messages[0].Role != "system" || o.Messages[0].Content != "sys" {
		t.Errorf("system string failed: %+v", o.Messages[0])
	}
}

func TestToolsAndToolUseAndToolResult(t *testing.T) {
	body := `{
		"model":"c","max_tokens":10,
		"tools":[{"name":"get_weather","description":"gets weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],
		"messages":[
			{"role":"user","content":"weather?"},
			{"role":"assistant","content":[
				{"type":"text","text":"let me check"},
				{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Paris"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":"sunny"}
			]}
		]
	}`
	o := RequestAnthropicToOpenAI(decodeReq(t, body), "w")

	// tool definition
	if len(o.Tools) != 1 || o.Tools[0].Type != "function" || o.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("tool def failed: %+v", o.Tools)
	}
	var params map[string]interface{}
	if err := json.Unmarshal(o.Tools[0].Function.Parameters, &params); err != nil || params["type"] != "object" {
		t.Errorf("input_schema->parameters failed: %v", err)
	}

	// assistant with text + tool_call
	asst := o.Messages[1]
	if asst.Role != "assistant" || asst.Content != "let me check" || len(asst.ToolCalls) != 1 {
		t.Fatalf("assistant tool_use failed: %+v", asst)
	}
	if asst.ToolCalls[0].ID != "toolu_1" || asst.ToolCalls[0].Function.Name != "get_weather" {
		t.Errorf("tool_call fields failed: %+v", asst.ToolCalls[0])
	}
	if !strings.Contains(asst.ToolCalls[0].Function.Arguments, `"city":"Paris"`) {
		t.Errorf("tool_call arguments stringify failed: %q", asst.ToolCalls[0].Function.Arguments)
	}

	// tool_result -> role:tool message
	tool := o.Messages[2]
	if tool.Role != "tool" || tool.ToolCallID != "toolu_1" || tool.Content != "sunny" {
		t.Errorf("tool_result failed: %+v", tool)
	}
}

func TestToolResultIsError(t *testing.T) {
	body := `{"model":"c","max_tokens":10,"messages":[
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"boom"}]}
	]}`
	o := RequestAnthropicToOpenAI(decodeReq(t, body), "w")
	if got, ok := o.Messages[0].Content.(string); !ok || !strings.HasPrefix(got, "[tool_error] ") {
		t.Errorf("is_error marker missing: %+v", o.Messages[0].Content)
	}
}

func TestToolChoiceVariants(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want interface{}
	}{
		{"auto", `{"type":"auto"}`, "auto"},
		{"any", `{"type":"any"}`, "required"},
		{"none", `{"type":"none"}`, "none"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := AnthropicRequest{ToolChoice: json.RawMessage(c.in), Messages: []AnthropicMessage{}}
			o := RequestAnthropicToOpenAI(a, "w")
			if o.ToolChoice != c.want {
				t.Errorf("tool_choice %s: got %v want %v", c.name, o.ToolChoice, c.want)
			}
		})
	}
	t.Run("tool", func(t *testing.T) {
		a := AnthropicRequest{ToolChoice: json.RawMessage(`{"type":"tool","name":"foo"}`)}
		o := RequestAnthropicToOpenAI(a, "w")
		m, ok := o.ToolChoice.(map[string]interface{})
		if !ok || m["type"] != "function" {
			t.Fatalf("tool choice object failed: %+v", o.ToolChoice)
		}
		fn := m["function"].(map[string]string)
		if fn["name"] != "foo" {
			t.Errorf("tool choice name failed: %+v", fn)
		}
	})
	t.Run("absent", func(t *testing.T) {
		o := RequestAnthropicToOpenAI(AnthropicRequest{}, "w")
		if o.ToolChoice != nil {
			t.Errorf("absent tool_choice should be nil, got %v", o.ToolChoice)
		}
	})
}

func TestImageRequestBase64AndURL(t *testing.T) {
	body := `{"model":"c","max_tokens":10,"messages":[
		{"role":"user","content":[
			{"type":"text","text":"what is this?"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},
			{"type":"image","source":{"type":"url","url":"https://x/y.jpg"}}
		]}
	]}`
	o := RequestAnthropicToOpenAI(decodeReq(t, body), "w")
	parts, ok := o.Messages[0].Content.([]OpenAIContentPart)
	if !ok || len(parts) != 3 {
		t.Fatalf("expected 3 multimodal parts, got %#v", o.Messages[0].Content)
	}
	if parts[0].Type != "text" || parts[0].Text != "what is this?" {
		t.Errorf("text part wrong: %+v", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL.URL != "data:image/png;base64,AAAA" {
		t.Errorf("base64 image_url wrong: %+v", parts[1].ImageURL)
	}
	if parts[2].ImageURL.URL != "https://x/y.jpg" {
		t.Errorf("url image_url wrong: %+v", parts[2].ImageURL)
	}
}

func TestThinkingInHistoryDropped(t *testing.T) {
	body := `{"model":"c","max_tokens":10,"messages":[
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"secret cot","signature":"sig"},
			{"type":"text","text":"visible answer"}
		]}
	]}`
	o := RequestAnthropicToOpenAI(decodeReq(t, body), "w")
	if len(o.Messages) != 1 || o.Messages[0].Content != "visible answer" {
		t.Errorf("thinking should be dropped, only text kept: %+v", o.Messages)
	}
}

// ---------------------------------------------------------------------------
// Response translation (non-streaming)
// ---------------------------------------------------------------------------

func TestResponseTextAndUsage(t *testing.T) {
	o := OpenAIResponse{
		ID:      "chatcmpl-1",
		Choices: []OpenAIChoice{{Message: OpenAIResponseMessage{Content: "hi there"}, FinishReason: "stop"}},
		Usage: &OpenAIUsage{
			PromptTokens:        10,
			CompletionTokens:    5,
			PromptTokensDetails: &OpenAIPromptTokensDetails{CachedTokens: 4},
		},
	}
	r := ResponseOpenAIToAnthropic(o, "claude-3-5-sonnet")
	if r.Type != "message" || r.Role != "assistant" || r.Model != "claude-3-5-sonnet" {
		t.Errorf("envelope wrong: %+v", r)
	}
	if len(r.Content) != 1 || r.Content[0].Type != "text" || r.Content[0].Text != "hi there" {
		t.Fatalf("text block wrong: %+v", r.Content)
	}
	if r.StopReason != "end_turn" {
		t.Errorf("stop_reason: got %q", r.StopReason)
	}
	if r.Usage.InputTokens != 10 || r.Usage.OutputTokens != 5 {
		t.Errorf("usage map wrong: %+v", r.Usage)
	}
	if r.Usage.CacheReadInputTokens == nil || *r.Usage.CacheReadInputTokens != 4 {
		t.Errorf("cached tokens passthrough failed: %+v", r.Usage.CacheReadInputTokens)
	}
}

func TestResponseReasoningContentToThinking(t *testing.T) {
	// The verified real-world payload: content empty, reasoning_content populated.
	raw := `{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":"","reasoning_content":"Here's a thinking process:\n1. Analyze..."}}]}`
	var o OpenAIResponse
	if err := json.Unmarshal([]byte(raw), &o); err != nil {
		t.Fatal(err)
	}
	r := ResponseOpenAIToAnthropic(o, "claude")
	if len(r.Content) != 1 {
		t.Fatalf("expected 1 thinking block, got %d: %+v", len(r.Content), r.Content)
	}
	b := r.Content[0]
	if b.Type != "thinking" || !strings.HasPrefix(b.Thinking, "Here's a thinking process") {
		t.Fatalf("thinking block wrong: %+v", b)
	}
	if b.Signature == nil || *b.Signature != "" {
		t.Errorf("thinking signature should be empty string, got %v", b.Signature)
	}
	// signature:"" must be present in the JSON output
	js := mustJSON(t, b)
	if !strings.Contains(js, `"signature":""`) {
		t.Errorf("emitted thinking block missing signature field: %s", js)
	}
	if r.StopReason != "max_tokens" {
		t.Errorf("stop_reason for length: got %q", r.StopReason)
	}
}

func TestResponseThinkingBeforeTextBeforeToolOrder(t *testing.T) {
	o := OpenAIResponse{
		Choices: []OpenAIChoice{{
			FinishReason: "tool_calls",
			Message: OpenAIResponseMessage{
				ReasoningContent: "let me think",
				Content:          "here is text",
				ToolCalls: []OpenAIToolCall{
					{ID: "t1", Type: "function", Function: OpenAIToolCallFunction{Name: "f", Arguments: `{"a":1}`}},
				},
			},
		}},
	}
	r := ResponseOpenAIToAnthropic(o, "c")
	if len(r.Content) != 3 {
		t.Fatalf("expected 3 blocks, got %d", len(r.Content))
	}
	if r.Content[0].Type != "thinking" || r.Content[1].Type != "text" || r.Content[2].Type != "tool_use" {
		t.Errorf("block order wrong: %s %s %s", r.Content[0].Type, r.Content[1].Type, r.Content[2].Type)
	}
	if m, ok := r.Content[2].Input.(map[string]interface{}); !ok || m["a"].(float64) != 1 {
		t.Errorf("tool input parse wrong: %+v", r.Content[2].Input)
	}
	if r.StopReason != "tool_use" {
		t.Errorf("stop_reason for tool_calls: got %q", r.StopReason)
	}
}

func TestStopReasonMapping(t *testing.T) {
	cases := []struct {
		finish, matched, wantReason string
		wantSeq                     interface{}
	}{
		{"stop", "", "end_turn", nil},
		{"stop", "END", "stop_sequence", "END"},
		{"length", "", "max_tokens", nil},
		{"tool_calls", "", "tool_use", nil},
		{"function_call", "", "tool_use", nil},
		{"content_filter", "", "refusal", nil},
		{"weird", "", "end_turn", nil},
	}
	for _, c := range cases {
		gotReason, gotSeq := MapStopReason(c.finish, c.matched)
		if gotReason != c.wantReason || gotSeq != c.wantSeq {
			t.Errorf("MapStopReason(%q,%q) = (%q,%v) want (%q,%v)",
				c.finish, c.matched, gotReason, gotSeq, c.wantReason, c.wantSeq)
		}
	}
}

func TestStopSequenceFromChoice(t *testing.T) {
	raw := `{"choices":[{"finish_reason":"stop","stop_reason":"<<END>>","message":{"content":"done"}}]}`
	var o OpenAIResponse
	if err := json.Unmarshal([]byte(raw), &o); err != nil {
		t.Fatal(err)
	}
	r := ResponseOpenAIToAnthropic(o, "c")
	if r.StopReason != "stop_sequence" || r.StopSequence != "<<END>>" {
		t.Errorf("matched stop sequence not honoured: reason=%q seq=%v", r.StopReason, r.StopSequence)
	}
}

func TestRefusal(t *testing.T) {
	refusal := "I can't help with that."
	o := OpenAIResponse{
		Choices: []OpenAIChoice{{
			FinishReason: "stop",
			Message:      OpenAIResponseMessage{Refusal: &refusal},
		}},
	}
	r := ResponseOpenAIToAnthropic(o, "c")
	if r.StopReason != "refusal" {
		t.Errorf("refusal stop_reason: got %q", r.StopReason)
	}
	if len(r.Content) != 1 || r.Content[0].Type != "text" || r.Content[0].Text != refusal {
		t.Errorf("refusal text block wrong: %+v", r.Content)
	}
}

func TestResponseMultipleChoicesUsesFirst(t *testing.T) {
	o := OpenAIResponse{Choices: []OpenAIChoice{
		{Message: OpenAIResponseMessage{Content: "first"}, FinishReason: "stop"},
		{Message: OpenAIResponseMessage{Content: "second"}, FinishReason: "stop"},
	}}
	r := ResponseOpenAIToAnthropic(o, "c")
	if len(r.Content) != 1 || r.Content[0].Text != "first" {
		t.Errorf("should use choices[0]: %+v", r.Content)
	}
}

// ---------------------------------------------------------------------------
// Error translation
// ---------------------------------------------------------------------------

func TestTranslateErrorOpenAIBody(t *testing.T) {
	body := []byte(`{"error":{"message":"model not found","type":"not_found_error","code":"model_missing"}}`)
	e := TranslateError(404, body)
	if e.Type != "error" || e.Error.Type != "not_found_error" || e.Error.Message != "model not found" {
		t.Errorf("openai error translation failed: %+v", e)
	}
}

func TestTranslateErrorStatusFallback(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{400, "invalid_request_error"},
		{401, "authentication_error"},
		{429, "rate_limit_error"},
		{500, "api_error"},
		{503, "overloaded_error"},
		{418, "api_error"},
	}
	for _, c := range cases {
		e := TranslateError(c.status, []byte("plain text failure"))
		if e.Error.Type != c.want {
			t.Errorf("status %d: got %q want %q", c.status, e.Error.Type, c.want)
		}
		if e.Error.Message != "plain text failure" {
			t.Errorf("status %d: message not preserved: %q", c.status, e.Error.Message)
		}
	}
}

// ---------------------------------------------------------------------------
// Streaming translation
// ---------------------------------------------------------------------------

// sseEvent is a parsed Anthropic SSE event.
type sseEvent struct {
	name string
	data map[string]interface{}
}

// parseSSE splits an Anthropic SSE byte stream into ordered events.
func parseSSE(t *testing.T, raw string) []sseEvent {
	t.Helper()
	var events []sseEvent
	for _, block := range strings.Split(raw, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				name = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		var m map[string]interface{}
		if data != "" {
			if err := json.Unmarshal([]byte(data), &m); err != nil {
				t.Fatalf("bad SSE data %q: %v", data, err)
			}
		}
		events = append(events, sseEvent{name: name, data: m})
	}
	return events
}

// names returns the ordered event names.
func names(evs []sseEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.name
	}
	return out
}

// runStream builds an OpenAI SSE input from the given data payloads and returns
// the parsed Anthropic events.
func runStream(t *testing.T, inboundModel string, chunks ...string) []sseEvent {
	t.Helper()
	var in strings.Builder
	for _, c := range chunks {
		in.WriteString("data: " + c + "\n\n")
	}
	in.WriteString("data: [DONE]\n\n")
	var out bytes.Buffer
	if err := StreamOpenAIToAnthropic(&out, strings.NewReader(in.String()), inboundModel); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	return parseSSE(t, out.String())
}

func TestStreamText(t *testing.T) {
	evs := runStream(t, "claude",
		`{"choices":[{"delta":{"content":"Hel"}}]}`,
		`{"choices":[{"delta":{"content":"lo"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2}}`,
	)
	got := names(evs)
	want := []string{"message_start", "ping", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence:\n got  %v\n want %v", got, want)
	}
	// text deltas
	if evs[3].data["delta"].(map[string]interface{})["text"] != "Hel" {
		t.Errorf("first text delta wrong: %+v", evs[3].data)
	}
	// message_delta stop + usage
	md := evs[6].data
	if md["delta"].(map[string]interface{})["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason wrong: %+v", md)
	}
	usage := md["usage"].(map[string]interface{})
	if usage["output_tokens"].(float64) != 2 || usage["input_tokens"].(float64) != 7 {
		t.Errorf("usage wrong: %+v", usage)
	}
}

func TestStreamReasoningToThinking(t *testing.T) {
	evs := runStream(t, "c",
		`{"choices":[{"delta":{"reasoning_content":"think "}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"more"}}]}`,
		`{"choices":[{"delta":{"content":"answer"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	got := names(evs)
	// thinking block (start + 2 deltas + stop) then text block (start + delta + stop)
	want := []string{
		"message_start", "ping",
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop",
		"content_block_start", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence:\n got  %v\n want %v", got, want)
	}
	// first block is thinking at index 0
	start0 := evs[2].data
	if start0["index"].(float64) != 0 || start0["content_block"].(map[string]interface{})["type"] != "thinking" {
		t.Errorf("first block not thinking@0: %+v", start0)
	}
	// thinking delta type
	if evs[3].data["delta"].(map[string]interface{})["type"] != "thinking_delta" {
		t.Errorf("expected thinking_delta: %+v", evs[3].data)
	}
	if evs[3].data["delta"].(map[string]interface{})["thinking"] != "think " {
		t.Errorf("thinking text wrong: %+v", evs[3].data)
	}
	// second block is text at index 1
	start1 := evs[6].data
	if start1["index"].(float64) != 1 || start1["content_block"].(map[string]interface{})["type"] != "text" {
		t.Errorf("second block not text@1: %+v", start1)
	}
}

func TestStreamThinkingTextParallelToolsIndexOrder(t *testing.T) {
	evs := runStream(t, "c",
		`{"choices":[{"delta":{"reasoning_content":"cot"}}]}`,
		`{"choices":[{"delta":{"content":"txt"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"f0","arguments":"{\"x\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"f1","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)

	// Collect (event, index, block-type) for structural checks.
	type blk struct {
		typ   string
		index int
	}
	var starts []blk
	for _, e := range evs {
		if e.name == "content_block_start" {
			cb := e.data["content_block"].(map[string]interface{})
			starts = append(starts, blk{typ: cb["type"].(string), index: int(e.data["index"].(float64))})
		}
	}
	// Expected: thinking@0, text@1, tool_use@2, tool_use@3
	want := []blk{{"thinking", 0}, {"text", 1}, {"tool_use", 2}, {"tool_use", 3}}
	if len(starts) != len(want) {
		t.Fatalf("expected %d block starts, got %d (%+v)", len(want), len(starts), starts)
	}
	for i := range want {
		if starts[i] != want[i] {
			t.Errorf("block %d: got %+v want %+v", i, starts[i], want[i])
		}
	}

	// Verify exactly one content_block_stop per start and correct final stop_reason.
	stops := 0
	for _, e := range evs {
		if e.name == "content_block_stop" {
			stops++
		}
	}
	if stops != 4 {
		t.Errorf("expected 4 content_block_stop, got %d", stops)
	}
	last := evs[len(evs)-2] // message_delta before message_stop
	if last.name != "message_delta" || last.data["delta"].(map[string]interface{})["stop_reason"] != "tool_use" {
		t.Errorf("final stop_reason wrong: %+v", last.data)
	}

	// Verify input_json_delta carries partial_json for tool index 0.
	var jsonDeltas []string
	for _, e := range evs {
		if e.name == "content_block_delta" {
			d := e.data["delta"].(map[string]interface{})
			if d["type"] == "input_json_delta" && int(e.data["index"].(float64)) == 2 {
				jsonDeltas = append(jsonDeltas, d["partial_json"].(string))
			}
		}
	}
	if strings.Join(jsonDeltas, "") != `{"x":1}` {
		t.Errorf("tool 0 partial_json wrong: %q", strings.Join(jsonDeltas, ""))
	}
}

func TestStreamMidStreamError(t *testing.T) {
	in := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n" +
		"data: {\"error\":{\"message\":\"worker exploded\",\"type\":\"server_error\"}}\n\n"
	var out bytes.Buffer
	if err := StreamOpenAIToAnthropic(&out, strings.NewReader(in), "c"); err != nil {
		t.Fatalf("stream returned err: %v", err)
	}
	evs := parseSSE(t, out.String())
	last := evs[len(evs)-1]
	if last.name != "error" {
		t.Fatalf("expected final error event, got %q (%v)", last.name, names(evs))
	}
	errObj := last.data["error"].(map[string]interface{})
	if errObj["message"] != "worker exploded" || errObj["type"] != "api_error" {
		t.Errorf("error event wrong: %+v", errObj)
	}
	// Open text block must have been closed before the error.
	closed := false
	for _, e := range evs {
		if e.name == "content_block_stop" {
			closed = true
		}
	}
	if !closed {
		t.Errorf("open block not closed before error event")
	}
}

func TestStreamStopSequence(t *testing.T) {
	evs := runStream(t, "c",
		`{"choices":[{"delta":{"content":"x"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop","stop_reason":"HALT"}]}`,
	)
	md := evs[len(evs)-2].data
	delta := md["delta"].(map[string]interface{})
	if delta["stop_reason"] != "stop_sequence" || delta["stop_sequence"] != "HALT" {
		t.Errorf("stream stop_sequence wrong: %+v", delta)
	}
}
