package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/config"
)

// The rung that #35 is about. The failure it exists to catch is NOT "no
// toolchain" — it is "toolchain installed, invisible to the worker", which is what
// burned task12: go lived at ~/go/bin/go, the worker inherited a PATH without it,
// ran a stray pytest instead, and shipped 1157 lines that had never compiled.
func TestWorkerEndpointRung(t *testing.T) {
	t.Run("a real completion passes", func(t *testing.T) {
		var hit string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hit = r.URL.Path
			w.Write([]byte(`{"choices":[]}`))
		}))
		defer srv.Close()

		r := checkWorkerEndpoint(config.Config{WorkerAPIBase: srv.URL + "/v1", WorkerModel: "m"})

		if r.status != rungPass {
			t.Fatalf("status = %s, want PASS (%s)", r.status.label(), r.detail)
		}
		if hit != "/v1/chat/completions" {
			t.Errorf("probed %q, want the completions endpoint — /v1/models is what hid the bug", hit)
		}
	})

	t.Run("401 is reported as AUTH, distinctly", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}))
		defer srv.Close()

		r := checkWorkerEndpoint(config.Config{WorkerAPIBase: srv.URL + "/v1", WorkerModel: "m", WorkerAPIKey: "mangled"})

		if r.status != rungFail {
			t.Fatalf("status = %s, want FAIL", r.status.label())
		}
		if !strings.Contains(r.detail, "AUTH") {
			t.Errorf("a 401 must be labelled AUTH, not merely a failure:\n%s", r.detail)
		}
		if !strings.Contains(r.fix, "WORKER_API_KEY") {
			t.Errorf("fix must point at the key:\n%s", r.fix)
		}
	})

	t.Run("unreachable is a different diagnosis from a refusal", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		url := srv.URL
		srv.Close() // nothing listening now

		r := checkWorkerEndpoint(config.Config{WorkerAPIBase: url + "/v1", WorkerModel: "m"})

		if r.status != rungFail || !strings.Contains(r.detail, "unreachable") {
			t.Errorf("want an unreachable diagnosis, got %s / %s", r.status.label(), r.detail)
		}
	})
}

func TestConfigRung(t *testing.T) {
	if r := checkConfig(config.Config{}); r.status != rungFail || !strings.Contains(r.fix, "setup") {
		t.Errorf("no worker endpoint must FAIL with a setup fix, got %s / %s", r.status.label(), r.fix)
	}
	// ENABLED=false is the #22 shape: everything wired, nothing delegated.
	r := checkConfig(config.Config{WorkerAPIBase: "http://x/v1", Enabled: false})
	if r.status != rungFail || !strings.Contains(r.detail, "nothing is wired up") {
		t.Errorf("ENABLED=false must FAIL and say why, got %s / %s", r.status.label(), r.detail)
	}
	if r := checkConfig(config.Config{WorkerAPIBase: "http://x/v1", WorkerModel: "m", Enabled: true}); r.status != rungPass {
		t.Errorf("a configured, enabled rig must PASS, got %s / %s", r.status.label(), r.detail)
	}
}

// The switch's own refusal rule (never bill the worker leg to the paid account)
// has to be visible as a rung, not discovered when a run dies. It is
// unconditional now: there is no second engine to skip in favour of, and a SKIP
// here would be a lie about the only thing the product does.
func TestSwitchRung(t *testing.T) {
	// The rung asks the one question a session depends on: does THIS install's proxy
	// answer an Anthropic-format request on its worker leg? The old rung probed
	// RIG_CC_BASE_URL, which only ever mattered while the worker was a subprocess.
	t.Run("no proxy listening fails", func(t *testing.T) {
		r := checkSwitch(config.Config{Port: "1", Enabled: true})
		if r.status != rungFail || !strings.Contains(r.detail, "no proxy listening") {
			t.Errorf("want a listener diagnosis, got %s / %s", r.status.label(), r.detail)
		}
	})

	t.Run("the worker leg answering Anthropic-format passes", func(t *testing.T) {
		var hit string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hit = r.URL.Path
			w.Write([]byte(`{"content":[]}`))
		}))
		defer srv.Close()

		port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
		r := checkSwitch(config.Config{Port: port, Enabled: true})

		if r.status != rungPass {
			t.Fatalf("status = %s, want PASS (%s)", r.status.label(), r.detail)
		}
		if hit != "/r/worker/v1/messages" {
			t.Errorf("probed %q, want the proxy's worker leg", hit)
		}
	})
}

