package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/internal/service"
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
	service      bool
	force        bool
	noDetect     bool
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
	force := fs.Bool("force", false, "overwrite an existing config file")
	noDetect := fs.Bool("no-detect", false, "skip probing for a local worker endpoint")
	svc := fs.Bool("service", false, "install an OS service so the proxy survives reboots (requires --global)")
	_ = fs.Parse(args)

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
		enabled: *workerBase != "" || *backend != "",
		service: *svc, force: *force, noDetect: *noDetect,
	})
}

// applyInit performs the actual bootstrap for a resolved initOpts.
func applyInit(o initOpts) int {
	dataDir := config.LocalDir()
	if o.global {
		dataDir = config.GlobalDir()
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

// userClaudeJSON returns ~/.claude.json, the user-scope config where a top-level
// `mcpServers` entry loads in every project (how Serena registers globally).
func userClaudeJSON() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude.json")
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

// steerImportFile is where the steer goes when the user owns their CLAUDE.md.
// Claude Code resolves `@name.md` inside a memory file, so one line pulls it in
// and deleting that line switches it off without touching anything else.
const steerImportFile = "rig-move-llm.md"

// steerSentinel marks a file as rig-move-llm-authored. Nothing writes a steer any
// more — the switch has no tool to point Claude at — but a machine that ran an
// older rig has one on disk, and uninstall is the only thing that removes it.
const steerSentinel = "<!-- rig-move-llm:delegate-steer -->"

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

// qwenCommandMD is the direct button. It calls the same one tool the steer names,
// so there are not two surfaces to keep in sync (S1).
//
// It leaks into the worker's own slash_commands list — every one of the user's
// commands does, verified in the init event — but a slash command is inert until
// somebody types it, and nobody types this one inside a worker session.
