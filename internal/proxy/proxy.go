// Package proxy is the switch. It sits at ANTHROPIC_BASE_URL and decides, per
// request, which model answers: the paid Anthropic upstream (forwarded verbatim,
// OAuth / anthropic-beta headers preserved, streamed unbuffered, tee-scanned for
// token usage) or the user's own worker endpoint (translated Anthropic <-> OpenAI,
// folded into a separate ledger and never billed to the paid account).
//
// Claude Code is never told the difference, which is the whole point: the flip
// lands mid-session, in the same conversation, with the context intact. Config is
// re-read from disk on every request (no cache), so `rig qwen on` takes effect on
// the very next turn of a session that is already running.
//
// Two path prefixes shape a request before it is routed, and they compose in this
// order: /r/<leg> forces one leg for this request alone, and /p/<id> selects which
// registered project's config applies (allowlist-gated, fail-closed).
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
	cfg config.Config
	// reload re-reads the daemon's own scope from disk for requests that carry no
	// /p/<id>. A nil reload means "cfg is the whole truth", which is what a test
	// injecting a fixture wants — without that escape hatch every such test would
	// silently pick up the developer's real config.env and egress to the real
	// upstream. New() wires the production implementation.
	reload  func() config.Config
	rec     *stats.Recorder // observability recorder; nil disables recording
	httpSrv *http.Server
}

// New builds a Server from resolved configuration. It opens the observability
// recorder; if that fails the server still runs, just without recording.
func New(cfg config.Config) *Server {
	s := &Server{cfg: cfg, reload: config.Load}
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
// Without a prefix the daemon's OWN scope is re-read, equally fresh — see below
// for why that is not the same as reusing the config it booted with.
func (s *Server) resolveProject(w http.ResponseWriter, r *http.Request) (cfg config.Config, project string, ok bool) {
	if !strings.HasPrefix(r.URL.Path, projectPrefix) {
		// Re-read the daemon's own scope, same as the /p/<id> path below. Returning
		// the boot config here meant `rig qwen on` silently did nothing for any
		// session not launched inside a REGISTERED project: the flag reached the
		// file, the daemon never looked at the file again, and the flip reported
		// success while every turn kept going to the paid leg. Measured before the
		// fix: flag on, plain request, still answered by the main upstream.
		//
		// The listener and the ledger belong to the process, so those two survive
		// the reload — everything else is whatever is on disk right now.
		if s.reload == nil {
			return s.cfg, "", true
		}
		fresh := s.reload()
		fresh.Port, fresh.DataDir = s.cfg.Port, s.cfg.DataDir
		return fresh, "", true
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

// routePrefix carries a per-invocation leg override in the base URL path:
// /r/worker/... forces the qwen (worker) leg; /r/main/... forces the verbatim
// Anthropic (paid) leg. One shared daemon therefore serves both legs at once,
// without a restart and without flipping the global flag — which is what lets a
// measurement run both arms against the same process. It is stripped before
// /p/<id>. ENABLED=false overrides it: see handle.
const routePrefix = "/r/"

// stripRoutePrefix removes a leading /r/<leg>/ segment from the request path and
// returns the leg ("worker" | "main"), or "" when absent. Unknown legs are ignored
// (treated as absent) so a stray path can never silently mis-route.
func stripRoutePrefix(r *http.Request) string {
	if !strings.HasPrefix(r.URL.Path, routePrefix) {
		return ""
	}
	leg, tail, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, routePrefix), "/")
	if leg != "worker" && leg != "main" {
		return ""
	}
	r.URL.Path = "/" + tail
	return leg
}

// handle routes each request. POST /v1/messages goes to the worker (qwen) leg or
// the paid Anthropic leg per the effective routing decision; the worker leg is
// translated + folded into the WORKER ledger, the main leg is a tee-scanned
// verbatim passthrough. Other paths (count_tokens, GET, etc.) are non-billable.
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	// Per-invocation leg override is read (and stripped) before project
	// resolution, which owns the /p/<id> segment that may follow it.
	leg := stripRoutePrefix(r)

	cfg, project, ok := s.resolveProject(w, r)
	if !ok {
		return
	}

	// Effective routing. ENABLED is the master switch and it is checked FIRST: with
	// it false NOTHING reaches the worker, not even an explicit /r/worker, because
	// `rig disable` prints "Claude Code runs normally" and that has to be true. Below
	// it, an explicit /r/<leg> wins over the RouteAllToWorker flag — /r/main always
	// reaches Claude even while the flag is on, so a request aimed at the paid leg is
	// never trapped on the worker leg.
	routeWorker := cfg.Enabled && (leg == "worker" || (leg == "" && cfg.RouteAllToWorker))

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
		if routeWorker {
			logReq(r.Method, r.URL.Path, peek.Model, "WORKER")
			s.handleWorkerRoute(w, r, cfg, project, body, peek.Model)
			return
		}
		logReq(r.Method, r.URL.Path, peek.Model, "MAIN")
		s.handleMain(w, r, cfg, project, body, peek.Model, true)
		return
	}

	// count_tokens has no worker equivalent; answer with a local estimate when
	// routing to qwen so CC's context pacing still works without an upstream call.
	if routeWorker && r.Method == http.MethodPost && r.URL.Path == "/v1/messages/count_tokens" {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(countTokensEstimate(body))
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
		in, cacheRead, cacheWrite, out := scan.close()
		s.rec.Record(stats.Record{
			Leg:       stats.LegMain,
			Project:   project,
			Routed:    stats.RoutedMain,
			Model:     model,
			InTokens:  in,
			CacheRead: cacheRead,
			CacheWrit: cacheWrite,
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
