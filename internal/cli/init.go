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
	enabled      bool // ENABLED written to config; false = wired but inert (Claude Code runs normally)
	npxWorker    bool // spawn the worker MCP as `npx -y rig-move-llm worker` (zero global install)
	service      bool
	force        bool
	noDetect     bool
}

// cmdInit bootstraps a scope: it writes the config file and wires Claude Code
// (hooks + permissions + worker MCP + output style) so that a plain `claude`
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
	npx := fs.Bool("npx", false, "spawn the worker via `npx -y rig-move-llm worker` (no global binary needed)")
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
		port: *port,
		// A worker endpoint was configured -> enable; otherwise stay inert.
		enabled:   *workerBase != "" || *backend != "",
		npxWorker: *npx, service: *svc, force: *force, noDetect: *noDetect,
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

	// 2. Claude Code wiring (hooks + permissions + session-start auto-materialize).
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "init:", err)
		return 1
	}
	if err := wireSettings(filepath.Join(claudeDir, "settings.json"), filepath.Join(dataDir, "settings.json.bak"), config.Load().GateMode); err != nil {
		fmt.Fprintln(os.Stderr, "init: settings:", err)
		return 1
	}
	fmt.Println("wired hooks + permissions in", filepath.Join(claudeDir, "settings.json"))

	// 3. MCP config for `run --mcp-config` back-compat: the same worker (+optional
	// toolbelt) served as a one-off file. Bare `claude` ignores this; it reads the
	// project-root .mcp.json (local) or the user-scope ~/.claude.json (global).
	mcpPath := filepath.Join(dataDir, "mcp.json")
	if err := os.WriteFile(mcpPath, []byte(renderMCP(o.npxWorker)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "init: mcp:", err)
		return 1
	}
	fmt.Println("wrote MCP config (worker + toolbelt)", mcpPath)

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

	// 4d. Output style = the persistent, SYSTEM-PROMPT-tier terse-delegate workflow
	// (no-flag equivalent of P9's --append-system-prompt). wireSettings activates it.
	stylePath := filepath.Join(claudeDir, "output-styles", "rig-delegate.md")
	if err := os.MkdirAll(filepath.Dir(stylePath), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "init: output-styles dir:", err)
		return 1
	}
	if err := os.WriteFile(stylePath, []byte(outputStyleMD), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "init: output style:", err)
		return 1
	}
	// The soft-gate (explore-first) sibling style is always written too; which one
	// is ACTIVE is decided by wireSettings from GATE_MODE, so flipping the mode is
	// a config change + settings rewire, not a reinstall.
	explorePath := filepath.Join(claudeDir, "output-styles", "rig-explore.md")
	if err := os.WriteFile(explorePath, []byte(exploreStyleMD), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "init: output style:", err)
		return 1
	}
	fmt.Println("wrote output styles", stylePath, explorePath)

	// 4c. Delegate-only steer (guidance, not enforcement). Never clobber a user's
	// CLAUDE.md: write only when absent (or already ours).
	memPath := filepath.Join(claudeDir, "CLAUDE.md")
	if existing, err := os.ReadFile(memPath); err != nil {
		if err := os.WriteFile(memPath, []byte(delegateSteerMD), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "init: CLAUDE.md:", err)
			return 1
		}
		fmt.Println("wrote delegate steer", memPath)
	} else if strings.Contains(string(existing), steerSentinel) {
		fmt.Println("delegate steer already present in", memPath)
	} else {
		fmt.Printf("NOTE: %s exists — add the delegate steer manually or see .claude/CLAUDE.md guidance (leaving it untouched)\n", memPath)
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
	// Master on/off. Written explicitly so the state is unambiguous: false = wired
	// but inert (Claude Code runs normally), flip to true after setting an endpoint.
	enabled := "false"
	if v.enabled {
		enabled = "true"
	}
	kv("master switch: true = offload active; false = Claude Code runs normally (no force-delegate). Skipping the worker in setup leaves this false.", "ENABLED", enabled)
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
func workerMCPEntry(npx bool) map[string]any {
	if npx {
		return map[string]any{"type": "stdio", "command": "npx", "args": []string{"-y", "rig-move-llm", "worker"}}
	}
	return map[string]any{"type": "stdio", "command": "rig-move-llm", "args": []string{"worker"}}
}

// renderMCP builds the CC-side .mcp.json. rig-move injects ONLY its own `worker`
// server — never the user's other MCPs. Knowledge and SOFA-search are deliberately
// absent: they are violin-native capabilities served behind the worker's generic
// OpenAI endpoint (server-side enrichment), not MCP tools CC or the worker sees.
// See map5 (local-enrichment) — enrichment moved server-side so the OSS client
// stays a plain OpenAI client with no coupling to our compute.
func renderMCP(npx bool) string {
	servers := map[string]any{
		// The worker MCP server is the Option-2 offload mechanism: CC spawns it on
		// stdio and calls its `implement` tool, whose agentic loop runs on the
		// configured worker endpoint (guaranteed egress, independent of CC's
		// in-process agent runtime — see ticket P9).
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

// wireSettings merges the rig-move-llm hooks into an existing (or new) Claude Code
// settings.json, preserving unrelated keys. The original file is backed up once to
// backupPath so `uninstall` can restore it verbatim.
func wireSettings(path, backupPath, gateMode string) error {
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

	// Activate the output style — the system-prompt-tier workflow lever (P10).
	// hard gate = terse plan→delegate→review; soft gate (map6) = explore-first:
	// Stage-0 explore → triage solo|delegate → act. Loaded by bare `claude` at
	// session start; no CLI flag.
	if gateMode == "soft" {
		settings["outputStyle"] = "rig-explore"
	} else {
		settings["outputStyle"] = "rig-delegate"
	}

	settings["hooks"] = map[string]any{
		"PreToolUse": []any{map[string]any{
			"matcher": "*",
			"hooks":   []any{map[string]any{"type": "command", "command": "rig-move-llm hook pre-tool"}},
		}},
		"PostToolUse": []any{map[string]any{
			// Gate on the worker MCP tool return (Option 2) and on legacy/teammate
			// Task/Agent returns.
			"matcher": "Task|Agent|mcp__worker__implement",
			"hooks":   []any{map[string]any{"type": "command", "command": "rig-move-llm hook post-tool", "timeout": 600}},
		}},
		// SessionStart: lazily materialize a per-project .rig-move-llm/ carrying the
		// configured settings, the way Serena creates .serena on first session. Runs
		// on a new session or a resume; context-only, never blocks.
		"SessionStart": []any{map[string]any{
			"matcher": "startup|resume",
			"hooks":   []any{map[string]any{"type": "command", "command": "rig-move-llm hook session-start"}},
		}},
		// UserPromptSubmit: probe the worker endpoint once per message (zero-token
		// HTTP GET). An unreachable worker flips the per-tool hooks to passthrough so
		// the hybrid degrades to plain Claude Code automatically. Never blocks.
		"UserPromptSubmit": []any{map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": "rig-move-llm hook user-prompt"}},
		}},
	}

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

// steerSentinel marks a CLAUDE.md as rig-move-llm-authored so uninstall can remove
// it without touching a user's own memory file.
const steerSentinel = "<!-- rig-move-llm:delegate-steer -->"

// outputStyleMD is the terse plan→delegate→review workflow, ported from the proven
// hybrid-shim --append-system-prompt (which measured −30% billed output vs solo) to
// reference the mcp__worker__implement offload tool. keep-coding-instructions: true
// makes it APPEND to CC's default system prompt (append-equivalent), the closest
// persistent match to that flag. The force-delegate hook remains the hard enforcer;
// this recovers the savings the hook alone does not (it curbs MAIN verbosity).
const outputStyleMD = `---
name: rig-delegate
description: Terse plan -> delegate to worker -> review orchestrator for the subscription-preserving hybrid.
keep-coding-instructions: true
---

You are the MAIN agent in a subscription-preserving hybrid. Your inference is paid;
the worker (a local/cheap model behind the mcp__worker__implement tool) is free. Your
job is ONLY:

1. PLAN — understand the task and outline the approach in 1-3 terse sentences.
2. DELEGATE — hand ALL code changes, file edits, command/test runs, and knowledge/
   search lookups to the worker by calling the mcp__worker__implement tool with a
   scoped spec. Do NOT edit files or run commands yourself — those tools are blocked
   for you by design (a PreToolUse hook denies them). If you catch yourself about to
   edit or run a test, stop and delegate instead.
3. REVIEW — read the worker's summary and the gate result. Before closing, confirm the
   change introduced no NEW test failure: the worker must have run the full test file(s)
   covering the code it touched and reported them green (not just the one new/target test).
   If any pre-existing test now fails because of the change (common when the fix constrains
   behaviour an existing test relied on), that is fallout — re-delegate to fix it. This
   regression check is NOT optional, even when terse. When it is clean, reply with ONLY a
   short closing line (files changed + outcome); claim nothing more. You may Read/Grep/Glob
   to plan and review.

Be terse in every message: a brief plan, the delegation, a brief review. No verbose
explanations, no restating the task, no narrating what you are about to do. Prefer
delegating on the first try to avoid wasted round-trips.
`

// exploreStyleMD is the soft-gate (map6) workflow: offload the READING to the
// free worker first, decide solo|delegate on grounded evidence, and use the
// bounded solo/repair windows the hook opens instead of paying delegation
// round-trips for tiny changes.
const exploreStyleMD = `---
name: rig-explore
description: Explore-first cost-aware orchestrator - Stage-0 worker explore, evidence-based triage, solo tiny edits or delegate.
keep-coding-instructions: true
---

You are the MAIN agent in a subscription-preserving hybrid. Your inference is paid;
the worker (a local/cheap model behind the mcp__worker__* tools) is free. Reading the
repo yourself is the biggest cost sink — offload it. Workflow, in order:

1. EXPLORE (Stage 0, free) — for any task that touches the repo, FIRST call
   mcp__worker__explore ONCE with the task. It returns machine-verified evidence:
   relevant files, candidate edit sites (path:line + verbatim snippets checked
   against disk), entrypoints/repro, and declared blind spots. It runs to
   completion in that single call (rig explores large repos in internal rounds),
   so you do not loop it. Do not pre-read the repo yourself; at most spot-check
   2-3 of the returned citations with Read.
2. TRIAGE (Gate A) — call mcp__worker__triage with decision "solo" or "delegate"
   plus a one-line reason citing the evidence. solo = ONE obvious small edit at a
   verified site, no open questions. delegate = multi-file, needs investigation or
   repro, ambiguous, or ANY uncertainty. The declaration is checked against the
   evidence and may be overridden to delegate — accept the effective decision.
3. ACT —
   - solo: make the small edit yourself at the verified site (the hook allows only
     the grounded files, size-bounded), then verify via the worker or close.
   - delegate: call mcp__worker__implement with a scoped spec that INCLUDES the
     Stage-0 evidence (files, sites, repro) so the worker does not re-explore.
4. REVIEW — read the worker's summary, diff, and gate result. Confirm no NEW test
   failure (the worker must have run the full affected test file(s), not just the
   target test); hit_iteration_cap=true means extra scrutiny. If the returned work
   has a TINY residue (typo-scale, a couple of lines), patch it directly — a short
   repair window is open after each worker return. Anything bigger: re-delegate.

Be terse in every message: brief plan, the tool calls, a brief review, a one-line
close (files changed + outcome). No verbose explanations, no restating the task.
`

const delegateSteerMD = steerSentinel + `
# Delegation

This project runs a subscription-preserving hybrid: heavy code work is offloaded to
a local/cheap worker model, keeping the paid main-leg budget for planning and review.

**Delegate ALL code implementation to the ` + "`mcp__worker__implement`" + ` tool.** Do not edit
files, run tests, or run shell commands yourself — hand the scoped task to the worker
tool, which edits files, runs the project's real tests as its gate, and reports back.
Plan and review here; implement there. (A PreToolUse hook enforces this — delegating
on the first try just avoids the deny round-trip.)
`
