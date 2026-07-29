package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Cheevatech/rig-move-llm/internal/cli/tui"
	"github.com/Cheevatech/rig-move-llm/internal/config"
)

// cmdSetup is the interactive, guided install: a plain `rig-move-llm` (no args)
// or `rig-move-llm setup` walks the user through scope, worker endpoint (which is
// skippable — skipping installs everything inert so Claude Code runs normally),
// and then wires Claude Code end to end. Choices use a colored arrow/space-select
// TUI (internal/cli/tui — hand-rolled stdlib raw mode, no framework, single static
// binary); free-text values use colored line prompts. Every step explains its
// options so the user knows what to pick, and on a non-TTY/`-p` run the TUI degrades
// to numbered line prompts automatically.
func cmdSetup(args []string) int {
	tui.Banner()

	o := initOpts{mainUpstream: "https://api.anthropic.com", port: "4000", force: true}

	// 1. Scope. Global "follows you" across every project (like Serena); project
	//    scope touches only this directory.
	scope := tui.Select("Scope — where should the hybrid apply?", []tui.Option{
		{Label: "global", Description: "install once for every project (follows you)", Recommended: true},
		{Label: "project", Description: "wire only this directory"},
	}, 0)
	if scope < 0 {
		fmt.Println("setup cancelled.")
		return 1
	}
	o.global = scope == 0

	// 2. Worker endpoint — SKIPPABLE. Auto-detect a local one first. Skipping installs
	//    everything inert (ENABLED=false) so Claude Code runs exactly as normal; the
	//    user can turn it on later with `rig-move-llm enable`.
	fmt.Println()
	fmt.Println("Worker endpoint — where the offloaded code work runs (your own local model or")
	fmt.Println("any OpenAI-compatible API). You can SKIP it: rig installs but stays OFF, so Claude")
	fmt.Println("Code runs exactly as normal. Turn it on later with `rig-move-llm enable`.")
	if d, ok := detectWorker(); ok {
		if tui.Confirm(fmt.Sprintf("Found a local worker: %s at %s%s — use it?", d.Backend, d.Base, modelNote(d.Model)),
			"offload code work to this endpoint", "ignore it and set one manually (or skip)", true) {
			o.backend, o.workerBase, o.workerModel = d.Backend, d.Base, d.Model
		}
	}
	if o.workerBase == "" {
		o.workerBase = tui.Prompt("worker base URL (Enter to SKIP — install OFF)",
			"OpenAI-compatible endpoint, e.g. http://localhost:11434/v1", "")
		if o.workerBase != "" {
			o.backend = pickBackend()
			o.workerModel = tui.Prompt("worker model name", "the model the worker sends to that endpoint", o.workerModel)
			o.workerKey = tui.Prompt("worker API key", "optional for local models; Enter to skip", "")
		}
	}
	o.enabled = o.workerBase != ""

	// 2b. Worker engine — HOW the offloaded work runs. Only asked when a worker
	// endpoint exists; the default stays the built-in loop (map10: cc flips to
	// default only after the P4 evidence gate passes, never because of lab numbers).
	if o.workerBase != "" {
		fmt.Println()
		eng := tui.Select("Worker engine — how should the worker execute?", []tui.Option{
			{Label: "loop", Description: "built-in 3-tool loop (no extra dependencies)", Recommended: true},
			{Label: "cc", Description: "native `claude -p` subprocess on your endpoint (experimental; needs the claude CLI + an Anthropic-format endpoint)"},
		}, 0)
		if eng == 1 {
			// The Anthropic-format endpoint the cc engine needs ships in the product
			// itself: `rig-move-llm serve` exposes /r/worker, translating Anthropic
			// /v1/messages to the worker endpoint configured above (#13 / map13 A4
			// MAJOR-2 — an empty default here was a dead end for anyone who did not
			// already know that).
			base := tui.Prompt("cc base URL",
				"Anthropic-format endpoint for the worker model. Enter = rig's own serve route (run `rig-move-llm serve` before launching claude) — it translates to the worker endpoint above. Type `loop` to keep the loop engine",
				ccDefaultBase(o.port))
			if base == "" || strings.EqualFold(base, "loop") {
				fmt.Println("  keeping the loop engine (the cc engine refuses to run without a base URL; it would bill the worker leg to your paid account).")
			} else {
				o.workerEngine = "cc"
				o.ccBase = base
				o.ccModel = tui.Prompt("cc model name", "model the subprocess runs as", "haiku")
			}
		}
	}

	// 3. Make the binary permanent. The hooks invoke `rig-move-llm ...`, so it must
	//    stay on PATH — but `npx rig-move-llm` runs transiently. Install it globally
	//    now (that is what makes this a single command). Skipped when it is already
	//    a real global install (not the npx cache).
	if !globallyInstalled() {
		fmt.Println()
		if tui.Confirm("Install rig-move-llm globally now? (the hooks call `rig-move-llm`, so it must stay on your PATH)",
			"run `npm install -g rig-move-llm`", "skip — I'll install it myself before launching claude", true) {
			c := exec.Command("npm", "install", "-g", "rig-move-llm")
			c.Stdout, c.Stderr = os.Stdout, os.Stderr
			if err := c.Run(); err != nil {
				fmt.Println("  npm install failed — run `npm install -g rig-move-llm` yourself, then re-run setup.")
			} else {
				fmt.Println("  installed rig-move-llm globally.")
			}
		} else {
			fmt.Println("  skipped — run `npm install -g rig-move-llm` before launching claude.")
		}
	}

	// 4. Wire Claude Code.
	fmt.Println()
	if rc := applyInit(o); rc != 0 {
		return rc
	}
	fmt.Println()
	fmt.Println("Done. Just run:  claude")
	return 0
}

