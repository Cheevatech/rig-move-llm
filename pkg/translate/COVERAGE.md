# `pkg/translate` — Coverage Matrix

A translator between the **Anthropic Messages API** and the **OpenAI Chat
Completions API**, standard-library only.

## Direction

This package implements ONE direction end to end:

```
Anthropic client  ──►  RequestAnthropicToOpenAI  ──►  OpenAI-compatible worker
                                                       (llama.cpp / vLLM / ollama)
Anthropic client  ◄──  ResponseOpenAIToAnthropic ◄──  worker (non-streaming)
Anthropic client  ◄──  StreamOpenAIToAnthropic   ◄──  worker (SSE stream)
Anthropic client  ◄──  TranslateError            ◄──  worker (error body)
```

The **reverse** direction (an OpenAI client talking to an Anthropic worker) is a
future extension and is **not** implemented. All entry points are exported:
`RequestAnthropicToOpenAI`, `ResponseOpenAIToAnthropic`, `StreamOpenAIToAnthropic`,
`TranslateError`, and the helper `MapStopReason`.

Legend: ✅ full · ⚠️ partial (see note) · ❌ not applicable (see why)

## Request — Anthropic Messages → OpenAI Chat Completions

| Feature | Status | Notes |
|---|---|---|
| `model` | ✅ | Overridden with the caller-supplied `workerModel`. |
| `max_tokens` | ✅ | → `max_tokens`. |
| `temperature` | ✅ | Pointer preserved (0 vs. unset distinguished). |
| `top_p` | ✅ | Pointer preserved. |
| `top_k` | ❌ | **Dropped** — OpenAI Chat Completions has no standard `top_k`. Read explicitly in code to document the drop. |
| `stop_sequences` | ✅ | → `stop`. |
| `stream` | ✅ | → `stream` + `stream_options.include_usage:true` so the worker reports token usage on the final SSE chunk. |
| `metadata` | ❌ | Dropped; no OpenAI equivalent, and strict servers reject unknown fields. |
| `system` (string) | ✅ | → leading `{role:"system"}` message. |
| `system` (array of blocks) | ✅ | Text blocks concatenated into one system message. `cache_control` ignored. |
| content: `text` | ✅ | Merged into the message content. |
| content: `image` (base64) | ✅ | → `{type:"image_url",image_url:{url:"data:<media>;base64,<data>"}}`; message content becomes an array of parts. |
| content: `image` (url) | ✅ | → `{type:"image_url",image_url:{url:<url>}}`. |
| content: `tool_use` | ✅ | → assistant `tool_calls[]` with `arguments` = JSON-stringified `input`. |
| content: `tool_result` (string) | ✅ | → `{role:"tool",tool_call_id,content}`. |
| content: `tool_result` (array text+image) | ✅ | Text joined; images become `image_url` parts (array content). |
| content: `tool_result` `is_error` | ⚠️ | OpenAI has no `is_error` field; surfaced as a leading `"[tool_error] "` marker in the tool message content so the signal is not lost. |
| content: `thinking` in history | ⚠️ | **Dropped.** OpenAI has no assistant "thinking" slot; replaying prior chain-of-thought as content pollutes context. `redacted_thinking` also dropped. |
| `tools` | ✅ | `{name,description,input_schema}` → `{type:"function",function:{name,description,parameters}}`. |
| `tool_choice:{type:"auto"}` | ✅ | → `"auto"`. |
| `tool_choice:{type:"any"}` | ✅ | → `"required"`. |
| `tool_choice:{type:"tool",name}` | ✅ | → `{type:"function",function:{name}}`. |
| `tool_choice:{type:"none"}` | ✅ | → `"none"`. |
| `tool_choice` absent | ✅ | Field omitted. |

## Response — OpenAI Chat Completions → Anthropic Messages (non-streaming)

| Feature | Status | Notes |
|---|---|---|
| Envelope | ✅ | `{id,type:"message",role:"assistant",model:<inbound>,content,stop_reason,stop_sequence,usage}`. Missing `id` → `msg_rmll`. |
| `content` block order | ✅ | thinking → text → tool_use. |
| `reasoning_content` → thinking | ✅ | **Priority feature.** `{type:"thinking",thinking:<text>,signature:""}`, always first. Handles the real-world payload where `content` is empty. |
| text (`message.content`) | ✅ | → `{type:"text"}`. |
| `tool_calls` → tool_use | ✅ | `arguments` parsed into an object (`input`); invalid JSON → `{}`. |
| `message.refusal` | ⚠️ | Emitted as a text block; forces `stop_reason:"refusal"`. Anthropic has no dedicated refusal content block, so text is the faithful carrier. |
| n > 1 choices | ⚠️ | Only `choices[0]` used — Anthropic messages are single-response; extra choices discarded. |
| usage `prompt_tokens` | ✅ | → `input_tokens`. |
| usage `completion_tokens` | ✅ | → `output_tokens`. |
| `prompt_tokens_details.cached_tokens` | ✅ | → `cache_read_input_tokens` (omitted when 0/absent). |
| `completion_tokens_details.reasoning_tokens` | ❌ | No Anthropic field; dropped. The reasoning **text** is preserved as a thinking block. |

