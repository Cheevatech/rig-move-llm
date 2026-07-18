// Package proxy is the rig-move-llm routing core. It sits at ANTHROPIC_BASE_URL
// and splits Claude Code's traffic in two legs:
//
//   - worker leg  (inbound model matches "haiku"): Anthropic Messages ->
//     OpenAI Chat translation -> the user's worker endpoint -> translated back.
//   - main leg    (everything else): verbatim byte passthrough to the paid
//     Anthropic upstream, preserving OAuth / anthropic-beta headers and streaming.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/internal/stats"
	"github.com/Cheevatech/rig-move-llm/pkg/translate"
)

var haikuRe = regexp.MustCompile(`(?i)haiku`)

// budgetWindow is the sliding window over which the L4 %-budget alternation
// measures the custom worker's token share (var so tests can widen/shrink it).
var budgetWindow = 15 * time.Minute

// budgetEndpoint labels log entries diverted by the %-budget, so they are
// distinguishable from L2 passthrough-fallback diverts in requests.jsonl.
const budgetEndpoint = "budget"

// httpClient is shared; no timeout so long streaming responses are not cut off.
var httpClient = &http.Client{}

// fallbackClient is used for worker attempts when a fallback chain exists: it
// bounds the time-to-first-byte so a dead endpoint costs seconds, not a hung
// session, before the next endpoint is tried. Single-endpoint configs keep the
// timeout-free httpClient — pre-L2 behavior unchanged.
var fallbackClient = func() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{Transport: tr}
}()

// healthCooldown is how long a failed worker endpoint is skipped before being
// retried (var so tests can shrink it). Endpoints in cooldown are still tried
// as a last resort when every endpoint in the chain is cooling down.
var healthCooldown = 30 * time.Second

// Server holds the resolved configuration and serves the routing handler.
type Server struct {
	cfg     config.Config
	rec     *stats.Recorder // observability recorder; nil disables recording
	httpSrv *http.Server

	healthMu  sync.Mutex
	unhealthy map[string]time.Time // endpoint label -> when it last failed
}

// New builds a Server from resolved configuration. It opens the observability
// recorder; if that fails the server still runs, just without recording.
func New(cfg config.Config) *Server {
	s := &Server{cfg: cfg}
	if rec, err := stats.NewRecorder(cfg.DataDir, cfg.LogBodies); err == nil {
		rec.SetMaxLogBytes(int64(cfg.LogMaxMB) << 20)
		s.rec = rec
	} else {
		log.Printf("stats: recording disabled: %v", err)
	}
	return s
}

// Handler returns the HTTP handler (single mux entry — routing is body-driven).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	return mux
}

// ListenAndServe binds the configured port and serves until error. It starts the
// recorder's periodic flush so counters survive an unclean exit between flushes.
func (s *Server) ListenAndServe() error {
	s.rec.StartFlusher(5 * time.Second)
	addr := ":" + s.cfg.Port
	log.Printf("rig-move-llm listening on %s | main=%s worker=%s backend=%s",
		addr, s.cfg.MainUpstreamURL, s.cfg.WorkerAPIBase, s.cfg.Backend.Name)
	s.httpSrv = &http.Server{Addr: addr, Handler: s.Handler()}
	return s.httpSrv.ListenAndServe()
}

// Shutdown gracefully stops the HTTP server and flushes+closes the recorder so
// the ledger and log are durable. Wired to SIGTERM by the serve command.
func (s *Server) Shutdown(ctx context.Context) error {
	var err error
	if s.httpSrv != nil {
		err = s.httpSrv.Shutdown(ctx)
	}
	if cerr := s.rec.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}

func logReq(method, path, model, leg string) {
	log.Printf("%s %s %s model=%q leg=%s", time.Now().UTC().Format(time.RFC3339), method, path, model, leg)
}

// projectPrefix is the base URL path prefix carrying a per-project identity:
// /p/<base64url(canonical project dir)>/... — embedded by `run`, stripped here.
const projectPrefix = "/p/"

