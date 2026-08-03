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
	"github.com/Cheevatech/rig-move-llm/internal/worker"
)

// The rung that #35 is about. The failure it exists to catch is NOT "no
// toolchain" — it is "toolchain installed, invisible to the worker", which is what
// burned task12: go lived at ~/go/bin/go, the worker inherited a PATH without it,
// ran a stray pytest instead, and shipped 1157 lines that had never compiled.
func TestGateToolchainRung(t *testing.T) {
	t.Run("on PATH passes and names the resolved binary", func(t *testing.T) {
		repo := t.TempDir()
		mustWrite(t, filepath.Join(repo, "go.mod"), "module x\n")
		bin := t.TempDir()
		fake := filepath.Join(bin, "go")
		mustWrite(t, fake, "#!/bin/sh\n")
		if err := os.Chmod(fake, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin)

		r := checkGateToolchain(repo)

		if r.status != rungPass {
			t.Fatalf("status = %s, want PASS (%s)", r.status.label(), r.detail)
		}
		if !strings.Contains(r.detail, fake) {
			t.Errorf("detail should name the resolved binary %q:\n%s", fake, r.detail)
		}
	})

	// The task12 shape: the toolchain IS installed, PATH just does not carry it.
	// Cargo's search list is home-relative only, so an overridden HOME keeps this
	// hermetic on a machine that really has a toolchain in /usr/local.
	t.Run("installed but off PATH fails and names the dir to add", func(t *testing.T) {
		repo := t.TempDir()
		mustWrite(t, filepath.Join(repo, "Cargo.toml"), "[package]\n")

		home := t.TempDir()
		cargoBin := filepath.Join(home, ".cargo", "bin")
		if err := os.MkdirAll(cargoBin, 0o755); err != nil {
			t.Fatal(err)
		}
		installed := filepath.Join(cargoBin, "cargo")
		mustWrite(t, installed, "#!/bin/sh\n")
		if err := os.Chmod(installed, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", home)
		t.Setenv("PATH", t.TempDir())

		r := checkGateToolchain(repo)

		if r.status != rungFail {
			t.Fatalf("status = %s, want FAIL (%s)", r.status.label(), r.detail)
		}
		for _, want := range []string{"NOT on the PATH the worker inherits", "IS installed at", installed} {
			if !strings.Contains(r.detail, want) {
				t.Errorf("detail missing %q:\n%s", want, r.detail)
			}
		}
		if !strings.Contains(r.fix, cargoBin) {
			t.Errorf("fix must name the directory to add (%s):\n%s", cargoBin, r.fix)
		}
	})

	// A project-local venv is searched relative to the REPO, not to wherever
	// doctor was invoked from.
	t.Run("a project-local venv is found relative to the repo", func(t *testing.T) {
		repo := t.TempDir()
		mustWrite(t, filepath.Join(repo, "pyproject.toml"), "[project]\n")
		venv := filepath.Join(repo, ".venv", "bin")
		if err := os.MkdirAll(venv, 0o755); err != nil {
			t.Fatal(err)
		}
		installed := filepath.Join(venv, "pytest")
		mustWrite(t, installed, "#!/bin/sh\n")
		if err := os.Chmod(installed, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", t.TempDir())
		t.Setenv("PATH", t.TempDir())

		if r := checkGateToolchain(repo); !strings.Contains(r.detail, installed) {
			t.Errorf("detail should name the repo-local venv binary %q:\n%s", installed, r.detail)
		}
	})

	t.Run("missing entirely fails with an install fix", func(t *testing.T) {
		repo := t.TempDir()
		mustWrite(t, filepath.Join(repo, "pyproject.toml"), "[project]\n")
		t.Setenv("HOME", t.TempDir())
		t.Setenv("PATH", t.TempDir())

		r := checkGateToolchain(repo)

		if r.status != rungFail {
			t.Fatalf("status = %s, want FAIL", r.status.label())
		}
		if !strings.Contains(r.detail, "pytest") || !strings.Contains(r.fix, "install pytest") {
			t.Errorf("want a pytest-specific diagnosis and fix, got %q / %q", r.detail, r.fix)
		}
	})

	t.Run("an unrecognised project shape skips rather than failing", func(t *testing.T) {
		if r := checkGateToolchain(t.TempDir()); r.status != rungSkip {
			t.Errorf("status = %s, want SKIP — rig must not invent a gate it cannot infer", r.status.label())
		}
	})

	// The rung reads internal/gate, the same table the worker engine runs from
	// (#59). It used to keep its own byte-identical copy, and the day the two
	// drifted this rung would bless a shape the worker cannot actually gate. An
	// ecosystem that exists in only one of the two copies proves which one this
	// reads.
	t.Run("it reads the shared table, not a private copy", func(t *testing.T) {
		repo := t.TempDir()
		mustWrite(t, filepath.Join(repo, "mix.exs"), "defmodule X do\nend\n")
		t.Setenv("HOME", t.TempDir())
		t.Setenv("PATH", t.TempDir())

		r := checkGateToolchain(repo)

		if r.status == rungSkip {
			t.Fatal("an Elixir repo is a recognised shape in internal/gate — this rung is reading something else")
		}
		if !strings.Contains(r.detail, "mix test") {
			t.Errorf("detail should name the shared table's gate command:\n%s", r.detail)
		}
	})

	// A wrapper is in the repo, so PATH is the wrong question — asking it
	// reported a missing toolchain for every Gradle/Maven-wrapper repo there is.
	t.Run("a repo-local wrapper is not looked for on PATH", func(t *testing.T) {
		repo := t.TempDir()
		mustWrite(t, filepath.Join(repo, "build.gradle"), "")
		wrapper := filepath.Join(repo, "gradlew")
		mustWrite(t, wrapper, "#!/bin/sh\n")
		if err := os.Chmod(wrapper, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", t.TempDir())
		t.Setenv("PATH", t.TempDir())

		r := checkGateToolchain(repo)

		if r.status != rungPass {
			t.Fatalf("status = %s, want PASS — the wrapper is right there (%s)", r.status.label(), r.detail)
		}
		if !strings.Contains(r.detail, "./gradlew test") {
			t.Errorf("detail should name the wrapper gate:\n%s", r.detail)
		}
	})
}

// The lesson from the mangled worker key: /v1/models answered 200 unauthenticated
// while every real call 401'd, so the health probe reported a healthy rig that
// could not do any work. This rung must call the endpoint that does the work, and
// must separate AUTH from unreachable.
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
	if r.status != rungFail || !strings.Contains(r.detail, "passes every tool through") {
		t.Errorf("ENABLED=false must FAIL and say why, got %s / %s", r.status.label(), r.detail)
	}
	if r := checkConfig(config.Config{WorkerAPIBase: "http://x/v1", WorkerModel: "m", Enabled: true}); r.status != rungPass {
		t.Errorf("a configured, enabled rig must PASS, got %s / %s", r.status.label(), r.detail)
	}
}

// The cc engine's own refusal rule (never bill the worker leg to the paid
// account) has to be visible as a rung, not discovered when a round dies.
func TestCCEngineRung(t *testing.T) {
	t.Run("loop engine skips", func(t *testing.T) {
		if r := checkCCEngine(config.Config{WorkerEngine: "loop"}); r.status != rungSkip {
			t.Errorf("status = %s, want SKIP", r.status.label())
		}
	})

	t.Run("cc without a base URL fails", func(t *testing.T) {
		r := checkCCEngine(config.Config{WorkerEngine: "cc"})
		if r.status != rungFail || !strings.Contains(r.detail, "RIG_CC_BASE_URL") {
			t.Errorf("want a base-URL diagnosis, got %s / %s", r.status.label(), r.detail)
		}
	})

	t.Run("cc with an anthropic-format endpoint passes", func(t *testing.T) {
		var hit string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hit = r.URL.Path
			w.Write([]byte(`{"content":[]}`))
		}))
		defer srv.Close()

		bin := t.TempDir()
		fake := filepath.Join(bin, "claude")
		mustWrite(t, fake, "#!/bin/sh\n")
		if err := os.Chmod(fake, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin)

		r := checkCCEngine(config.Config{WorkerEngine: "cc", CCBaseURL: srv.URL, CCModel: "qwen"})

		if r.status != rungPass {
			t.Fatalf("status = %s, want PASS (%s)", r.status.label(), r.detail)
		}
		if hit != "/v1/messages" {
			t.Errorf("probed %q, want the Anthropic messages endpoint", hit)
		}
	})

	t.Run("a missing claude CLI fails", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		r := checkCCEngine(config.Config{WorkerEngine: "cc", CCBaseURL: "http://127.0.0.1:1"})
		if r.status != rungFail || !strings.Contains(r.detail, "not on PATH") {
			t.Errorf("want a PATH diagnosis for the cc subprocess, got %s / %s", r.status.label(), r.detail)
		}
	})
}