### `stop_reason` mapping (`MapStopReason`)

| OpenAI `finish_reason` | Anthropic `stop_reason` | Notes |
|---|---|---|
| `stop` | `end_turn` | Natural stop. |
| `stop` + matched stop | `stop_sequence` | `stop_sequence` set to the matched string when the worker reports it (choice-level `stop_reason` / `matched_stop`, e.g. vLLM / llama.cpp). |
| `length` | `max_tokens` | |
| `tool_calls` | `tool_use` | |
| `function_call` | `tool_use` | Legacy alias. |
| `content_filter` | `refusal` | Closest native stop_reason for filtered/declined output (documented choice; the alternative was `end_turn`). |
| other / empty | `end_turn` | Conservative default. |

## Streaming — OpenAI SSE → Anthropic SSE (`StreamOpenAIToAnthropic`)

| Feature | Status | Notes |
|---|---|---|
| `message_start` | ✅ | `input_tokens:0` (usage arrives on the final chunk); real value reported in `message_delta`. |
| `ping` | ✅ | One emitted after `message_start` (Anthropic-supported, optional). |
| thinking block from `delta.reasoning_content` | ✅ | **Priority feature.** `content_block_start{thinking}` + `content_block_delta{thinking_delta}`; ordered before text/tools. |
| text block from `delta.content` | ✅ | `content_block_delta{text_delta}`. |
| tool_use from `delta.tool_calls` | ✅ | `content_block_delta{input_json_delta,partial_json}`. One block per OpenAI tool index. |
| block index bookkeeping | ✅ | Strictly sequential: current block closed with `content_block_stop` before the next `content_block_start`. Order thinking → text → tool(s); parallel tools become distinct blocks by index. |
| `message_delta` | ✅ | Mapped `stop_reason` (+ `stop_sequence`) and `usage.output_tokens` (+ `input_tokens` when the worker supplies it). |
| `message_stop` | ✅ | |
| mid-stream error frame | ✅ | A `{"error":{...}}` chunk closes any open block, emits an Anthropic `error` event, and stops. |
| `[DONE]` / EOF | ✅ | Both terminate the stream cleanly. |

## Errors — `TranslateError(status, body)`

| Input | Status | Notes |
|---|---|---|
| OpenAI `{error:{message,type,code}}` | ✅ | Message preserved; `type` mapped when recognisable. |
| Non-JSON / plain body | ✅ | Body text used as message. |
| HTTP status → Anthropic error type | ✅ | 400→invalid_request_error, 401→authentication_error, 403→permission_error, 404→not_found_error, 429→rate_limit_error, 500→api_error, 503→overloaded_error, other→api_error. |

## Known limitations / out of scope

- **Reverse direction** (OpenAI-in → Anthropic-worker) — not implemented.
- **top_k** — dropped (no OpenAI standard).
- **Historical `thinking` replay** — dropped on the request side.
- **`reasoning_tokens`** accounting — no Anthropic field; text preserved instead.
- **Prompt caching semantics** — Anthropic `cache_control` is ignored on input;
  only `cached_tokens` read-back on output is surfaced.
- **Signatures** — thinking-block signatures are always empty strings; OpenAI
  workers do not produce verifiable signatures.

## Tests

`translate_test.go` is table-driven, one concern per test: request params + text
roundtrip, system string/array, tools + tool_use + tool_result (+ `is_error`), all
four `tool_choice` variants, base64/url images, dropped history thinking,
`reasoning_content` → thinking (non-stream), thinking→text→tool ordering, the full
`stop_reason` table, matched stop sequence, refusal, n>1 choices, error translation
(OpenAI body + status fallback), streaming text, streaming `reasoning_content` →
`thinking_delta`, interleaved thinking/text/parallel-tools index order, mid-stream
error, streaming stop sequence, and usage detail.

Run:

```
go test ./...
# On Go 1.21.0 + macOS Darwin 25.x a dyld LC_UUID abort can occur; if so:
go test -ldflags=-linkmode=external ./...
```