// resolveProject strips a /p/<id> prefix from the request, validates the decoded
// project dir against the fail-closed allowlist, and loads that project's config
// fresh (no cache). It returns ok=false after writing the error response itself.
// Without a prefix, the daemon's boot config applies unchanged.
func (s *Server) resolveProject(w http.ResponseWriter, r *http.Request) (cfg config.Config, project string, ok bool) {
	if !strings.HasPrefix(r.URL.Path, projectPrefix) {
		return s.cfg, "", true
	}
	id, tail, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, projectPrefix), "/")

	dir, err := config.DecodeProjectID(id)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "malformed project id in URL path")
		return config.Config{}, "", false
	}
	// The allowlist gates everything: canonicalize first, deny on any failure,
	// and only then read the project's config from disk (no path oracle).
	canon, err := config.CanonicalPath(dir)
	if err != nil || !config.ProjectAllowed(canon) {
		writeError(w, http.StatusForbidden, "permission_error",
			"project is not registered with rig-move-llm; run 'rig-move-llm init' in "+dir)
		return config.Config{}, "", false
	}

	cfg = config.LoadFrom(canon)
	// The daemon owns the listener and the recorder: stats stay at the global
	// scope regardless of what the project's local layer says.
	cfg.Port, cfg.DataDir = s.cfg.Port, s.cfg.DataDir
	r.URL.Path = "/" + tail
	return cfg, canon, true
}

// handle routes the request between the worker leg (OpenAI translation) and the
// main leg (raw Anthropic passthrough), based on the JSON body's "model".
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	cfg, project, ok := s.resolveProject(w, r)
	if !ok {
		return
	}

	// Only POST /v1/messages (optionally with ?beta=) is eligible for the worker
	// leg. Everything else (count_tokens, other paths, GET, etc.) is passthrough.
	isMessages := r.Method == http.MethodPost && r.URL.Path == "/v1/messages"

	if isMessages {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "could not read request body")
			return
		}
		// Peek at the model field without fully committing to a struct.
		var peek struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &peek)

		if haikuRe.MatchString(peek.Model) {
			logReq(r.Method, r.URL.Path, peek.Model, "WORKER")
			s.handleWorker(w, r, cfg, project, body, peek.Model)
			return
		}
		// Non-haiku: passthrough, but we already consumed the body.
		logReq(r.Method, r.URL.Path, peek.Model, "MAIN")
		s.handleMain(w, r, cfg, project, body, peek.Model, true, stats.RoutedMain, "")
		return
	}

	logReq(r.Method, r.URL.Path, "", "MAIN")
	body, _ := io.ReadAll(r.Body)
	// Non-message passthrough (count_tokens, GET, etc.) is not a billable
	// completion; stream it but do not fold it into the token ledger.
	s.handleMain(w, r, cfg, project, body, "", false, stats.RoutedMain, "")
}

