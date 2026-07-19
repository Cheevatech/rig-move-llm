// Package proxy is the rig-move-llm observability layer. It sits at
// ANTHROPIC_BASE_URL and forwards Claude Code's traffic verbatim to the paid
// Anthropic upstream (OAuth / anthropic-beta headers preserved, streamed
// unbuffered), tee-scanning completions for token usage so the ledger can report
// what the paid (MAIN) leg spent.
//
// It is MAIN-leg only. Offload to a worker model is NOT done here: on CC 2.1.x
// native subagents run in-process and never egress to a base-URL proxy, so the
// old worker leg (haiku-model interception -> OpenAI translation -> local worker)
// could never see real subagent traffic and was removed in ticket P10-B. Offload
// now runs out-of-process through the worker MCP tool (mcp__worker__implement,
// internal/worker), whose token cost is off the paid ledger by construction.
//
// Per-project routing (the /p/<id> path prefix, allowlist-gated) still applies:
// it selects which project's config/upstream a request uses.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/internal/stats"
)

// httpClient is shared; no timeout so long streaming responses are not cut off.
var httpClient = &http.Client{}

// Server holds the resolved configuration and serves the routing handler.
type Server struct {
	cfg     config.Config
	rec     *stats.Recorder // observability recorder; nil disables recording
	httpSrv *http.Server
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
	log.Printf("rig-move-llm listening on %s | main=%s backend=%s",
		addr, s.cfg.MainUpstreamURL, s.cfg.Backend.Name)
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

// handle forwards every request to the paid Anthropic upstream. POST /v1/messages
// is tee-scanned for token usage and folded into the ledger; other paths
// (count_tokens, GET, etc.) are non-billable passthrough and skip the scanner.
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	cfg, project, ok := s.resolveProject(w, r)
	if !ok {
		return
	}

	if r.Method == http.MethodPost && r.URL.Path == "/v1/messages" {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "could not read request body")
			return
		}
		// Peek at the model field for the log/ledger without committing to a struct.
		var peek struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &peek)
		logReq(r.Method, r.URL.Path, peek.Model, "MAIN")
		s.handleMain(w, r, cfg, project, body, peek.Model, true)
		return
	}

	logReq(r.Method, r.URL.Path, "", "MAIN")
	body, _ := io.ReadAll(r.Body)
	s.handleMain(w, r, cfg, project, body, "", false)
}

// handleMain performs a verbatim byte passthrough to MAIN_UPSTREAM_URL,
// preserving all auth headers and streaming the response unbuffered. When record
// is true the upstream response is tee-scanned for Anthropic token usage and
// folded into the billed (MAIN) ledger — without buffering the stream.
func (s *Server) handleMain(w http.ResponseWriter, r *http.Request, cfg config.Config, project string, body []byte, model string, record bool) {
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
			Routed:    stats.RoutedMain,
			Model:     model,
			InTokens:  in,
			OutTokens: out,
			Millis:    time.Since(start).Milliseconds(),
			Status:    resp.StatusCode,
			ReqBody:   body,
		})
	}
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
