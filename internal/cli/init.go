package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/internal/service"
	"github.com/Cheevatech/rig-move-llm/internal/thin"
)

// initOpts is the resolved bootstrap request. Both cmdInit (flag-driven) and the
// interactive wizard (cmdSetup) build one and hand it to applyInit, so the wiring
// lives in exactly one place.
type initOpts struct {
	global       bool
	backend      string
	workerBase   string
	workerModel  string
	workerKey    string
	mainUpstream string
	port         string
	ccBase       string // RIG_CC_BASE_URL — the endpoint the switch points claude -p at
	ccModel      string // RIG_CC_MODEL (default haiku)
	enabled      bool   // ENABLED written to config; false = wired but inert (Claude Code runs normally)
	npxWorker    bool   // spawn the worker MCP as `npx -y rig-move-llm worker` (zero global install)
	service      bool
	force        bool
	noDetect     bool
	// trustWorkspace grants Claude Code's workspace trust for this directory (see
	// trust.go). It is never implied: only --trust-workspace, RIG_INIT_TRUST=1, or
	// the wizard's own question set it.
	trustWorkspace bool
}

// cmdInit bootstraps a scope: it writes the config file and wires Claude Code
// (permissions + the switch's MCP entry) so that a plain `claude`
// launches a working hybrid. Local (default) touches only this project; --global
// touches ~/.claude and applies to every project (the "follows you" mode).
func cmdInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	global := fs.Bool("global", false, "install for all projects (~/.claude + ~/.rig-move-llm)")
	backend := fs.String("backend", "", "worker backend: "+strings.Join(config.BackendNames(), "|"))
	workerBase := fs.String("worker-base", "", "worker OpenAI-compatible base URL (e.g. http://localhost:11434/v1)")
	workerModel := fs.String("worker-model", "", "worker model name")
	workerKey := fs.String("worker-key", "", "worker API key (optional for local models)")
	mainUpstream := fs.String("main-upstream", "https://api.anthropic.com", "paid (main-leg) upstream")
	port := fs.String("port", "4000", "proxy listen port")
	ccBase := fs.String("cc-base-url", "", "Anthropic-format base URL the switch points `claude -p` at")
	ccModel := fs.String("cc-model", "", "model name the subprocess runs as (default haiku)")
	npx := fs.Bool("npx", false, "spawn the worker via `npx -y rig-move-llm worker` (no global binary needed)")
	force := fs.Bool("force", false, "overwrite an existing config file")
	noDetect := fs.Bool("no-detect", false, "skip probing for a local worker endpoint")
	svc := fs.Bool("service", false, "install an OS service so the proxy survives reboots (requires --global)")
	trust := fs.Bool("trust-workspace", false, "accept Claude Code's workspace trust for this directory, so a headless `claude -p` run honours the permissions init writes (equivalent to accepting the trust dialog here; `uninstall` reverts it)")
	_ = fs.Parse(args)

	// Same grant, for a non-interactive installer that cannot pass flags.
	if os.Getenv("RIG_INIT_TRUST") == "1" {
		*trust = true
	}

	if *svc && !*global {
		fmt.Fprintln(os.Stderr, "init: --service requires --global (the daemon reads ~/.rig-move-llm/config.env, not a project dir)")
		return 2
	}

	// Zero-config path: if the user named neither a backend nor a base URL, probe
	// the machine for a local worker (Ollama / llama.cpp) and pre-fill.
	if *backend == "" && *workerBase == "" && !*noDetect {
		if d, ok := detectWorker(); ok {
			*backend, *workerBase = d.Backend, d.Base
			if *workerModel == "" {
				*workerModel = d.Model
			}
			fmt.Printf("detected %s at %s%s\n", d.Backend, d.Base, modelNote(d.Model))
		} else {
			fmt.Println("no local worker detected (probed Ollama:11434, llama.cpp:8080) — edit config.env to set WORKER_API_BASE")
		}
	}

	return applyInit(initOpts{
		global: *global, backend: *backend, workerBase: *workerBase,
		workerModel: *workerModel, workerKey: *workerKey, mainUpstream: *mainUpstream,
		port: *port, ccBase: *ccBase, ccModel: *ccModel,
		// A worker endpoint was configured -> enable; otherwise stay inert.
		enabled:   *workerBase != "" || *backend != "",
		npxWorker: *npx, service: *svc, force: *force, noDetect: *noDetect,
		trustWorkspace: *trust,
	})
}