// handleMain performs a verbatim byte passthrough to MAIN_UPSTREAM_URL,
// preserving all auth headers and streaming the response unbuffered. When record
// is true the upstream response is tee-scanned for Anthropic token usage and
// folded into the billed (MAIN) ledger — without buffering the stream. routed
// and endpoint annotate the log entry: a worker-tier request diverted here by
// the L2 passthrough fallback carries RoutedDiverted so it never masquerades as
// a genuine main-agent call in the accounting.
func (s *Server) handleMain(w http.ResponseWriter, r *http.Request, cfg config.Config, project string, body []byte, model string, record bool, routed, endpoint string) {
	start := time.Now()
	if cfg.MainUpstreamURL == "" {
		writeError(w, http.StatusBadGateway, "api_error", "MAIN_UPSTREAM_URL is not configured")
		return
	}

	target := cfg.MainUpstreamURL + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "failed to build upstream request: "+err.Error())
		return
	}

	// Copy request headers verbatim (authorization, x-api-key, anthropic-*, etc.).
	for k, vv := range r.Header {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vv {
			upReq.Header.Add(k, v)
		}
	}
	// When we intend to scan the response for usage, the upstream must not reply
	// with the client's negotiated encoding (gzip/br/zstd would reach the scanner
	// compressed). Dropping the header lets the transport negotiate gzip itself
	// and transparently decompress, so the scanner and the client both see
	// identity bytes. record=false passthrough stays header-verbatim.
	if record {
		upReq.Header.Del("Accept-Encoding")
	}
	// Only rewrite Host to match the upstream.
	if u, err := url.Parse(cfg.MainUpstreamURL); err == nil {
		upReq.Host = u.Host
		upReq.Header.Set("Host", u.Host)
	}
	upReq.ContentLength = int64(len(body))

	resp, err := httpClient.Do(upReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "main upstream request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	// Copy response headers verbatim.
	for k, vv := range resp.Header {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	var scan *mainUsageScanner
	if record {
		scan = newMainUsageScanner(resp.Header.Get("Content-Type"))
	}

	// Stream unbuffered, flushing per chunk (important for SSE). When recording,
	// each chunk is also fed to the usage scanner before being written out.
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if scan != nil {
				scan.feed(buf[:n])
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}

	if scan != nil {
		in, out := scan.close()
		s.rec.Record(stats.Record{
			Leg:       stats.LegMain,
			Project:   project,
			Endpoint:  endpoint,
			Routed:    routed,
			Model:     model,
			InTokens:  in,
			OutTokens: out,
			Millis:    time.Since(start).Milliseconds(),
			Status:    resp.StatusCode,
			ReqBody:   body,
		})
	}
}

// workerChain returns the fallback chain for this request: the workers.json
// chain when present, otherwise a single endpoint synthesized from the scalar
// WORKER_* config (pre-L2 behavior, byte-identical).
func workerChain(cfg config.Config) []config.WorkerEndpoint {
	if len(cfg.Workers) > 0 {
		return cfg.Workers
	}
	if cfg.WorkerAPIBase == "" {
		return nil
	}
	return []config.WorkerEndpoint{{
		Base:    cfg.WorkerAPIBase,
		Key:     cfg.WorkerAPIKey,
		Model:   cfg.WorkerModel,
		Backend: cfg.Backend,
	}}
}

// healthOrder reorders the chain so endpoints in failure cooldown sort after
// healthy ones (each group keeps its priority order). Cooling endpoints are
// kept, not dropped: if every endpoint is down they are still the best bet.
func (s *Server) healthOrder(chain []config.WorkerEndpoint) []config.WorkerEndpoint {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	now := time.Now()
	healthy := make([]config.WorkerEndpoint, 0, len(chain))
	var cooling []config.WorkerEndpoint
	for _, ep := range chain {
		if t, ok := s.unhealthy[ep.Label()]; ok && now.Sub(t) < healthCooldown {
			cooling = append(cooling, ep)
		} else {
			healthy = append(healthy, ep)
		}
	}
	return append(healthy, cooling...)
}

func (s *Server) markUnhealthy(label string) {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if s.unhealthy == nil {
		s.unhealthy = map[string]time.Time{}
	}
	s.unhealthy[label] = time.Now()
}

func (s *Server) markHealthy(label string) {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	delete(s.unhealthy, label)
}

// retryableStatus reports whether a worker HTTP status is an availability
// problem worth falling back on (as opposed to a request problem the next
// endpoint would reject identically).
func retryableStatus(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500
}

// handleWorker walks the health-ordered fallback chain: each endpoint is tried
// in turn until one serves the request, a passthrough entry diverts it to the
// paid main upstream, or the chain is exhausted. With a single endpoint the
// behavior (client, timeouts, errors) is exactly the pre-L2 worker leg.
func (s *Server) handleWorker(w http.ResponseWriter, r *http.Request, cfg config.Config, project string, body []byte, inboundModel string) {
	chain := workerChain(cfg)
	if len(chain) == 0 {
		writeError(w, http.StatusBadGateway, "api_error", "WORKER_API_BASE is not configured")
		return
	}

	var areq translate.AnthropicRequest
	if err := json.Unmarshal(body, &areq); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "could not parse Anthropic request: "+err.Error())
		return
	}

	// L4 %-budget alternation (opt-in via CUSTOM_SUBAGENT_USAGE 1-99): keep the
	// custom worker's share of worker-tier tokens over the sliding window near
	// the target by diverting to the paid upstream whenever the share is at or
	// above it. An empty window and a disabled recorder both route custom
	// (fail-cheap: missing data must not burn quota). The unset/100 default
	// skips this block entirely — pre-L4 byte-identical.
	if n := cfg.CustomSubagentUsage; n >= 1 && n <= 99 && s.rec != nil {
		workerTok, divertedTok := s.rec.WindowTokens(budgetWindow)
		if total := workerTok + divertedTok; total > 0 && workerTok*100 >= int64(n)*total {
			log.Printf("worker budget: custom share %d%% >= target %d%%, diverting %q to main upstream",
				workerTok*100/total, n, inboundModel)
			s.handleMain(w, r, cfg, project, body, inboundModel, true, stats.RoutedDiverted, budgetEndpoint)
			return
		}
	}

	hasFallback := len(chain) > 1
	var lastStatus int
	var lastBody []byte
	var lastErr error
	for _, ep := range s.healthOrder(chain) {
		if ep.Passthrough {
			// Honest quota burn while workers are down: billed on MAIN but tagged
			// diverted so savings math can tell it apart from real main traffic.
			log.Printf("worker chain: diverting %q to main upstream (endpoint %s)", inboundModel, ep.Label())
			s.handleMain(w, r, cfg, project, body, inboundModel, true, stats.RoutedDiverted, ep.Label())
			return
		}
		handled, status, errBody, err := s.tryWorker(w, r, cfg, project, ep, areq, body, inboundModel, hasFallback)
		if handled {
			return
		}
		lastStatus, lastBody, lastErr = status, errBody, err
		s.markUnhealthy(ep.Label())
		if err != nil {
			log.Printf("worker chain: endpoint %s failed (%v), falling back", ep.Label(), err)
		} else {
			log.Printf("worker chain: endpoint %s returned %d, falling back", ep.Label(), status)
		}
	}

	// Chain exhausted: surface the last failure in Anthropic error shape.
	if lastErr != nil {
		writeError(w, http.StatusBadGateway, "api_error", "all worker endpoints failed; last error: "+lastErr.Error())
		return
	}
	aerr := translate.TranslateError(lastStatus, lastBody)
	out, _ := json.Marshal(aerr)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = w.Write(out)
}

