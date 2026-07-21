// Package cli is rig-move-llm's command surface: a single static binary with
// subcommands, dispatched from a bare os.Args slice (stdlib flag, no framework).
//
//	rig-move-llm serve [--port N]        run the routing proxy
//	rig-move-llm hook  pre-tool|post-tool|session-start|user-prompt  Claude Code hook (reads stdin)
//	rig-move-llm init  [--global] ...     bootstrap config + wiring for a scope
//	rig-move-llm run   [--] <cmd...>      launch a command with the proxy wired in
//	rig-move-llm stats [--reset|--history] token accounting (observability)
//	rig-move-llm version
package cli

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/internal/hook"
	"github.com/Cheevatech/rig-move-llm/internal/proxy"
	"github.com/Cheevatech/rig-move-llm/internal/service"
	"github.com/Cheevatech/rig-move-llm/internal/worker"
)

// Version is stamped at build time via -ldflags "-X ...cli.Version=...".
var Version = "dev"

const usage = `rig-move-llm — move the heavy lifting off your paid LLM
  Plan/review on your paid LLM; offload the code work to your own local (or cheap)
  model. Install once, run a plain 'claude'.

Setup
  rig-move-llm                             interactive setup wizard (same as 'setup')
  rig-move-llm setup                       guided install: scope + worker + wiring
  rig-move-llm init  [--global] [--npx] [--service] [flags]  non-interactive bootstrap
  rig-move-llm uninstall [--global] [--purge]  reverse init for a scope (incl. OS service)

Control
  rig-move-llm enable  [--local]           turn offload ON  (flip ENABLED in config.env)
  rig-move-llm disable [--local]           turn offload OFF (Claude Code runs normally)
  rig-move-llm config  [--local] [--open]  show the effective config / open it in $EDITOR
  rig-move-llm stats   [--reset|--history] token accounting / savings

Run
  rig-move-llm run    [--] <command...>    launch a command with the proxy wired in
  rig-move-llm serve  [--port N] [--status]  run the routing proxy / report its state

Internal (invoked by Claude Code / MCP; rarely run by hand)
  rig-move-llm hook   pre-tool|post-tool|session-start|user-prompt  Claude Code hook (reads stdin)
  rig-move-llm worker                      run the worker MCP server on stdio (offload tool)

  rig-move-llm version
  rig-move-llm help

Scope: 'global' follows you across every project (~/.rig-move-llm); 'local' is this
directory only (./.rig-move-llm). Precedence: process env > local > global.
Run "rig-move-llm <command> -h" for command flags.`

// Main is the entry point; it returns a process exit code.
func Main(args []string) int {
	if len(args) == 0 {
		// A bare invocation launches the setup wizard for an interactive user; a
		// pipe/script (no TTY) gets the usage text instead of hanging on a prompt.
		if stdinIsTerminal() {
			return cmdSetup(nil)
		}
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	// Terminal-backend agent teams (tmux/iterm2) invoke us as
	// CLAUDE_CODE_TEAMMATE_COMMAND with the teammate's claude flags appended
	// (led by --agent-id). Route those to the teammate launcher before the
	// normal subcommand switch — the flags are not a rig subcommand.
	if looksLikeTeammateSpawn(args) {
		return cmdTeammateExec(args)
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "setup":
		return cmdSetup(rest)
	case "teammate-exec":
		// Explicit form (docs/tests): the remaining args are the claude flags.
		return cmdTeammateExec(rest)
	case "serve":
		return cmdServe(rest)
	case "hook":
		return cmdHook(rest)
	case "worker":
		return cmdWorker(rest)
	case "init":
		return cmdInit(rest)
	case "uninstall":
		return cmdUninstall(rest)
	case "enable":
		return cmdEnable(rest)
	case "disable":
		return cmdDisable(rest)
	case "config":
		return cmdConfig(rest)
	case "run":
		return cmdRun(rest)
	case "stats":
		return cmdStats(rest)
	case "version", "--version", "-v":
		fmt.Println("rig-move-llm", Version)
		return 0
	case "help", "-h", "--help":
		fmt.Println(usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s\n", cmd, usage)
		return 2
	}
}

func cmdServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.String("port", "", "listen port (overrides config PORT)")
	status := fs.Bool("status", false, "report OS-service supervision state and whether the proxy is listening")
	_ = fs.Parse(args)

	cfg := config.Load()
	if *status {
		return serveStatus(cfg)
	}
	if *port != "" {
		cfg.Port = *port
	}

	srv := proxy.New(cfg)

	// Flush the ledger and close the log cleanly on SIGTERM/SIGINT so counters
	// survive a reboot or `run` teardown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		stop()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return 0
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, "serve:", err)
			return 1
		}
		return 0
	}
}