// applyInit performs the actual bootstrap for a resolved initOpts.
func applyInit(o initOpts) int {
	dataDir := config.LocalDir()
	claudeDir := filepath.Join(".", ".claude")
	if o.global {
		dataDir = config.GlobalDir()
		home, _ := os.UserHomeDir()
		claudeDir = filepath.Join(home, ".claude")
	}

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "init:", err)
		return 1
	}

	// 1. config.env
	cfgPath := filepath.Join(dataDir, config.ConfigFile)
	preExisting := fileExists(cfgPath)
	if preExisting && !o.force {
		fmt.Printf("config exists: %s (use --force to overwrite)\n", cfgPath)
	} else {
		if err := os.WriteFile(cfgPath, []byte(renderConfigEnv(configEnvVals{
			backend: o.backend, workerBase: o.workerBase, workerModel: o.workerModel,
			workerKey: o.workerKey, mainUpstream: o.mainUpstream, port: o.port,
			ccBase: o.ccBase, ccModel: o.ccModel,
			enabled: o.enabled,
		})), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "init: write config:", err)
			return 1
		}
		fmt.Println("wrote", cfgPath)
	}

	// 1b. Register the project in the global daemon's fail-closed allowlist. A
	// cloned repo shipping its own config.env has no effect until this opt-in.
	if !o.global {
		canon, err := config.CanonicalPath(".")
		if err != nil {
			fmt.Fprintln(os.Stderr, "init: cannot canonicalize project dir:", err)
			return 1
		}
		if preExisting {
			fmt.Printf("WARNING: pre-existing %s is about to become active for the global daemon — review it (a cloned repo may point WORKER_API_BASE at an endpoint you do not trust)\n", cfgPath)
		}
		if err := config.RegisterProject(canon); err != nil {
			fmt.Fprintln(os.Stderr, "init: register project:", err)
			return 1
		}
		fmt.Println("registered", canon, "in", config.ProjectsPath())
	}

	// 2. Claude Code wiring (permissions + MCP pre-approve). No hooks since S4.
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "init:", err)
		return 1
	}
	if err := wireSettings(filepath.Join(claudeDir, "settings.json"), filepath.Join(dataDir, "settings.json.bak")); err != nil {
		fmt.Fprintln(os.Stderr, "init: settings:", err)
		return 1
	}
	fmt.Println("wired permissions in", filepath.Join(claudeDir, "settings.json"))

	// 2b. Workspace trust (#16). Those permissions are ignored outright until
	// Claude Code trusts this directory, so either grant it — on an explicit
	// request only, see trust.go — or say plainly that the headless path is still
	// broken and how to fix it.
	if canon, err := config.CanonicalPath("."); err == nil {
		if o.trustWorkspace {
			already, terr := grantWorkspaceTrust(canon, dataDir)
			switch {
			case terr != nil:
				fmt.Fprintln(os.Stderr, "init: workspace trust:", terr)
			case already:
				fmt.Println("workspace already trusted by Claude Code:", canon)
			default:
				fmt.Printf("granted Claude Code workspace trust for %s in %s (uninstall reverts it)\n", canon, userClaudeJSON())
			}
		} else if notice := untrustedWorkspaceNotice(canon); notice != "" {
			fmt.Println(notice)
		}
	}

	// 3. MCP config for `run --mcp-config` back-compat: the same one server, served
	// as a one-off file. Bare `claude` ignores this; it reads the project-root
	// .mcp.json (local) or the user-scope ~/.claude.json (global).
	mcpPath := filepath.Join(dataDir, "mcp.json")
	if err := os.WriteFile(mcpPath, []byte(renderMCP(o.npxWorker)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "init: mcp:", err)
		return 1
	}
	fmt.Println("wrote MCP config", mcpPath)

	// 4. Auto-wire so a PLAIN `claude` offloads to the worker with no flags.
	//   - local: a project-root .mcp.json CC auto-discovers, pre-approved by
	//     enableAllProjectMcpServers (set in wireSettings) so headless -p never hangs.
	//   - global: register the worker at USER scope in ~/.claude.json (top-level
	//     mcpServers) — loads in EVERY project automatically, no per-project trust
	//     prompt, exactly how Serena follows the user across projects.
	if o.global {
		if err := registerUserMCP(o.npxWorker); err != nil {
			fmt.Fprintln(os.Stderr, "init: user-scope MCP:", err)
			return 1
		}
		fmt.Println("registered worker at user scope in", userClaudeJSON())
	} else {
		rootMCP := filepath.Join(".", ".mcp.json")
		if err := os.WriteFile(rootMCP, []byte(renderMCP(o.npxWorker)), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "init: root mcp:", err)
			return 1
		}
		fmt.Println("wrote auto-discovered MCP config", rootMCP)
	}

	// 4b. Keep rig's own files out of git's view of the user's work. They are
	// wiring, not changes: an untracked .claude/ and .mcp.json otherwise show up
	// in the worker's returned diff as if the worker had authored them (#26), and
	// `git stash -u` — which the proof-retry protocol uses to reach a red state —
	// would sweep away rig's own config mid-run. .git/info/exclude is the right
	// home: it is local and never committed, so rig does not edit a .gitignore the
	// user owns and shares.
	if canon, err := config.CanonicalPath("."); err == nil {
		if added, err := excludeRigArtifacts(canon); err != nil {
			fmt.Fprintln(os.Stderr, "init: git exclude:", err)
		} else if added > 0 {
			fmt.Printf("excluded %d rig path(s) from git in %s\n", added, filepath.Join(".git", "info", "exclude"))
		}
	}

	// 4c. The steer. rig no longer writes an output style: that tier is
	// system-prompt-level and session-global, which is what made it enforcement in
	// the first place — and A1 measured the cost of it leaking, when the worker
	// inherited MAIN's style telling it not to edit files and dutifully tried to
	// delegate its own job. CLAUDE.md is the tier that matches the decision
	// (told, not forced): it is guidance, the user can read and edit it, and it
	// does not overwrite Claude Code's own idea of what it is.
	//
	// Never clobber a user's CLAUDE.md: write only when absent (or already ours).
	memPath := filepath.Join(claudeDir, "CLAUDE.md")
	if existing, err := os.ReadFile(memPath); err != nil {
		if err := os.WriteFile(memPath, []byte(steerMD), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "init: CLAUDE.md:", err)
			return 1
		}
		fmt.Println("wrote steer", memPath)
	} else if strings.Contains(string(existing), steerSentinel) {
		if err := os.WriteFile(memPath, []byte(steerMD), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "init: CLAUDE.md:", err)
			return 1
		}
		fmt.Println("updated steer", memPath)
	} else {
		// Their file, their call — but "add the steer by hand" is not an
		// instruction anybody can act on, and on a machine that already has a
		// CLAUDE.md (a common case, and the default one at global scope) this
		// branch is how an install ends up with no steer at all: MAIN is never
		// told the switch exists. So the steer goes in its own file and the user
		// is given the exact one line that pulls it in — the same @-import their
		// own memory files already use.
		sidePath := filepath.Join(claudeDir, steerImportFile)
		if err := os.WriteFile(sidePath, []byte(steerMD), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "init: steer:", err)
			return 1
		}
		if importedBy(memPath, steerImportFile) {
			fmt.Printf("updated steer %s (imported by your %s)\n", sidePath, memPath)
		} else {
			fmt.Printf("NOTE: %s is yours, so it was left untouched. The steer is in %s —\n"+
				"  add this one line to %s to switch it on:\n\n      @%s\n\n",
				memPath, sidePath, memPath, steerImportFile)
		}
	}

	// 4d. The button. Same one tool underneath, so there is nothing to keep in
	// sync — the command file is a few lines that call it (S1).
	cmdPath := filepath.Join(claudeDir, "commands", "qwen.md")
	if err := os.MkdirAll(filepath.Dir(cmdPath), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "init: commands dir:", err)
		return 1
	}
	if err := os.WriteFile(cmdPath, []byte(qwenCommandMD), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "init: /qwen command:", err)
		return 1
	}
	fmt.Println("wrote /qwen command", cmdPath)

	// 5. OS service (optional): supervise `serve` across reboots.
	if o.service {
		self, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, "init: service:", err)
			return 1
		}
		home, _ := os.UserHomeDir()
		msg, err := service.New(self, home, dataDir).Install()
		if err != nil {
			fmt.Fprintln(os.Stderr, "init: service:", err)
			return 1
		}
		fmt.Println(msg)
	}

	scope := "local (this project)"
	if o.global {
		scope = "global (all projects — follows you)"
	}
	state := "ENABLED (offload active)"
	if !o.enabled {
		state = "DISABLED (Claude Code runs normally; set a worker endpoint + ENABLED=true in config.env to turn it on)"
	}
	fmt.Printf("\ninit complete — scope: %s\nstatus: %s\nlaunch with:  claude\n", scope, state)
	return 0
}

