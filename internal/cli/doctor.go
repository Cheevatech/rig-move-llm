// doctor is the pre-flight ladder as a command. It exists because the ladder
// already existed as ad-hoc shell one-liners, and every rung of it has burned a
// measurement: a run that looked healthy while nothing was being offloaded at all
// (#22), a worker whose every tool call was denied (#24), and a probe that
// reported healthy against a key that 401s on every real call (llama-swap answers
// /v1/models unauthenticated).
//
// The rule the ladder encodes: never trust a number from rig without proving the
// mechanism that produced it was live. A rung therefore checks the thing that
// does the work, not a proxy for it — a real completion rather than /v1/models.
//
// S4 removed three rungs with the layer they measured (hook-resolvable,
// hook-fires, gate-toolchain). They are gone rather than kept-and-skipped on
// purpose: a rung that reports on a mechanism nobody has any more is #22 again,
// one level up.
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/internal/thin"
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

// jsonTag maps rungStatus to the lowercase strings used in JSON output.
func (s rungStatus) jsonTag() string {
	switch s {
	case rungPass:
		return "pass"
	case rungFail:
		return "fail"
	default:
		return "skip"
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

// doctorResult is the top-level JSON output for machine-readable consumption.
type doctorResult struct {
	Ok    bool         `json:"ok"`
	Rungs []rungResult `json:"rungs"`
}

// rungResult is a single rung as it appears in the JSON output.
type rungResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
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
		fmt.Println("Usage: rig-move-llm doctor [--json]\n\n" +
			"Proves the switch is live before you trust a measurement: config, worker\n" +
			"endpoint (authenticated), the switch itself, workspace trust, the effective\n" +
			"guards, and whether the MAIN leg has credentials to open a session at all.\n" +
			"  --json  Output results as a single JSON object on stdout\n" +
			"Exits non-zero if any rung failed.")
		return 0
	}

	jsonMode := false
	for _, a := range args {
		if a == "--json" {
			jsonMode = true
		}
	}

	cwd, _ := os.Getwd()
	cfg := config.Load()

	rungs := []rung{
		checkConfig(cfg),
		checkWorkerEndpoint(cfg),
		checkSwitch(cfg),
		checkWorkspaceTrust(cwd),
		checkWorkerCommand(),
		checkGuards(),
		checkMainAuth(),
	}

	failed := renderDoctor(os.Stdout, rungs, jsonMode)
	if failed > 0 {
		return 1
	}
	return 0
}

// renderDoctor writes the report to w and returns the count of failed rungs.
// When jsonMode is true it emits a single JSON object; otherwise it prints
// the human-readable ladder. The collection/rendering split keeps cmdDoctor
// thin and makes renderDoctor directly testable with fabricated rungs.
func renderDoctor(w io.Writer, rungs []rung, jsonMode bool) int {
	failed := 0
	if jsonMode {
		for _, r := range rungs {
			if r.status == rungFail {
				failed++
			}
		}
		result := doctorResult{
			Ok:    failed == 0,
			Rungs: make([]rungResult, len(rungs)),
		}
		for i, r := range rungs {
			result.Rungs[i] = rungResult{
				Name:   r.name,
				Status: r.status.jsonTag(),
				Detail: r.detail,
				Fix:    r.fix,
			}
		}
		b, _ := json.MarshalIndent(result, "", "  ")
		fmt.Fprintln(w, string(b))
		return failed
	}

	for _, r := range rungs {
		fmt.Fprintf(w, "%-4s  %-18s  %s\n", r.status.label(), r.name, r.detail)
		if r.status == rungFail {
			failed++
			fmt.Fprintf(w, "      %-18s  fix: %s\n", "", r.fix)
		}
	}
	fmt.Fprintln(w)
	if failed > 0 {
		fmt.Fprintf(w, "%d of %d rungs failed — do not trust a measurement from this install yet.\n", failed, len(rungs))
	} else {
		fmt.Fprintf(w, "all %d rungs pass — the offload rig is live.\n", len(rungs))
	}
	return failed
}

// --- rung 1: config -------------------------------------------------------