// serveStatus reports the two facts that matter for "is my rig alive": whether
// the OS supervisor has the service loaded, and whether anything is actually
// listening on the configured port (a session-child serve counts too).
func serveStatus(cfg config.Config) int {
	self, _ := os.Executable()
	home, _ := os.UserHomeDir()
	svc, _ := service.New(self, home, config.GlobalDir()).Status()
	fmt.Println("os service:", svc)

	addr := "127.0.0.1:" + cfg.Port
	if c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
		_ = c.Close()
		fmt.Println("proxy:      listening on", addr)
	} else {
		fmt.Println("proxy:      not listening on", addr)
		return 1
	}
	return 0
}

func cmdHook(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "hook: expected 'pre-tool' or 'post-tool'")
		return 2
	}
	switch args[0] {
	case "pre-tool":
		_ = buildHookState().PreTool(os.Stdin, os.Stdout)
	case "post-tool":
		_ = buildHookState().PostTool(os.Stdin, os.Stdout)
	case "session-start":
		// SessionStart hook (global installs): lazily materialize a per-project
		// .rig-move-llm/ carrying the configured settings, the way Serena creates
		// .serena on first session. Context-only; never blocks the session.
		return cmdSessionStart(os.Stdin, os.Stdout)
	case "user-prompt":
		// UserPromptSubmit hook: probe the worker endpoint (zero-token HTTP GET) and
		// cache the verdict so the per-tool hooks degrade to plain Claude Code when
		// the worker is unreachable. Never blocks the prompt.
		return cmdUserPrompt(os.Stdin, os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "hook: unknown phase %q (want pre-tool|post-tool|session-start|user-prompt)\n", args[0])
		return 2
	}
	return 0
}

// cmdWorker runs the worker MCP server on stdio: the OPTION-2 offload mechanism.
// Claude Code (or any MCP client) spawns this via the generated mcp-config and
// calls its `implement` tool; the tool runs an agentic loop on the configured
// worker endpoint. Because it is a separate process, that inference egresses to
// the worker by construction — the base-URL proxy is not involved. Config is read
// fresh from the current scope; the worker endpoint is bring-your-own.
func cmdWorker(args []string) int {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	_ = fs.Parse(args)
	if err := worker.Serve(config.Load(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		return 1
	}
	return 0
}

// buildHookState resolves the hook's on-disk state from config + env. The state
// dir defaults to the active scope's data dir; RIG_STATE_DIR overrides it.
func buildHookState() *hook.State {
	cfg := config.Load()
	dir := os.Getenv("RIG_STATE_DIR")
	if dir == "" {
		dir = cfg.DataDir
	}
	runner := os.Getenv("RIG_GATE_RUNNER")
	if runner == "" {
		if cand := filepath.Join(dir, "gate", "run_gate.sh"); fileExists(cand) {
			runner = cand
		}
	}
	return &hook.State{
		Enabled:      cfg.Enabled,
		LogPath:      filepath.Join(dir, "force-delegate.log"),
		GatePaths:    filepath.Join(dir, "gate_paths"),
		GateRunner:   runner,
		SharedMCP:    parseList(os.Getenv("MAIN_SHARED_MCP")),
		HealthMarker: hook.HealthMarkerPath(dir),
		HealthTTL:    10 * time.Minute,
		GateMode:     cfg.GateMode,
		StateDir:     dir,
	}
}

// parseList splits a comma/space-separated list into a set (lowercased).
func parseList(s string) map[string]bool {
	out := map[string]bool{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if f = strings.TrimSpace(f); f != "" {
			out[strings.ToLower(f)] = true
		}
	}
	return out
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
