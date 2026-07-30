// doctor is the pre-flight ladder as a command. It exists because the ladder
// already existed as ad-hoc shell one-liners, and every rung of it has burned a
// measurement: a run that looked healthy while the hook was passing everything
// through (#22), a worker whose every tool call was denied (#24), a probe that
// reported healthy against a key that 401s on every real call (llama-swap answers
// /v1/models unauthenticated), and a 60-minute round whose gate could not run at
// all because the toolchain was invisible to the worker (#35).
//
// The rule the ladder encodes: never trust a number from rig without proving the
// mechanism that produced it was live. A rung therefore checks the thing that
// does the work, not a proxy for it — a real completion rather than /v1/models, a
// real hook invocation rather than the presence of a settings key.
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/internal/worker"
)

// rungStatus is a rung's verdict. skip is not a failure: a rung that does not
// apply to this install (the cc engine when the loop engine is configured) must
// not turn a healthy doctor red.
type rungStatus int

const (
	rungPass rungStatus = iota
	rungFail
	rungSkip
)

func (s rungStatus) label() string {
	switch s {
	case rungPass:
		return "PASS"
	case rungFail:
		return "FAIL"
	default:
		return "SKIP"
	}
}

// rung is one line of the report: what was checked, what happened, and — when it
// failed — the concrete thing to do about it. fix is mandatory on a FAIL by
// convention: a diagnosis the user cannot act on is how the ad-hoc ladder used to
// waste a whole run.
type rung struct {
	name   string
	status rungStatus
	detail string
	fix    string
}

func pass(name, detail string) rung { return rung{name: name, status: rungPass, detail: detail} }
func fail(name, detail, fix string) rung {
	return rung{name: name, status: rungFail, detail: detail, fix: fix}
}
func skip(name, detail string) rung { return rung{name: name, status: rungSkip, detail: detail} }

// doctorTimeout bounds each probe doctor makes. Long enough for a COLD local
// model: measured on llama-swap, the first completion after an idle period pays
// the model load, which comfortably exceeds the 20s this used to be — and doctor
// reporting "unreachable" for an endpoint that was merely warming up is exactly
// the kind of false diagnosis this command exists to eliminate.
const doctorTimeout = 90 * time.Second

func cmdDoctor(args []string) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Println("Usage: rig-move-llm doctor\n\n" +
			"Proves the offload rig is live before you trust a measurement: config, worker\n" +
			"endpoint (authenticated), cc engine, workspace trust, hooks (actually firing),\n" +
			"the repo's gate toolchain as the WORKER sees it, the effective guards, and\n" +
			"whether the MAIN leg has credentials to open a session at all.\n" +
			"Exits non-zero if any rung fails.")
		return 0
	}

	cwd, _ := os.Getwd()
	cfg := config.Load()

	rungs := []rung{
		checkConfig(cfg),
		checkWorkerEndpoint(cfg),
		checkCCEngine(cfg),
		checkWorkspaceTrust(cwd),
		checkHookLive(),
		checkGateToolchain(cwd),
		checkGuards(),
		checkMainAuth(),
	}

	failed := 0
	for _, r := range rungs {
		fmt.Printf("%-4s  %-18s  %s\n", r.status.label(), r.name, r.detail)
		if r.status == rungFail {
			failed++
			fmt.Printf("      %-18s  fix: %s\n", "", r.fix)
		}
	}
	fmt.Println()
	if failed > 0 {
		fmt.Printf("%d of %d rungs failed — do not trust a measurement from this install yet.\n", failed, len(rungs))
		return 1
	}
	fmt.Printf("all %d rungs pass — the offload rig is live.\n", len(rungs))
	return 0
}

// --- rung 1: config -------------------------------------------------------

func checkConfig(cfg config.Config) rung {
	const name = "config"
	if strings.TrimSpace(cfg.WorkerAPIBase) == "" {
		return fail(name, "no worker endpoint configured",
			"run `rig-move-llm setup` (or set WORKER_API_BASE in config.env)")
	}
	if !cfg.Enabled {
		return fail(name, "ENABLED=false — the hook passes every tool through and nothing is delegated",
			"run `rig-move-llm enable`")
	}
	return pass(name, fmt.Sprintf("ENABLED, worker=%s model=%s engine=%s",
		cfg.WorkerAPIBase, cfg.WorkerModel, engineName(cfg)))
}

// engineName reports which engine implement will actually use, mirroring the
// selection rule in the worker package (cc when RIG_CC_BASE_URL is set and the
// engine is unset).
func engineName(cfg config.Config) string {
	switch strings.TrimSpace(cfg.WorkerEngine) {
	case "cc":
		return "cc"
	case "":
		if strings.TrimSpace(cfg.CCBaseURL) != "" {
			return "cc"
		}
		return "loop"
	default:
		return "loop"
	}
}