// The guard rung reports the ladder AND names silent env overrides — two runs
// compared as the same configuration is how the wall claim went wrong.
func TestGuardsRung(t *testing.T) {
	t.Run("the shipping ladder passes and shows the numbers", func(t *testing.T) {
		r := checkGuards()
		if r.status != rungPass {
			t.Fatalf("status = %s, want PASS (%s)", r.status.label(), r.detail)
		}
		for _, want := range []string{worker.StallGuard().String(), worker.WallGuard().String(),
			worker.GateCredit().String(), worker.WallCeiling().String(), worker.ClientCallTimeout().String(), "budget"} {
			if !strings.Contains(r.detail, want) {
				t.Errorf("detail missing %q:\n%s", want, r.detail)
			}
		}
	})

	t.Run("an override is named, not silent", func(t *testing.T) {
		t.Setenv("RIG_WORKER_RUN_TIMEOUT", "2000")
		if r := checkGuards(); !strings.Contains(r.detail, "env RIG_WORKER_RUN_TIMEOUT=2000") {
			t.Errorf("an env override must be visible in the report:\n%s", r.detail)
		}
	})

	t.Run("a broken ladder fails", func(t *testing.T) {
		// A wall below the stall guard means the round dies without the stall
		// diagnosis ever being reachable.
		t.Setenv("RIG_WORKER_RUN_TIMEOUT", "60")
		if r := checkGuards(); r.status != rungFail {
			t.Errorf("status = %s, want FAIL for stall >= wall", r.status.label())
		}
	})
}

