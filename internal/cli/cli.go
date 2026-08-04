// Package cli is rig-move-llm's command surface: a single static binary with
// subcommands, dispatched from a bare os.Args slice (stdlib flag, no framework).
//
//	rig-move-llm serve [--port N]        run the routing proxy
//	rig-move-llm thin-worker             the switch: MCP stdio server (one tool)
//	rig-move-llm init  [--global] ...     bootstrap config + wiring for a scope
//	rig-move-llm run   [--] <cmd...>      launch a command with the proxy wired in
//	rig-move-llm stats [--reset|--history] token accounting (observability)
//	rig-move-llm watch [--list]           follow a run's actions live
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
	"syscall"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/internal/proxy"
	"github.com/Cheevatech/rig-move-llm/internal/service"
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
  rig-move-llm doctor                      prove the offload rig is live before you trust a number
  rig-move-llm watch  [--list] [<run dir>]  follow what the worker is doing right now

Run
  rig-move-llm run    [--] <command...>    launch a command with the proxy wired in
  rig-move-llm serve  [--port N] [--status]  run the routing proxy / report its state

Internal (invoked by Claude Code / MCP; rarely run by hand)
  rig-move-llm thin-worker                 the switch: MCP stdio server exposing 'implement'

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
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "setup":
		return cmdSetup(rest)
	case "serve":
		return cmdServe(rest)
	case "thin-worker":
		return cmdThinWorker(rest)
	case "thin-supervise":
		return cmdThinSupervise(rest)
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
	case "doctor":
		return cmdDoctor(rest)
	case "watch":
		return cmdWatch(rest)
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

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