// --- rung 2: worker endpoint, authenticated -------------------------------

// checkWorkerEndpoint does a REAL minimal completion instead of the health
// probe's /v1/models. Measured 2026-07-30: a mangled WORKER_API_KEY (dashes
// stripped by a harness one-liner) 401'd every real call while /v1/models kept
// answering 200 unauthenticated, so health.json said healthy:true for a rig that
// could not do any work. An auth failure is reported as its own class — it is a
// different fix from an endpoint that is down.
func checkWorkerEndpoint(cfg config.Config) rung {
	const name = "worker endpoint"
	body, _ := json.Marshal(map[string]any{
		"model":      cfg.WorkerModel,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
	})
	url := strings.TrimRight(cfg.WorkerAPIBase, "/") + "/chat/completions"
	code, err := probeJSON(url, cfg.WorkerAPIKey, body)
	switch {
	case err != nil:
		fix := "check WORKER_API_BASE and that the endpoint is up: " + url
		if strings.Contains(err.Error(), "Client.Timeout") || strings.Contains(err.Error(), "deadline exceeded") {
			fix = "the endpoint accepted the connection but did not answer within " + doctorTimeout.String() +
				" — a local model may still be loading; warm it up and re-run doctor"
		}
		return fail(name, "unreachable: "+err.Error(), fix)
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fail(name, fmt.Sprintf("AUTH: the endpoint answered %d — the key is wrong or missing (a health probe against /v1/models cannot see this)", code),
			"fix WORKER_API_KEY in config.env; copy it verbatim, including dashes")
	case code >= 300:
		return fail(name, fmt.Sprintf("the endpoint answered %d to a real completion", code),
			"check WORKER_MODEL is a model this endpoint serves")
	}
	return pass(name, "a real completion succeeded (not just /v1/models)")
}

// --- rung 3: cc engine ----------------------------------------------------

func checkCCEngine(cfg config.Config) rung {
	const name = "cc engine"
	if engineName(cfg) != "cc" {
		return skip(name, "loop engine configured — nothing to check")
	}
	base := strings.TrimSpace(cfg.CCBaseURL)
	if base == "" {
		return fail(name, "RIG_WORKER_ENGINE=cc but RIG_CC_BASE_URL is empty — the engine refuses to run rather than bill the worker leg to your account",
			"point RIG_CC_BASE_URL at an Anthropic-format endpoint for the worker model (rig serves one at /r/worker)")
	}
	bin := strings.TrimSpace(os.Getenv("RIG_CC_BIN"))
	if bin == "" {
		bin = "claude"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return fail(name, "the cc engine runs `"+bin+"` as a subprocess and it is not on PATH",
			"install the claude CLI, or set RIG_CC_BIN to its absolute path")
	}

	body, _ := json.Marshal(map[string]any{
		"model":      ccModelOf(cfg),
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
	})
	code, err := probeJSON(strings.TrimRight(base, "/")+"/v1/messages", "", body)
	switch {
	case err != nil:
		return fail(name, "RIG_CC_BASE_URL unreachable: "+err.Error(),
			"start the endpoint (`rig-move-llm serve`) or fix RIG_CC_BASE_URL")
	case code >= 300:
		return fail(name, fmt.Sprintf("RIG_CC_BASE_URL answered %d to an Anthropic-format request", code),
			"the cc engine needs an ANTHROPIC-format endpoint; an OpenAI-format one will not do")
	}
	return pass(name, bin+" on PATH, "+base+" answers Anthropic-format")
}

func ccModelOf(cfg config.Config) string {
	if m := strings.TrimSpace(cfg.CCModel); m != "" {
		return m
	}
	return "haiku"
}

// --- rung 4: workspace trust ---------------------------------------------

func checkWorkspaceTrust(cwd string) rung {
	const name = "workspace trust"
	if cwd == "" {
		return fail(name, "cannot resolve the working directory", "run doctor from inside the project")
	}
	if !workspaceTrusted(cwd) {
		return fail(name, "Claude Code does not trust "+cwd+" — it will not load rig's hooks or MCP server here",
			"run `rig-move-llm init --trust-workspace`, or open the dir in Claude Code once and accept the prompt")
	}
	return pass(name, "granted for "+cwd)
}

// --- rung 5: the hook actually fires --------------------------------------

