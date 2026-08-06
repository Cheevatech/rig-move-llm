package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/internal/stats"
	"github.com/Cheevatech/rig-move-llm/pkg/translate"
)

// handleWorkerRoute is the worker leg. It answers an inbound
// Anthropic /v1/messages request by translating it to OpenAI Chat Completions,
// sending it to the worker endpoint, and translating the reply back to the
// Anthropic wire shape Claude Code expects. Nothing here reaches the paid
// Anthropic upstream: an UNMODIFIED CC session runs entirely on the worker, driving its
// own toolset. Usage is folded into the WORKER ledger (off the paid ledger).
//
// It supports both streaming (stream:true — the CC default) and non-streaming
// requests. There is no escalation to Claude in this prototype; that is the
// escalation path, built only once convergence is proven.
func (s *Server) handleWorkerRoute(w http.ResponseWriter, r *http.Request, cfg config.Config, project string, body []byte, inboundModel string) {
	start := time.Now()

	if cfg.WorkerAPIBase == "" {
		writeError(w, http.StatusBadGateway, "api_error", "RIG_ROUTE_ALL_TO_WORKER is set but WORKER_API_BASE is not configured")
		return
	}

	var areq translate.AnthropicRequest
	if err := json.Unmarshal(body, &areq); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "could not parse Anthropic request: "+err.Error())
		return
	}

	workerModel := cfg.WorkerModel
	if workerModel == "" {
		workerModel = "local"
	}
	oreq := translate.RequestAnthropicToOpenAI(areq, workerModel)

	buf, err := json.Marshal(oreq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "failed to encode worker request: "+err.Error())
		return
	}

	target := strings.TrimRight(cfg.WorkerAPIBase, "/") + "/chat/completions"
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(buf))
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "failed to build worker request: "+err.Error())
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	if cfg.WorkerAPIKey != "" {
		upReq.Header.Set("Authorization", "Bearer "+cfg.WorkerAPIKey)
	}
	for k, v := range cfg.Backend.ExtraHeaders {
		upReq.Header.Set(k, v)
	}

	resp, err := httpClient.Do(upReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "worker request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	// Worker error -> translate to an Anthropic error envelope so CC surfaces it.
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		status, aerr := translate.TranslateErrorStatus(resp.StatusCode, errBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(aerr)
		s.recordWorker(project, inboundModel, 0, 0, start, status, body)
		return
	}

	if areq.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		usage, _ := translate.StreamOpenAIToAnthropicUsage(w, resp.Body, inboundModel)
		s.recordWorker(project, inboundModel, usage.InputTokens, usage.OutputTokens, start, http.StatusOK, body)
		return
	}

	// Non-streaming: decode the full OpenAI response and re-shape to Anthropic.
	oBody, _ := io.ReadAll(resp.Body)
	var oResp translate.OpenAIResponse
	if err := json.Unmarshal(oBody, &oResp); err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "failed to decode worker response: "+err.Error())
		return
	}
	aResp := translate.ResponseOpenAIToAnthropic(oResp, inboundModel)
	out, _ := json.Marshal(aResp)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)

	in, outTok := 0, 0
	if oResp.Usage != nil {
		in, outTok = oResp.Usage.PromptTokens, oResp.Usage.CompletionTokens
	}
	s.recordWorker(project, inboundModel, in, outTok, start, http.StatusOK, body)
}

// recordWorker folds a routed inference into the WORKER ledger (off the paid one).
func (s *Server) recordWorker(project, model string, in, out int, start time.Time, status int, body []byte) {
	if s.rec == nil {
		return
	}
	s.rec.Record(stats.Record{
		Leg:       stats.LegWorker,
		Project:   project,
		Routed:    stats.RoutedWorker,
		Model:     model,
		InTokens:  in,
		OutTokens: out,
		Millis:    time.Since(start).Milliseconds(),
		Status:    status,
		ReqBody:   body,
	})
}

// countTokensEstimate answers a /v1/messages/count_tokens request when routing to
// the worker, which has no equivalent endpoint. Claude Code uses the count only to
// pace its context window, so a cheap character-based estimate (~4 chars/token
// over the serialized body) is adequate for the prototype and avoids a round-trip.
func countTokensEstimate(body []byte) []byte {
	n := len(body) / 4
	if n < 1 {
		n = 1
	}
	out, _ := json.Marshal(map[string]int{"input_tokens": n})
	return out
}
