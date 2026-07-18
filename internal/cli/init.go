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

// cmdInit bootstraps a scope: it writes the config file and wires Claude Code
// (hooks + permissions + worker subagent + MCP toolbelt) so that `rig-move-llm
// run -- claude` launches a working hybrid. Local (default) touches only this
// project; --global touches ~/.claude and applies to every project.
func cmdInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	global := fs.Bool("global", false, "install for all projects (~/.claude + ~/.rig-move-llm)")
	backend := fs.String("backend", "", "worker backend: "+strings.Join(config.BackendNames(), "|"))
	workerBase := fs.String("worker-base", "", "worker OpenAI-compatible base URL (e.g. http://localhost:11434/v1)")
	workerModel := fs.String("worker-model", "", "worker model name")
	workerKey := fs.String("worker-key", "", "worker API key (optional for local models)")
	mainUpstream := fs.String("main-upstream", "https://api.anthropic.com", "paid (main-leg) upstream")
	port := fs.String("port", "4000", "proxy listen port")
	knowledgeURL := fs.String("knowledge-url", "", "optional knowledge MCP SSE URL")
	searchURL := fs.String("search-url", "", "optional search MCP SSE URL")
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

	dataDir := config.LocalDir()
	claudeDir := filepath.Join(".", ".claude")
	if *global {
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
	if preExisting && !*force {
		fmt.Printf("config exists: %s (use --force to overwrite)\n", cfgPath)
	} else {
		if err := os.WriteFile(cfgPath, []byte(renderConfigEnv(configEnvVals{
			backend: *backend, workerBase: *workerBase, workerModel: *workerModel,
			workerKey: *workerKey, mainUpstream: *mainUpstream, port: *port,
			knowledgeURL: *knowledgeURL, searchURL: *searchURL,
		})), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "init: write config:", err)
			return 1
		}
		fmt.Println("wrote", cfgPath)
	}

	// 1b. Register the project in the global daemon's fail-closed allowlist. A
	// cloned repo shipping its own config.env has no effect until this opt-in.
	if !*global {
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

	// 2. Claude Code wiring (hooks + permissions).
	if err := os.MkdirAll(filepath.Join(claudeDir, "agents"), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "init:", err)
		return 1
	}
	if err := wireSettings(filepath.Join(claudeDir, "settings.json"), filepath.Join(dataDir, "settings.json.bak")); err != nil {
		fmt.Fprintln(os.Stderr, "init: settings:", err)
		return 1
	}
	fmt.Println("wired hooks + permissions in", filepath.Join(claudeDir, "settings.json"))

	// 3. Worker subagent.
	agentPath := filepath.Join(claudeDir, "agents", "rig-worker.md")
	if err := os.WriteFile(agentPath, []byte(workerAgentMD), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "init: agent:", err)
		return 1
	}
	fmt.Println("wrote worker subagent", agentPath)

	// 4. MCP toolbelt (worker-side knowledge/search) — only if configured.
	if *knowledgeURL != "" || *searchURL != "" {
		mcpPath := filepath.Join(dataDir, "mcp.json")
		if err := os.WriteFile(mcpPath, []byte(renderMCP(*knowledgeURL, *searchURL)), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "init: mcp:", err)
			return 1
		}
		fmt.Println("wrote MCP toolbelt", mcpPath)
	}

	// 5. OS service (optional): supervise `serve` across reboots.
	if *svc {
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
	if *global {
		scope = "global (all projects)"
	}
	fmt.Printf("\ninit complete — scope: %s\nlaunch with:  rig-move-llm run -- claude\n", scope)
	return 0
}

type configEnvVals struct {
	backend, workerBase, workerModel, workerKey, mainUpstream, port string
	knowledgeURL, searchURL                                         string
}

func renderConfigEnv(v configEnvVals) string {
	var b strings.Builder
	b.WriteString("# rig-move-llm config — bring-your-own worker endpoint.\n")
	b.WriteString("# Precedence: process env > local config.env > global config.env.\n")
	b.WriteString("# Multi-endpoint fallback: create workers.json next to this file (chmod 0600 —\n")
	b.WriteString("# it may hold API keys, so it is never auto-created). It replaces the WORKER_*\n")
	b.WriteString("# values below with a priority chain; see README \"Fallback chain\".\n\n")
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
	kv("paid main-leg upstream (raw passthrough, OAuth untouched)", "MAIN_UPSTREAM_URL", v.mainUpstream)
	kv("proxy listen port", "PORT", v.port)
	b.WriteString("\n")
	kv("set LOG_BODIES=1 to log full request/response bodies (default: metadata only)", "LOG_BODIES", "")
	kv("size cap in MB for logs/requests.jsonl; past it the oldest half is compacted away (default 50)", "LOG_MAX_MB", "")
	kv("MCP servers the MAIN agent may still use, comma-separated (default: none)", "MAIN_SHARED_MCP", "")
	kv("target % of worker-tier tokens served by your worker over a sliding 15 min window; 1-99 diverts the excess to the paid upstream (burns quota, logged routed=diverted); default 100 = always your worker", "CUSTOM_SUBAGENT_USAGE", "")
	return b.String()
}

func renderMCP(knowledgeURL, searchURL string) string {
	servers := map[string]any{}
	if knowledgeURL != "" {
		servers["knowledge"] = map[string]string{"type": "sse", "url": knowledgeURL}
	}
	if searchURL != "" {
		servers["search"] = map[string]string{"type": "sse", "url": searchURL}
	}
	out, _ := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	return string(out) + "\n"
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
func wireSettings(path, backupPath string) error {
	settings := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &settings)
		if !fileExists(backupPath) {
			_ = os.WriteFile(backupPath, data, 0o644)
		}
	}

	settings["hooks"] = map[string]any{
		"PreToolUse": []any{map[string]any{
			"matcher": "*",
			"hooks":   []any{map[string]any{"type": "command", "command": "rig-move-llm hook pre-tool"}},
		}},
		"PostToolUse": []any{map[string]any{
			"matcher": "Task|Agent",
			"hooks":   []any{map[string]any{"type": "command", "command": "rig-move-llm hook post-tool", "timeout": 600}},
		}},
	}

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

const workerAgentMD = `---
name: rig-worker
description: Heavy-lifting code worker. Runs on your worker model (free/cheap). The MAIN agent delegates every code change, file edit, test run, and knowledge lookup here; it edits files, consults knowledge/search when available, and MUST run the project's real tests as its own gate before returning.
tools: Read, Edit, Write, Bash, Grep, Glob, mcp__knowledge, mcp__search
---

You are the worker in a subscription-preserving hybrid. Your inference runs on the
user's worker model; your tools execute natively on this repo. The MAIN agent has
planned and delegated a scoped task to you.

Do the work end to end: read what you need, make the edits, and **run the project's
real tests as your own gate before returning**. Report concisely what you changed and
the test result. If a knowledge/search MCP is configured, use it before guessing.
`