// checkHookLive feeds a MAIN Bash payload through rig's own hook and requires a
// deny. The presence of hook entries in .claude/settings.json proves nothing:
// #22 was a session where the hooks were wired, the config was right, and every
// tool passed through anyway because a failed health probe put the hook in
// passthrough for the whole run. What the measurement needed was a deny it could
// see.
func checkHookLive() rung {
	const name = "hook live"
	self, err := os.Executable()
	if err != nil {
		return fail(name, "cannot locate the rig binary to invoke: "+err.Error(), "reinstall rig-move-llm")
	}
	cmd := exec.Command(self, "hook", "pre-tool")
	cmd.Stdin = strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"echo doctor"}}`)
	out, err := cmd.Output()
	if err != nil {
		return fail(name, "the pre-tool hook errored: "+err.Error(), "run `rig-move-llm init` to rewire the hooks")
	}

	var decision struct {
		HookSpecificOutput struct {
			PermissionDecision string `json:"permissionDecision"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal(out, &decision) != nil {
		return fail(name, "the pre-tool hook returned output this version does not understand",
			"upgrade rig-move-llm and re-run `rig-move-llm init`")
	}
	if decision.HookSpecificOutput.PermissionDecision != "deny" {
		return fail(name, "a MAIN Bash call was NOT denied — the hook is in passthrough, so nothing is being delegated and any measurement is of plain Claude Code",
			"check the other rungs first (a down worker endpoint puts the hook in passthrough by design), then `rig-move-llm init`")
	}
	return pass(name, "a MAIN Bash call was denied, as it must be")
}

// --- rung 6: the gate toolchain, as the WORKER sees it (#35) --------------

// gateTool is a repo-shape marker paired with the command that verifies that
// shape, plus the places the command commonly lives when it is installed but not
// on PATH.
type gateTool struct {
	marker string
	cmd    string
	verify string
	lookIn []string
}

var gateTools = []gateTool{
	{marker: "go.mod", cmd: "go", verify: "go build ./... && go test ./...",
		lookIn: []string{"/usr/local/go/bin", "~/go/bin", "/usr/lib/go/bin", "/opt/homebrew/bin"}},
	{marker: "pyproject.toml", cmd: "pytest", verify: "pytest -q",
		lookIn: []string{".venv/bin", "venv/bin", "~/.local/bin"}},
	{marker: "setup.py", cmd: "pytest", verify: "pytest -q",
		lookIn: []string{".venv/bin", "venv/bin", "~/.local/bin"}},
	{marker: "package.json", cmd: "npm", verify: "npm test",
		lookIn: []string{"/usr/local/bin", "/opt/homebrew/bin"}},
	{marker: "Cargo.toml", cmd: "cargo", verify: "cargo test",
		lookIn: []string{"~/.cargo/bin"}},
}

// checkGateToolchain is #35. The worker inherits its PATH from the process that
// spawns it, so a toolchain that is installed but not on THIS PATH does not exist
// as far as the worker is concerned — and a worker that cannot run the repo's
// gate cannot produce evidence, only claims.
//
// Measured (map13 task12): `go` was installed at ~/go/bin/go and invisible to the
// worker, which fell back to a stray pytest run reporting "3 passed" for a Go
// change and shipped 1157 lines that had never compiled. 60 minutes gated on
// nothing. The failure mode was not "no toolchain" but "toolchain not visible to
// the worker", so a rung that only asks "is it installed somewhere?" would have
// passed and taught us nothing.
func checkGateToolchain(cwd string) rung {
	const name = "gate toolchain"
	tool, ok := detectGateTool(cwd)
	if !ok {
		return skip(name, "no recognised project shape here — rig cannot infer the gate command")
	}
	if p, err := exec.LookPath(tool.cmd); err == nil {
		return pass(name, fmt.Sprintf("%s found at %s (gate: %s)", tool.cmd, p, tool.verify))
	}
	detail := fmt.Sprintf("%s (%s) declares a %s project, but `%s` is NOT on the PATH the worker inherits — it cannot run `%s`, so it cannot prove anything it changes",
		tool.marker, cwd, tool.cmd, tool.cmd, tool.verify)
	if found := findOffPath(tool, cwd); found != "" {
		return fail(name, detail+"; it IS installed at "+found,
			"add "+filepath.Dir(found)+" to PATH in the environment that launches Claude Code (a non-interactive shell does not read your profile)")
	}
	return fail(name, detail, "install "+tool.cmd+" and make sure it is on PATH for the process that launches Claude Code")
}

func detectGateTool(dir string) (gateTool, bool) {
	for _, t := range gateTools {
		if _, err := os.Stat(filepath.Join(dir, t.marker)); err == nil {
			return t, true
		}
	}
	return gateTool{}, false
}