func checkConfig(cfg config.Config) rung {
	const name = "config"
	if strings.TrimSpace(cfg.WorkerAPIBase) == "" {
		return fail(name, "no worker endpoint configured",
			"run `rig-move-llm setup` (or set WORKER_API_BASE in config.env)")
	}
	if !cfg.Enabled {
		return fail(name, "ENABLED=false — nothing is wired up to offload to",
			"run `rig-move-llm enable`")
	}
	return pass(name, fmt.Sprintf("ENABLED, worker=%s model=%s",
		cfg.WorkerAPIBase, cfg.WorkerModel))
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

// --- rung 3: the switch ---------------------------------------------------

// checkSwitch proves the one mechanism rig still has: a `claude -p` subprocess
// that can be pointed at the local model. There is no engine choice left to
// report — the loop engine and the contract layer it served were deleted in S4 —
// so this rung is unconditional. A skip here would be a lie about the only thing
// the product does.
func checkSwitch(cfg config.Config) rung {
	const name = "switch"
	base := strings.TrimSpace(cfg.CCBaseURL)
	if base == "" {
		return fail(name, "RIG_CC_BASE_URL is empty — the switch refuses to run rather than bill the worker leg to your account",
			"point RIG_CC_BASE_URL at an Anthropic-format endpoint for the worker model (rig serves one at /r/worker)")
	}
	bin := strings.TrimSpace(os.Getenv("RIG_CC_BIN"))
	if bin == "" {
		bin = "claude"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return fail(name, "the switch runs `"+bin+"` as a subprocess and it is not on PATH",
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
		return fail(name, "Claude Code does not trust "+cwd+" — it will not load rig's MCP server here",
			"run `rig-move-llm init --trust-workspace`, or open the dir in Claude Code once and accept the prompt")
	}
	return pass(name, "granted for "+cwd)
}

// commandMentionsRig reports whether a command string references the rig binary.
func commandMentionsRig(cmd string) bool {
	return strings.Contains(cmd, "rig-move-llm")
}

// getSettingsPaths returns all candidate settings.json and settings.local.json
// paths for the given repo and home directories. Only paths that actually exist
// are included.
func getSettingsPaths(repo, home string) []string {
	var paths []string
	for _, base := range []string{repo, home} {
		for _, name := range []string{"settings.json", "settings.local.json"} {
			p := filepath.Join(base, ".claude", name)
			if _, err := os.Stat(p); err == nil {
				paths = append(paths, p)
			}
		}
	}
	return paths
}

// --- rung 5: the command the MCP entry names is resolvable ----------------

// checkWorkerCommand asks whether the thing rig told Claude Code to launch can
// actually be launched. The MCP entry names `rig-move-llm` (or npx), and Claude
// Code resolves it from the PATH of whatever started it — so an install can be
// complete in every visible way and still never start a worker.
//
// Measured 2026-08-04 on a real machine: `init --global` wrote a correct
// registration, and the session reported `worker: failed` with no
// mcp__worker__implement anywhere, because the binary had never been put on
// PATH. Nothing said so. The old ladder had a rung for exactly this shape, aimed
// at the hook command; S4 deleted it along with the hooks instead of repointing
// it at the command that survived, so this rung is that one, restored to the
// surface rig actually has.
func checkWorkerCommand() rung {
	const name = "worker command"
	cmd, source := workerMCPCommand()
	if cmd == "" {
		return skip(name, "no MCP registration found — run `rig-move-llm init`")
	}
	if cmd == "npx" {
		// npx resolves the package at spawn time; the only thing to check here is
		// that npx itself exists.
		if p, err := exec.LookPath("npx"); err == nil {
			return pass(name, "npx found at "+p+" (worker spawned via npx, from "+source+")")
		}
		return fail(name, "the registration in "+source+" spawns the worker with npx, and npx is not on PATH",
			"install Node, or re-run `rig-move-llm init` without --npx after installing the binary globally")
	}
	p, err := exec.LookPath(cmd)
	if err != nil {
		return fail(name,
			fmt.Sprintf("%s registers the worker as %q, and that is NOT on PATH — Claude Code reports the server as `failed` and no mcp__worker__implement tool exists, so nothing can be delegated", source, cmd),
			"install it globally (`npm i -g rig-move-llm`), or put the binary on the PATH of whatever launches Claude Code")
	}
	return pass(name, fmt.Sprintf("%s found at %s (registered in %s)", cmd, p, source))
}

// workerMCPCommand reads back the registration rig actually wrote, rather than
// assuming what it would have written: the point of the rung is to check the
// file on disk, which may be from an older install or hand-edited.
func workerMCPCommand() (cmd, source string) {
	type entry struct {
		Command string `json:"command"`
	}
	// Project scope first — a project-root .mcp.json wins for this directory.
	if data, err := os.ReadFile(".mcp.json"); err == nil {
		var f struct {
			MCPServers map[string]entry `json:"mcpServers"`
		}
		if json.Unmarshal(data, &f) == nil {
			if e, ok := f.MCPServers["worker"]; ok && e.Command != "" {
				return e.Command, ".mcp.json"
			}
		}
	}
	if data, err := os.ReadFile(userClaudeJSON()); err == nil {
		var f struct {
			MCPServers map[string]entry `json:"mcpServers"`
		}
		if json.Unmarshal(data, &f) == nil {
			if e, ok := f.MCPServers["worker"]; ok && e.Command != "" {
				return e.Command, "~/.claude.json (user scope)"
			}
		}
	}
	return "", ""
}

// --- rung 6: the guards ---------------------------------------------------

// checkGuards reports the two ceilings rather than judging them: silence, then
// total wall, then the per-call timeout rig declares to the client. The ladder
// must hold or a run is killed by someone who cannot explain why — the client
// aborting first means the caller gets a bare protocol error instead of a
// diagnosis and the partial diff.
//
// Two numbers that used to be here are gone with the layer that needed them:
// the gate credit (there is no gate to excuse time for) and the per-turn
// delegation budget (nothing counts rounds any more).
func checkGuards() rung {
	const name = "guards"
	stall, wall, client := thin.StallCeiling(), thin.WallCeiling(), thin.ClientCallTimeout()
	detail := fmt.Sprintf("stall %s%s < wall %s%s < client %s",
		stall, envSource("RIG_THIN_STALL"),
		wall, envSource("RIG_THIN_WALL"),
		client)
	if stall >= wall || wall >= client {
		return fail(name, detail,
			"restore the ordering stall < wall < client, or the run is killed by someone who cannot explain why")
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

// --- rung 7: the MAIN leg can start a session at all ----------------------

// checkMainAuth is the hole the other rungs left open. Measured
// 2026-07-30: an install answered all-PASS in a HOME where `claude` had never
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
