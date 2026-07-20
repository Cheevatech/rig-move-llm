package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Cheevatech/rig-move-llm/internal/config"
)

// cmdSetup is the interactive, guided install: a plain `rig-move-llm` (no args)
// or `rig-move-llm setup` walks the user through scope, worker endpoint (which is
// skippable — skipping installs everything inert so Claude Code runs normally),
// and then wires Claude Code end to end. Line-based stdin prompts only, so the
// single static stdlib binary stays dependency-free (no TUI framework).
func cmdSetup(args []string) int {
	in := bufio.NewScanner(os.Stdin)
	ask := func(prompt, def string) string {
		if def != "" {
			fmt.Printf("  %s [%s]: ", prompt, def)
		} else {
			fmt.Printf("  %s: ", prompt)
		}
		if !in.Scan() {
			return def
		}
		if s := strings.TrimSpace(in.Text()); s != "" {
			return s
		}
		return def
	}

	fmt.Println("rig-move-llm setup — move the heavy lifting off your paid LLM.")
	fmt.Println()

	o := initOpts{mainUpstream: "https://api.anthropic.com", port: "4000", force: true}

	// 1. Scope. Global "follows you" across every project (like Serena); project
	//    scope touches only this directory.
	fmt.Println("Scope: 'global' installs once for every project (recommended, follows you);")
	fmt.Println("       'project' wires only this directory.")
	o.global = !strings.HasPrefix(strings.ToLower(ask("scope (global/project)", "global")), "p")
	fmt.Println()

	// 2. Worker endpoint — SKIPPABLE. Auto-detect a local one first.
	fmt.Println("Worker endpoint — where the offloaded code work runs (your own local model or")
	fmt.Println("any OpenAI-compatible API). Press Enter at the URL to SKIP: rig installs but stays")
	fmt.Println("OFF, so Claude Code runs exactly as normal. You can turn it on later.")
	if d, ok := detectWorker(); ok {
		fmt.Printf("  detected a local worker: %s at %s%s\n", d.Backend, d.Base, modelNote(d.Model))
		if yes(ask("use it? (y/n)", "y")) {
			o.backend, o.workerBase, o.workerModel = d.Backend, d.Base, d.Model
		}
	}
	if o.workerBase == "" {
		o.workerBase = ask("worker base URL (OpenAI-compatible, e.g. http://localhost:11434/v1)", "")
		if o.workerBase != "" {
			o.backend = ask("backend ("+strings.Join(config.BackendNames(), "|")+")", "generic")
			o.workerModel = ask("worker model name", o.workerModel)
			o.workerKey = ask("worker API key (optional, Enter to skip)", "")
		}
	}
	o.enabled = o.workerBase != ""
	fmt.Println()

	// 3. Make the binary permanent. The hooks invoke `rig-move-llm ...`, so it must
	//    stay on PATH — but `npx rig-move-llm` runs transiently. Install it globally
	//    now (that is what makes this a single command). Skipped when it is already
	//    a real global install (not the npx cache).
	if !globallyInstalled() {
		fmt.Println("The hooks call `rig-move-llm`, so it needs to stay on your PATH.")
		if yes(ask("install it globally now (npm install -g rig-move-llm)? (y/n)", "y")) {
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
		fmt.Println()
	}

	// 4. Wire Claude Code.
	if rc := applyInit(o); rc != 0 {
		return rc
	}
	fmt.Println()
	fmt.Println("Done. Just run:  claude")
	return 0
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