// The all-7-PASS install that could not delegate: the worker leg runs on a dummy
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
func TestHookResolvableRung(t *testing.T) {
	setHome := func(t *testing.T) string {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		return home
	}

	t.Run("resolvable command passes", func(t *testing.T) {
		home := setHome(t)
		repo := t.TempDir()
		claudeDir := filepath.Join(repo, ".claude")
		if err := os.MkdirAll(claudeDir, 0o755); err != nil {
			t.Fatal(err)
		}

		bin := t.TempDir()
		fake := filepath.Join(bin, "rig-move-llm")
		mustWrite(t, fake, "#!/bin/sh\necho ok\n")
		if err := os.Chmod(fake, 0o755); err != nil {
			t.Fatal(err)
		}

		settings := map[string]any{
			"hooks": map[string]any{
				"PreToolUse": []any{map[string]any{
					"matcher": "*",
					"hooks": []any{
						map[string]any{"type": "command", "command": fake + " hook pre-tool"},
					},
				}},
			},
		}
		settingsJSON, _ := json.MarshalIndent(settings, "", "  ")
		mustWrite(t, filepath.Join(claudeDir, "settings.json"), string(settingsJSON))

		r := checkHookResolvableIn(repo, home)
		if r.status != rungPass {
			t.Fatalf("status = %s, want PASS (%s)", r.status.label(), r.detail)
		}
	})

	t.Run("unresolvable command fails with command and file named", func(t *testing.T) {
		home := setHome(t)
		repo := t.TempDir()
		claudeDir := filepath.Join(repo, ".claude")
		if err := os.MkdirAll(claudeDir, 0o755); err != nil {
			t.Fatal(err)
		}

		settings := map[string]any{
			"hooks": map[string]any{
				"PreToolUse": []any{map[string]any{
					"matcher": "*",
					"hooks": []any{
						map[string]any{"type": "command", "command": "rig-move-llm-does-not-exist --hook"},
					},
				}},
			},
		}
		settingsJSON, _ := json.MarshalIndent(settings, "", "  ")
		settingsPath := filepath.Join(claudeDir, "settings.json")
		mustWrite(t, settingsPath, string(settingsJSON))

		r := checkHookResolvableIn(repo, home)

		if r.status != rungFail {
			t.Fatalf("status = %s, want FAIL (%s)", r.status.label(), r.detail)
		}
		if !strings.Contains(r.detail, "rig-move-llm-does-not-exist") {
			t.Errorf("detail must name the offending command:\n%s", r.detail)
		}
		if !strings.Contains(r.detail, settingsPath) {
			t.Errorf("detail must name the settings file:\n%s", r.detail)
		}
	})

	t.Run("bare name on PATH resolves", func(t *testing.T) {
		home := setHome(t)
		repo := t.TempDir()
		claudeDir := filepath.Join(repo, ".claude")
		if err := os.MkdirAll(claudeDir, 0o755); err != nil {
			t.Fatal(err)
		}

		settings := map[string]any{
			"hooks": map[string]any{
				"PreToolUse": []any{map[string]any{
					"matcher": "*",
					"hooks": []any{
						map[string]any{"type": "command", "command": "rig-move-llm hook pre-tool"},
					},
				}},
			},
		}
		settingsJSON, _ := json.MarshalIndent(settings, "", "  ")
		mustWrite(t, filepath.Join(claudeDir, "settings.json"), string(settingsJSON))

		// Create executable and put its dir on PATH.
		bin := t.TempDir()
		fake := filepath.Join(bin, "rig-move-llm")
		mustWrite(t, fake, "#!/bin/sh\necho ok\n")
		if err := os.Chmod(fake, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin+string(filepath.ListSeparator)+os.Getenv("PATH"))

		if r := checkHookResolvableIn(repo, home); r.status != rungPass {
			t.Fatalf("bare name on PATH should pass: status = %s (%s)", r.status.label(), r.detail)
		}

		// Now remove that dir from PATH and expect failure.
		t.Setenv("PATH", t.TempDir())
		if r := checkHookResolvableIn(repo, home); r.status != rungFail {
			t.Fatalf("bare name off PATH should fail: status = %s (%s)", r.status.label(), r.detail)
		}
	})

	t.Run("non-executable file fails", func(t *testing.T) {
		home := setHome(t)
		repo := t.TempDir()
		claudeDir := filepath.Join(repo, ".claude")
		if err := os.MkdirAll(claudeDir, 0o755); err != nil {
			t.Fatal(err)
		}

		bin := t.TempDir()
		notExec := filepath.Join(bin, "rig-move-llm")
		mustWrite(t, notExec, "#!/bin/sh\necho not-exec\n")
		// Deliberately leave mode at 0644 (not executable).

		settings := map[string]any{
			"hooks": map[string]any{
				"PreToolUse": []any{map[string]any{
					"matcher": "*",
					"hooks": []any{
						map[string]any{"type": "command", "command": notExec + " hook pre-tool"},
					},
				}},
			},
		}
		settingsJSON, _ := json.MarshalIndent(settings, "", "  ")
		mustWrite(t, filepath.Join(claudeDir, "settings.json"), string(settingsJSON))

		r := checkHookResolvableIn(repo, home)

		if r.status != rungFail {
			t.Fatalf("non-executable file should fail: status = %s (%s)", r.status.label(), r.detail)
		}
	})

	t.Run("no rig hooks configured fails", func(t *testing.T) {
		home := setHome(t)
		repo := t.TempDir()
		claudeDir := filepath.Join(repo, ".claude")
		if err := os.MkdirAll(claudeDir, 0o755); err != nil {
			t.Fatal(err)
		}

		// Settings with no hooks at all.
		mustWrite(t, filepath.Join(claudeDir, "settings.json"), `{"permissions":{"allow":[]}}`)

		r := checkHookResolvableIn(repo, home)

		if r.status != rungFail {
			t.Fatalf("no hooks should fail: status = %s (%s)", r.status.label(), r.detail)
		}
		if !strings.Contains(r.detail, "no rig-move-llm hook") {
			t.Errorf("detail should say no rig hooks found:\n%s", r.detail)
		}
	})

	t.Run("empty hooks fails", func(t *testing.T) {
		home := setHome(t)
		repo := t.TempDir()
		claudeDir := filepath.Join(repo, ".claude")
		if err := os.MkdirAll(claudeDir, 0o755); err != nil {
			t.Fatal(err)
		}

		// Settings with empty hooks object.
		mustWrite(t, filepath.Join(claudeDir, "settings.json"), `{"hooks":{}}`)

		r := checkHookResolvableIn(repo, home)

		if r.status != rungFail {
			t.Fatalf("empty hooks should fail: status = %s (%s)", r.status.label(), r.detail)
		}
	})

	t.Run("settings.local.json with unresolvable command fails", func(t *testing.T) {
		home := setHome(t)
		repo := t.TempDir()
		claudeDir := filepath.Join(repo, ".claude")
		if err := os.MkdirAll(claudeDir, 0o755); err != nil {
			t.Fatal(err)
		}

		settings := map[string]any{
			"hooks": map[string]any{
				"PreToolUse": []any{map[string]any{
					"matcher": "*",
					"hooks": []any{
						map[string]any{"type": "command", "command": "rig-move-llm-fake-local --hook"},
					},
				}},
			},
		}
		settingsJSON, _ := json.MarshalIndent(settings, "", "  ")
		settingsLocalPath := filepath.Join(claudeDir, "settings.local.json")
		mustWrite(t, settingsLocalPath, string(settingsJSON))

		r := checkHookResolvableIn(repo, home)

		if r.status != rungFail {
			t.Fatalf("status = %s, want FAIL (%s)", r.status.label(), r.detail)
		}
		if !strings.Contains(r.detail, "rig-move-llm-fake-local") {
			t.Errorf("detail must name the offending command:\n%s", r.detail)
		}
		if !strings.Contains(r.detail, "settings.local.json") {
			t.Errorf("detail must name the settings.local.json path:\n%s", r.detail)
		}
	})

	t.Run("hooks from other tools only fails", func(t *testing.T) {
		home := setHome(t)
		repo := t.TempDir()
		claudeDir := filepath.Join(repo, ".claude")
		if err := os.MkdirAll(claudeDir, 0o755); err != nil {
			t.Fatal(err)
		}

		settings := map[string]any{
			"hooks": map[string]any{
				"PreToolUse": []any{map[string]any{
					"matcher": "*",
					"hooks": []any{
						map[string]any{"type": "command", "command": "some-other-tool do-something"},
					},
				}},
			},
		}
		settingsJSON, _ := json.MarshalIndent(settings, "", "  ")
		mustWrite(t, filepath.Join(claudeDir, "settings.json"), string(settingsJSON))

		r := checkHookResolvableIn(repo, home)

		if r.status != rungFail {
			t.Fatalf("non-rig hooks should fail: status = %s (%s)", r.status.label(), r.detail)
		}
	})
}