// The all-PASS install that could not delegate: the worker leg runs on a dummy
// key against the local endpoint, so no other rung ever touches the paid
// identity, and a HOME with no login looked perfectly healthy.
func TestMainAuthRung(t *testing.T) {
	// setHome isolates the rung from the developer's real login.
	setHome := func(t *testing.T) string {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("ANTHROPIC_API_KEY", "")
		return home
	}

	t.Run("no credentials fails and names the one step a human must do", func(t *testing.T) {
		home := setHome(t)

		r := checkMainAuth()

		if r.status != rungFail {
			t.Fatalf("status = %s, want FAIL (%s)", r.status.label(), r.detail)
		}
		if !strings.Contains(r.detail, home) {
			t.Errorf("detail must name the HOME it inspected:\n%s", r.detail)
		}
		if !strings.Contains(r.fix, "HOME="+home) || !strings.Contains(r.fix, "log in") {
			t.Errorf("fix must be the runnable login step:\n%s", r.fix)
		}
	})

	// The dogfood shape: .claude.json exists (it carries the workspace trust the
	// trust rung reads) but records no account.
	t.Run("a config with only project entries is not a login", func(t *testing.T) {
		home := setHome(t)
		mustWrite(t, filepath.Join(home, ".claude.json"), `{"projects":{"/repo":{}}}`)

		if r := checkMainAuth(); r.status != rungFail {
			t.Errorf("status = %s, want FAIL — workspace trust is not credentials", r.status.label())
		}
	})

	t.Run("a recorded oauth account passes", func(t *testing.T) {
		home := setHome(t)
		mustWrite(t, filepath.Join(home, ".claude.json"), `{"projects":{},"oauthAccount":{"accountUuid":"u"}}`)

		r := checkMainAuth()

		if r.status != rungPass {
			t.Fatalf("status = %s, want PASS (%s)", r.status.label(), r.detail)
		}
		// The rung must not overclaim: it never spent a request, so it cannot know
		// the token still works.
		if !strings.Contains(r.detail, "freshness not checked") {
			t.Errorf("a presence check must say what it did NOT prove:\n%s", r.detail)
		}
	})

	t.Run("a credentials file passes", func(t *testing.T) {
		home := setHome(t)
		if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(home, ".claude", ".credentials.json"), `{"claudeAiOauth":{}}`)

		if r := checkMainAuth(); r.status != rungPass {
			t.Errorf("status = %s, want PASS (%s)", r.status.label(), r.detail)
		}
	})

	t.Run("an API key is a valid MAIN identity and is reported as such", func(t *testing.T) {
		setHome(t)
		t.Setenv("ANTHROPIC_API_KEY", "sk-test")

		r := checkMainAuth()

		if r.status != rungPass || !strings.Contains(r.detail, "bills the API") {
			t.Errorf("want PASS naming the billing mode, got %s / %s", r.status.label(), r.detail)
		}
	})
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// JSON output: renderDoctor must produce valid, structured JSON when jsonMode
// is true, and the existing human-readable ladder when it is false.
func TestJSONRender(t *testing.T) {
	t.Run("all pass — ok true, no fixes, valid JSON", func(t *testing.T) {
		rungs := []rung{
			{name: "config", status: rungPass, detail: "enabled"},
			{name: "guards", status: rungPass, detail: "stall < wall"},
			{name: "cc engine", status: rungSkip, detail: "not configured"},
		}
		var buf strings.Builder
		failed := renderDoctor(&buf, rungs, true)

		if failed != 0 {
			t.Fatalf("want 0 failed, got %d", failed)
		}

		var result doctorResult
		if err := json.Unmarshal([]byte(buf.String()), &result); err != nil {
			t.Fatalf("JSON unmarshal failed: %v", err)
		}
		if !result.Ok {
			t.Error("want ok=true")
		}
		if len(result.Rungs) != 3 {
			t.Errorf("want 3 rungs, got %d", len(result.Rungs))
		}
		for _, r := range result.Rungs {
			if r.Name == "" {
				t.Error("name should be non-empty")
			}
			if r.Status != "pass" && r.Status != "fail" && r.Status != "skip" {
				t.Errorf("invalid status: %s", r.Status)
			}
			if r.Fix != "" {
				t.Errorf("pass/skip rungs should not have fix: %q", r.Fix)
			}
		}

		trimmed := strings.TrimSpace(buf.String())
		if !strings.HasPrefix(trimmed, "{") {
			t.Error("should start with {")
		}
		if !strings.HasSuffix(trimmed, "}") {
			t.Error("should end with }")
		}
	})

	t.Run("a failing rung — ok false, status fail, fix present", func(t *testing.T) {
		rungs := []rung{
			{name: "config", status: rungPass, detail: "ok"},
			{name: "worker", status: rungFail, detail: "unreachable", fix: "check endpoint"},
		}
		var buf strings.Builder
		failed := renderDoctor(&buf, rungs, true)

		if failed != 1 {
			t.Fatalf("want 1 failed, got %d", failed)
		}

		var result doctorResult
		if err := json.Unmarshal([]byte(buf.String()), &result); err != nil {
			t.Fatalf("JSON unmarshal failed: %v", err)
		}
		if result.Ok {
			t.Error("want ok=false")
		}
		failRung := result.Rungs[1]
		if failRung.Status != "fail" {
			t.Errorf("want status fail, got %s", failRung.Status)
		}
		if failRung.Fix != "check endpoint" {
			t.Errorf("want fix, got %q", failRung.Fix)
		}
	})
}

func TestTextRender(t *testing.T) {
	t.Run("text mode still prints human ladder", func(t *testing.T) {
		rungs := []rung{
			{name: "config", status: rungPass, detail: "enabled"},
			{name: "worker", status: rungFail, detail: "unreachable", fix: "check"},
		}
		var buf strings.Builder
		failed := renderDoctor(&buf, rungs, false)

		if failed != 1 {
			t.Fatalf("want 1 failed, got %d", failed)
		}

		out := buf.String()
		if !strings.Contains(out, "PASS") {
			t.Error("should contain PASS")
		}
		if !strings.Contains(out, "FAIL") {
			t.Error("should contain FAIL")
		}
		if !strings.Contains(out, "fix:") {
			t.Error("should contain fix line")
		}

		trimmed := strings.TrimSpace(out)
		if strings.HasPrefix(trimmed, "{") {
			t.Error("text mode should not output JSON")
		}
	})
}

// jsonTag maps rungStatus to the lowercase strings used in JSON output.
func TestRungStatusJSONTag(t *testing.T) {
	if rungPass.jsonTag() != "pass" {
		t.Errorf("rungPass.jsonTag() = %q, want \"pass\"", rungPass.jsonTag())
	}
	if rungFail.jsonTag() != "fail" {
		t.Errorf("rungFail.jsonTag() = %q, want \"fail\"", rungFail.jsonTag())
	}
	if rungSkip.jsonTag() != "skip" {
		t.Errorf("rungSkip.jsonTag() = %q, want \"skip\"", rungSkip.jsonTag())
	}
}