type configEnvVals struct {
	backend, workerBase, workerModel, workerKey, mainUpstream, port string
	ccBase, ccModel                                                 string
	enabled                                                         bool
}

func renderConfigEnv(v configEnvVals) string {
	var b strings.Builder
	b.WriteString("# rig-move-llm config — bring-your-own worker endpoint.\n")
	b.WriteString("# Precedence: process env > local config.env > global config.env.\n\n")
	kv := func(comment, key, val string) {
		if comment != "" {
			b.WriteString("# " + comment + "\n")
		}
		if val == "" {
			b.WriteString("# " + key + "=\n")
		} else {
			b.WriteString(key + "=" + val + "\n")
		}
	}
	kv("worker backend (ollama|llamacpp|tabby|openrouter|openai|generic); sets a default base URL", "WORKER_BACKEND", v.backend)
	kv("worker OpenAI-compatible endpoint; overrides the backend default", "WORKER_API_BASE", v.workerBase)
	kv("worker model name", "WORKER_MODEL", v.workerModel)
	kv("worker API key (optional for local models; use an OpenRouter key for OpenRouter)", "WORKER_API_KEY", v.workerKey)
	b.WriteString("\n")
	kv("Anthropic-format base URL the switch points `claude -p` at — REQUIRED (it keeps the worker leg off the paid account; the switch refuses to run without it)", "RIG_CC_BASE_URL", v.ccBase)
	kv("model name the subprocess runs as (default haiku — the worker-leg routing key on the shim)", "RIG_CC_MODEL", v.ccModel)
	b.WriteString("\n")
	// Master on/off. Written explicitly so the state is unambiguous: false = wired
	// but inert (Claude Code runs normally), flip to true after setting an endpoint.
	enabled := "false"
	if v.enabled {
		enabled = "true"
	}
	kv("master switch: true = offload active; false = Claude Code runs normally. Skipping the worker in setup leaves this false.", "ENABLED", enabled)
	kv("paid main-leg upstream (raw passthrough, OAuth untouched)", "MAIN_UPSTREAM_URL", v.mainUpstream)
	kv("proxy listen port", "PORT", v.port)
	b.WriteString("\n")
	kv("worker health-check path probed at each message start (default /v1/models; set off to disable — call-time fallback still applies)", "WORKER_HEALTH_PATH", "")
	kv("health probe timeout in ms (default 2000)", "WORKER_HEALTH_TIMEOUT_MS", "")
	kv("reuse a health probe result for this many seconds (default 15)", "WORKER_HEALTH_CACHE_SEC", "")
	b.WriteString("\n")
	kv("set LOG_BODIES=1 to log full request/response bodies (default: metadata only)", "LOG_BODIES", "")
	kv("size cap in MB for logs/requests.jsonl; past it the oldest half is compacted away (default 50)", "LOG_MAX_MB", "")
	kv("MCP servers the MAIN agent may still use, comma-separated (default: none)", "MAIN_SHARED_MCP", "")
	return b.String()
}