// tryWorker attempts one endpoint. handled=true means a response was written to
// the client (success, or a failure not worth retrying elsewhere). Otherwise
// nothing was written and status/errBody/err describe the retryable failure.
func (s *Server) tryWorker(w http.ResponseWriter, r *http.Request, cfg config.Config, project string, ep config.WorkerEndpoint, areq translate.AnthropicRequest, body []byte, inboundModel string, hasFallback bool) (handled bool, status int, errBody []byte, err error) {
	start := time.Now()

	model := ep.Model
	if model == "" {
		model = cfg.WorkerModel
	}
	oreq := translate.RequestAnthropicToOpenAI(areq, model)
	// Backend quirk: some servers reject stream_options.
	if ep.Backend.DropStreamOptions {
		oreq.StreamOptions = nil
	}
	oBody, err := json.Marshal(oreq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to encode worker request: "+err.Error())
		return true, 0, nil, nil
	}

	target := ep.Base + "/chat/completions"
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(oBody))
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "failed to build worker request: "+err.Error())
		return true, 0, nil, nil
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Accept", "application/json")
	if ep.Key != "" {
		upReq.Header.Set("Authorization", "Bearer "+ep.Key)
	}
	for k, v := range ep.Backend.ExtraHeaders {
		upReq.Header.Set(k, v)
	}

	// With a fallback available, bound time-to-first-byte so a dead endpoint
	// moves on in seconds; single-endpoint keeps the timeout-free client.
	client := httpClient
	if hasFallback {
		client = fallbackClient
	}
	resp, err := client.Do(upReq)
	if err != nil {
		if hasFallback {
			return false, 0, nil, err
		}
		writeError(w, http.StatusBadGateway, "api_error", "worker upstream request failed: "+err.Error())
		return true, 0, nil, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		eb, _ := io.ReadAll(resp.Body)
		if hasFallback && retryableStatus(resp.StatusCode) {
			return false, resp.StatusCode, eb, nil
		}
		aerr := translate.TranslateError(resp.StatusCode, eb)
		out, _ := json.Marshal(aerr)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write(out)
		return true, 0, nil, nil
	}

	s.markHealthy(ep.Label())

	if areq.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		usage, _ := translate.StreamOpenAIToAnthropicUsage(w, resp.Body, inboundModel)
		s.rec.Record(stats.Record{
			Leg:       stats.LegWorker,
			Project:   project,
			Endpoint:  ep.Label(),
			Routed:    stats.RoutedWorker,
			Model:     inboundModel,
			InTokens:  usage.InputTokens,
			OutTokens: usage.OutputTokens,
			Millis:    time.Since(start).Milliseconds(),
			Status:    http.StatusOK,
			ReqBody:   body,
		})
		return true, 0, nil, nil
	}

	// Non-streaming.
	oRespBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "failed to read worker response: "+err.Error())
		return true, 0, nil, nil
	}
	var oResp translate.OpenAIResponse
	if err := json.Unmarshal(oRespBody, &oResp); err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "failed to parse worker response: "+err.Error())
		return true, 0, nil, nil
	}
	aResp := translate.ResponseOpenAIToAnthropic(oResp, inboundModel)
	out, _ := json.Marshal(aResp)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	s.rec.Record(stats.Record{
		Leg:       stats.LegWorker,
		Project:   project,
		Endpoint:  ep.Label(),
		Routed:    stats.RoutedWorker,
		Model:     inboundModel,
		InTokens:  aResp.Usage.InputTokens,
		OutTokens: aResp.Usage.OutputTokens,
		Millis:    time.Since(start).Milliseconds(),
		Status:    http.StatusOK,
		ReqBody:   body,
		RespBody:  out,
	})
	return true, 0, nil, nil
}

// writeError emits an Anthropic-shaped error envelope.
func writeError(w http.ResponseWriter, status int, etype, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	env := map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    etype,
			"message": msg,
		},
	}
	_ = json.NewEncoder(w).Encode(env)
}

var hopByHop = map[string]bool{
	"connection":          true,
	"proxy-connection":    true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

func isHopByHop(k string) bool {
	return hopByHop[strings.ToLower(k)]
}