// findOffPath looks for the command in the usual install locations, so the report
// can name the directory to add instead of telling the user to install something
// they already have. Relative candidates (a project's .venv/bin) resolve against
// the REPO, not the process working directory — the worker runs in the repo, and
// doctor may not.
func findOffPath(t gateTool, repo string) string {
	home, _ := os.UserHomeDir()
	for _, dir := range t.lookIn {
		switch {
		case strings.HasPrefix(dir, "~/"):
			if home == "" {
				continue
			}
			dir = filepath.Join(home, dir[2:])
		case !filepath.IsAbs(dir):
			dir = filepath.Join(repo, dir)
		}
		p := filepath.Join(dir, t.cmd)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// --- rung 7: the guards ---------------------------------------------------

// checkGuards reports the ladder rather than judging it: stall < wall < the
// per-call timeout rig declares to the client (#33). Ordering is enforced by
// tests in the worker package; what a user needs here is the effective numbers
// and where each came from, because an env override is invisible in config.env.
func checkGuards() rung {
	const name = "guards"
	stall, wall, client := worker.StallGuard(), worker.WallGuard(), worker.ClientCallTimeout()
	detail := fmt.Sprintf("stall %s%s < wall %s%s < client %s; delegation budget %d/turn%s",
		stall, envSource("RIG_CC_STALL_TIMEOUT"),
		wall, envSource("RIG_WORKER_RUN_TIMEOUT"),
		client,
		maxDelegateRounds(), envSource("RIG_MAX_DELEGATE_ROUNDS"))
	if stall >= wall || wall >= client {
		return fail(name, detail,
			"restore the ordering stall < wall < client, or the round is killed by someone who cannot explain why")
	}
	return pass(name, detail)
}

// envSource annotates a value with where it came from — a silent override is how
// two runs get compared as if they were the same configuration.
func envSource(key string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return " (env " + key + "=" + v + ")"
	}
	return ""
}

// --- rung 8: the MAIN leg can start a session at all ----------------------

// checkMainAuth is the hole the other seven rungs left open. Measured
// 2026-07-30: an install answered all-7-PASS in a HOME where `claude` had never
// been logged in — the worker leg rides a dummy key against the local endpoint
// (auth hygiene, by design), so nothing below this rung touches the paid
// identity, and doctor happily blessed an install that could not open a MAIN
// session and therefore could not delegate anything.
//
// It is a credentials-PRESENT check, not a liveness one: the only way to prove
// the subscription leg is live is to spend a real request on it, and a
// diagnostic that silently bills the user is not one we ship. So the rung's
// claim is narrow and its detail says so — an expired token still reads as
// PASS here, and Claude Code will say so itself on launch.
func checkMainAuth() rung {
	const name = "MAIN auth"
	if strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) != "" {
		return pass(name, "ANTHROPIC_API_KEY is set — the MAIN leg bills the API rather than a subscription")
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return fail(name, "cannot resolve HOME, so the credentials Claude Code would use are unknown",
			"set HOME to the account that runs the MAIN session")
	}
	launch := "HOME=" + home + " claude"
	if _, err := os.Stat(filepath.Join(home, ".claude", ".credentials.json")); err == nil {
		return pass(name, "subscription credentials present in "+home+"/.claude (freshness not checked — an expired token still reads as PASS)")
	}
	if hasOAuthAccount(filepath.Join(home, ".claude.json")) {
		return pass(name, "a logged-in account is recorded in "+home+"/.claude.json (freshness not checked)")
	}
	return fail(name, "no MAIN credentials under HOME="+home+" — `claude` cannot open a session here, so there is no MAIN leg to delegate FROM (the worker leg's dummy key hides this from every other rung)",
		"run `"+launch+"` once, log in, and accept the workspace trust prompt — it needs a terminal, so it cannot be scripted")
}

// hasOAuthAccount reports whether Claude Code's config records a logged-in
// account. On macOS the token itself lives in the Keychain rather than in a
// file, so this key is the only file-visible evidence of a login there.
func hasOAuthAccount(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var cfg struct {
		OAuthAccount json.RawMessage `json:"oauthAccount"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return false
	}
	return len(cfg.OAuthAccount) > 0 && string(cfg.OAuthAccount) != "null"
}

// --- shared probe ---------------------------------------------------------

// probeJSON POSTs body and returns the status code. A transport error is returned
// separately from an HTTP status: "nothing is listening" and "it said no" are
// different diagnoses with different fixes.
func probeJSON(url, key string, body []byte) (int, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(key) != "" {
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("x-api-key", key)
	}
	resp, err := (&http.Client{Timeout: doctorTimeout}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}
