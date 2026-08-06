package proxy

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/internal/stats"
)

// anthropicReply is a minimal non-stream Anthropic message with usage, enough for
// handleMain's scanner to fold into the ledger.
const anthropicReply = `{"type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":1}}`

// TestProjectPrefixRouting drives the /p/<id> path prefix end to end: a
// registered project's local config wins per request (and re-reads fresh on
// edit), an unregistered or malformed id fails closed, and the request log
// carries the project field while stats stay in the daemon's global scope.
// Per-project selection is observed on the MAIN leg — each project points its
// MAIN_UPSTREAM_URL at a different stub upstream.
func TestProjectPrefixRouting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var hitsA, hitsB atomic.Int64
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsA.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, anthropicReply)
	}))
	defer upstreamA.Close()
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsB.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, anthropicReply)
	}))
	defer upstreamB.Close()

	// A registered project whose local config points at upstream A.
	proj, err := config.CanonicalPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	localDir := filepath.Join(proj, config.DirName)
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatal(err)
	}
	localCfg := filepath.Join(localDir, config.ConfigFile)
	if err := os.WriteFile(localCfg, []byte("MAIN_UPSTREAM_URL="+upstreamA.URL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.RegisterProject(proj); err != nil {
		t.Fatal(err)
	}

	// The daemon boots with upstream B (its global-scope view).
	dataDir := t.TempDir()
	rec, err := stats.NewRecorder(dataDir, false)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: config.Config{MainUpstreamURL: upstreamB.URL, DataDir: dataDir}, rec: rec}
	h := s.Handler()

	body := `{"model":"claude-sonnet-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	id := config.EncodeProjectID(proj)

	// Prefixed request → the project's own upstream (A), not the boot config (B).
	if code := postPath(t, h, "/p/"+id+"/v1/messages", body); code != http.StatusOK {
		t.Fatalf("prefixed request status %d", code)
	}
	if hitsA.Load() != 1 || hitsB.Load() != 0 {
		t.Fatalf("prefixed request hit A=%d B=%d, want 1/0", hitsA.Load(), hitsB.Load())
	}

	// Unprefixed request → boot config (B), unchanged behavior.
	if code := postPath(t, h, "/v1/messages", body); code != http.StatusOK {
		t.Fatalf("unprefixed request status %d", code)
	}
	if hitsB.Load() != 1 {
		t.Fatalf("unprefixed request did not use boot config")
	}

	// Config edit is honored on the very next request (no cache).
	if err := os.WriteFile(localCfg, []byte("MAIN_UPSTREAM_URL="+upstreamB.URL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := postPath(t, h, "/p/"+id+"/v1/messages", body); code != http.StatusOK {
		t.Fatalf("post-edit request status %d", code)
	}
	if hitsB.Load() != 2 {
		t.Fatalf("config edit not picked up fresh (B hits = %d, want 2)", hitsB.Load())
	}

	// Unregistered project → 403 fail-closed; malformed id → 400.
	other, _ := config.CanonicalPath(t.TempDir())
	if code := postPath(t, h, "/p/"+config.EncodeProjectID(other)+"/v1/messages", body); code != http.StatusForbidden {
		t.Errorf("unregistered project status %d, want 403", code)
	}
	if code := postPath(t, h, "/p/!!!bad/v1/messages", body); code != http.StatusBadRequest {
		t.Errorf("malformed id status %d, want 400", code)
	}

	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	// The daemon-scope log carries the project on prefixed entries only.
	var projects []string
	f, err := os.Open(filepath.Join(dataDir, "logs", "requests.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var line struct {
			Project string `json:"project"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("bad log line: %v", err)
		}
		projects = append(projects, line.Project)
	}
	want := []string{proj, "", proj}
	if len(projects) != len(want) {
		t.Fatalf("log has %d entries, want %d", len(projects), len(want))
	}
	for i := range want {
		if projects[i] != want[i] {
			t.Errorf("log entry %d project = %q, want %q", i, projects[i], want[i])
		}
	}
}

func postPath(t *testing.T, h http.Handler, path, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	return rw.Code
}

// TestFlipReachesUnprefixedRequests is the regression test for a switch that
// reported success and did not switch. A request without /p/<id> used to be
// served from the config the daemon booted with, so `rig qwen on` — which writes
// the flag to disk — never reached any session that was not launched inside a
// REGISTERED project. Measured before the fix: flag on, plain request, still
// answered by the paid upstream.
func TestFlipReachesUnprefixedRequests(t *testing.T) {
	var workerHit, mainHit bool
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workerHit = true
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer worker.Close()
	main := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mainHit = true
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","type":"message","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer main.Close()

	// The daemon's scope is a temp HOME whose config.env is edited mid-flight,
	// exactly as `rig qwen on` edits it.
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, config.DirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, config.ConfigFile)
	write := func(flag string) {
		body := "ENABLED=true\nMAIN_UPSTREAM_URL=" + main.URL +
			"\nWORKER_API_BASE=" + worker.URL + "\nWORKER_MODEL=qwen\nRIG_ROUTE_ALL_TO_WORKER=" + flag + "\n"
		if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Boot with the flag OFF, so the boot config says "paid leg". The test's own
	// cwd carries no .rig-move-llm, so Load() resolves the temp HOME set above —
	// which is the layering a globally-installed daemon actually sees.
	write("false")
	rec, _ := stats.NewRecorder(t.TempDir(), false)
	h := (&Server{cfg: config.Load(), reload: config.Load, rec: rec}).Handler()

	body := `{"model":"claude-opus-4-8","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	workerHit, mainHit = false, false
	if code := postPath(t, h, "/v1/messages", body); code != http.StatusOK {
		t.Fatalf("flag off: status %d", code)
	}
	if !mainHit || workerHit {
		t.Fatalf("flag off: mainHit=%v workerHit=%v, want main only", mainHit, workerHit)
	}

	// Flip on disk while the server keeps running — no restart, as `qwen on` promises.
	write("true")
	workerHit, mainHit = false, false
	if code := postPath(t, h, "/v1/messages", body); code != http.StatusOK {
		t.Fatalf("flag on: status %d", code)
	}
	if !workerHit || mainHit {
		t.Errorf("flag on: workerHit=%v mainHit=%v — the flip did not reach an unprefixed request", workerHit, mainHit)
	}
}