// workerMCPEntry is the worker server definition for an mcp config. When npx is
// true it is spawned via `npx -y rig-move-llm worker` (zero global install — npx
// resolves the published package each spawn); otherwise via the `rig-move-llm`
// binary on PATH (a global npm/binary install).
// The entry carries an explicit per-call `timeout` (#33). Two reasons, both
// measured: it names the client-side wall instead of inheriting the ~28-hour
// default, and on Claude Code v2.1.203+ a per-server timeout is also a FLOOR on
// the idle timeout — without it a stdio server that answers only at the end of a
// long round (which is exactly what the worker does) is aborted after 30 minutes
// of "idleness" while it is in fact working. rig sizes it above its own wall guard
// so the run is always the one that kills itself and can return a diagnosis and the
// partial diff.
func workerMCPEntry(npx bool) map[string]any {
	entry := map[string]any{"type": "stdio", "command": "rig-move-llm", "args": []string{"thin-worker"}}
	if npx {
		entry = map[string]any{"type": "stdio", "command": "npx", "args": []string{"-y", "rig-move-llm", "thin-worker"}}
	}
	entry["timeout"] = int(thin.ClientCallTimeout() / time.Millisecond)
	return entry
}

// renderMCP builds the CC-side .mcp.json. rig-move injects ONLY its own `worker`
// server — never the user's other MCPs. Knowledge and SOFA-search are deliberately
// absent: they are violin-native capabilities served behind the worker's generic
// OpenAI endpoint (server-side enrichment), not MCP tools CC or the worker sees.
// See map5 (local-enrichment) — enrichment moved server-side so the OSS client
// stays a plain OpenAI client with no coupling to our compute.
func renderMCP(npx bool) string {
	servers := map[string]any{
		// The one server. CC spawns it on stdio and calls its `implement` tool,
		// which runs `claude -p` against the configured endpoint — guaranteed
		// egress, independent of CC's in-process agent runtime (ticket P9).
		"worker": workerMCPEntry(npx),
	}
	out, _ := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	return string(out) + "\n"
}