// ccDefaultBase is the wizard's suggested RIG_CC_BASE_URL: this install's own
// serve route, which needs no external Anthropic-format dependency.
func ccDefaultBase(port string) string {
	if port == "" {
		port = "4000"
	}
	return "http://localhost:" + port + "/r/worker"
}

// pickBackend presents the known worker backends as an explained menu, defaulting to
// "generic" (any OpenAI-compatible endpoint). It returns the chosen backend name.
func pickBackend() string {
	desc := map[string]string{
		"ollama":     "local Ollama daemon",
		"llamacpp":   "llama.cpp server",
		"tabby":      "ExLlama via TabbyAPI",
		"openrouter": "OpenRouter hosted models",
		"openai":     "OpenAI API",
		"generic":    "any other OpenAI-compatible endpoint",
	}
	names := config.BackendNames()
	opts := make([]tui.Option, len(names))
	def := 0
	for i, n := range names {
		opts[i] = tui.Option{Label: n, Description: desc[n]}
		if n == "generic" {
			opts[i].Recommended = true
			def = i
		}
	}
	if i := tui.Select("Backend — which endpoint family?", opts, def); i >= 0 {
		return names[i]
	}
	return "generic"
}

// globallyInstalled reports whether rig-move-llm lives in the npm global bin dir
// (a real `npm i -g`), as opposed to the transient `npx` cache. It lets a
// `npx rig-move-llm` run offer to make itself permanent without nagging a user
// who already installed it globally.
func globallyInstalled() bool {
	out, err := exec.Command("npm", "prefix", "-g").Output()
	if err != nil {
		return false
	}
	return fileExists(filepath.Join(strings.TrimSpace(string(out)), "bin", "rig-move-llm"))
}

func yes(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "" || s == "y" || s == "yes"
}

// stdinIsTerminal reports whether stdin is an interactive terminal, so a bare
// `rig-move-llm` with no args launches the wizard only when a human can answer
// it (scripts/pipes get the usage text instead).
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