// userClaudeJSON returns ~/.claude.json, the user-scope config where a top-level
// `mcpServers` entry loads in every project (how Serena registers globally).
func userClaudeJSON() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude.json")
}

// registerUserMCP merges the worker server into the top-level `mcpServers` of
// ~/.claude.json, preserving every other key and server. This is the global
// "follows you" registration: user-scope MCP servers load in all projects with
// no per-project .mcp.json and no trust prompt.
func registerUserMCP(npx bool) error {
	path := userClaudeJSON()
	root := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &root)
	}
	servers, _ := root["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	servers["worker"] = workerMCPEntry(npx)
	root["mcpServers"] = servers
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

// unregisterUserMCP removes the worker server from ~/.claude.json's top-level
// mcpServers (uninstall of a global scope), leaving everything else intact.
func unregisterUserMCP() {
	path := userClaudeJSON()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	root := map[string]any{}
	if json.Unmarshal(data, &root) != nil {
		return
	}
	if servers, ok := root["mcpServers"].(map[string]any); ok {
		delete(servers, "worker")
		if len(servers) == 0 {
			delete(root, "mcpServers")
		}
	}
	if out, err := json.MarshalIndent(root, "", "  "); err == nil {
		_ = os.WriteFile(path, append(out, '\n'), 0o644)
	}
}

func modelNote(model string) string {
	if model == "" {
		return " (no model listed — set WORKER_MODEL)"
	}
	return " model=" + model
}

// workerToolPermission is the permission rule pre-granting the worker MCP tool.
// enableAllProjectMcpServers covers server TRUST only; without this grant a
// headless `claude -p` burns the run asking a human to click allow (#6).
const workerToolPermission = "mcp__worker__implement"

// wireSettings grants the switch's MCP tool in an existing (or new) Claude Code
// settings.json, preserving unrelated keys. The original file is backed up once to
// backupPath so `uninstall` can restore it verbatim.
//
// It no longer writes hooks. rig used to install four (PreToolUse denying MAIN's
// edit tools, PostToolUse gating the return, SessionStart, UserPromptSubmit) —
// all of them served the contract layer, and all of them called `rig hook`, a
// subcommand that no longer exists. The switch is something Claude is TOLD to
// use, not forced to.
func wireSettings(path, backupPath string) error {
	settings := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &settings)
		if !fileExists(backupPath) {
			_ = os.WriteFile(backupPath, data, 0o644)
		}
	}

	// Pre-approve the project-root .mcp.json server so headless `claude -p` does not
	// hang on the MCP trust dialog (see memory cc-persistent-autowire-recipe).
	settings["enableAllProjectMcpServers"] = true

	// Trust alone is not permission (#6): a headless -p run still stalls on the
	// tool-permission dialog for the worker tool. Grant it here, preserving any
	// user-managed permissions around it.
	perms, _ := settings["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
	}
	allow, _ := perms["allow"].([]any)
	granted := false
	for _, v := range allow {
		if v == workerToolPermission {
			granted = true
			break
		}
	}
	if !granted {
		allow = append(allow, workerToolPermission)
	}
	perms["allow"] = allow
	settings["permissions"] = perms

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

// steerImportFile is where the steer goes when the user owns their CLAUDE.md.
// Claude Code resolves `@name.md` inside a memory file, so one line pulls it in
// and deleting that line switches it off without touching anything else.
const steerImportFile = "rig-move-llm.md"

// importedBy reports whether the memory file already pulls in name via @import,
// so a re-run says "updated" instead of repeating instructions already followed.
func importedBy(memPath, name string) bool {
	data, err := os.ReadFile(memPath)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "@"+name {
			return true
		}
	}
	return false
}

// steerSentinel marks a file as rig-move-llm-authored so uninstall can remove it
// without touching a user's own memory file. The string is unchanged from the
// enforcement era on purpose: an install made by an older rig carries it too, and
// uninstall has to recognise those as ours.
const steerSentinel = "<!-- rig-move-llm:delegate-steer -->"

// steerMD is what rig tells Claude, and the whole of it.
//
// Two things shape this text, and both were paid for:
//
// The first paragraph exists because THIS FILE IS ALSO READ BY THE WORKER. rig's
// steer lands in the user's project, and the worker runs `claude -p` in that same
// project, where CLAUDE.md is auto-discovered. The old steer said "Delegate ALL
// code changes; do not edit files yourself" — A1 measured the worker reading that
// and trying to hand its own job to a subagent in 10–23% of runs. So the file
// disarms itself against the one fact that separates the two readers: MAIN has
// the implement tool, the worker does not (--strict-mcp-config leaves it with no
// MCP servers at all — verified in the init event of every live run).
//
// The rest is guidance rather than prohibition because the enforcement it used to
// describe is gone (S4), and because the reason for enforcing was a pipeline with
// nobody watching. There is somebody watching now.
const steerMD = steerSentinel + `
# Delegating implementation in this repo

**If you do not have a tool called ` + "`mcp__worker__implement`" + `, stop here — the rest of this
file is not about you.** You are the one doing the work: read and edit the files
yourself and run this repo's own tests.

If you do have it, then implementation is cheaper somewhere else. Your inference is
paid for; the model behind that tool runs on a local or cheap endpoint. So the split
worth defaulting to is:

- **Think here.** Understand the task, pick the approach, and scope the change down
  to something one pass can finish.
- **Hand the doing over.** Call ` + "`mcp__worker__implement`" + ` with the task in plain language,
  with enough detail to act on without coming back to ask. It works in the repo with
  the full Claude Code harness: it reads, edits, and runs the tests.
- **Pass the result on.** It returns a status line, a path to its run log, the diff,
  and the last command it ran. Show the diff. You are not being asked to certify it —
  a human reads it and decides.

Nothing prevents you from editing files yourself, and sometimes you should: a one-line
change you are already sure of is not worth a round trip. This is a default, not a
rule, and "this task is a bad fit for the worker" is a fine thing to say out loud.
`

// qwenCommandMD is the direct button. It calls the same one tool the steer names,
// so there are not two surfaces to keep in sync (S1).
//
// It leaks into the worker's own slash_commands list — every one of the user's
// commands does, verified in the init event — but a slash command is inert until
// somebody types it, and nobody types this one inside a worker session.
const qwenCommandMD = steerSentinel + `
---
description: Hand a task to the local worker model and show me the diff
---

Call ` + "`mcp__worker__implement`" + ` with the task below, then show me the diff it returns.

Do not implement it yourself, and do not re-run or re-verify what it did — I read the
diff and decide. If the task is too vague to act on, say so instead of guessing.

Task: $ARGUMENTS
`
