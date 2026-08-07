// Package cli is rig-move-llm's command surface: a single static binary with
// subcommands, dispatched from a bare os.Args slice (stdlib flag, no framework).
//
//	rig-move-llm serve [--port N]         run the routing proxy
//	rig-move-llm worker on|off|status     swap the model answering the next turn
//	rig-move-llm init  [--global] ...     bootstrap config for a scope
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
	"syscall"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/internal/proxy"
	"github.com/Cheevatech/rig-move-llm/internal/service"
)

// Version is stamped at build time via -ldflags "-X ...cli.Version=...".
var Version = "dev"

const usage = `rig-move-llm — move the heavy lifting off your paid LLM
  Plan on your paid model, then swap in your own local (or cheap) model for the
  next turn — same session, same context, no hand-off.

    rig-move-llm run -- claude     launch a session through the switch
    /worker on                     (inside the session) next turn runs on your worker
    /worker off                    (inside the session) next turn runs on your paid model

  Either model can run "worker on|off" itself, and so can you — from a second
  terminal, which is the one path that works whichever model is driving.

Setup
  rig-move-llm                             interactive setup wizard (same as 'setup')
  rig-move-llm setup                       guided install: scope + worker endpoint
  rig-move-llm init  [--global] [--service] [flags]  non-interactive bootstrap
  rig-move-llm uninstall [--global] [--purge]  reverse init for a scope (incl. OS service)

Control
  rig-move-llm worker  on|off|status [--global]  swap the model answering the NEXT turn
  rig-move-llm config  [--local] [--open]  show the effective config / open it in $EDITOR
  rig-move-llm enable  [--local]           allow the switch to route to the worker
  rig-move-llm disable [--local]           pin every request to your paid model
  rig-move-llm stats   [--reset|--history] token accounting, split by leg
  rig-move-llm doctor  [--json]            prove the switch is live before you trust a number

Run
  rig-move-llm run    [--] <command...>    launch a command with the proxy wired in
  rig-move-llm serve  [--port N] [--bind ADDR] [--status]  run the routing proxy / report its state

  rig-move-llm version
  rig-move-llm help

A bare 'claude' does NOT go through rig: 'run' is what sets ANTHROPIC_BASE_URL, and
it sets it for that one process only.

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
	case "worker":
		return cmdWorker(rest)
	case "qwen":
		// Undocumented alias. The command shipped to nobody under this name — it
		// was renamed before the first release that contains it — but it is what
		// this project's own author typed for a week, so it keeps working and says
		// so once. Drop it in 0.9.
		fmt.Fprintln(os.Stderr, "note: `qwen` is now `worker` (the endpoint is whatever model you point rig at)")
		return cmdWorker(rest)
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
	bind := fs.String("bind", proxy.LoopbackBind, "interface to listen on — anything but loopback exposes an unauthenticated relay to your worker")
	_ = fs.Parse(args)

	cfg := config.Load()
	if *status {
		return serveStatus(cfg)
	}
	if *port != "" {
		cfg.Port = *port
	}

	srv := proxy.New(cfg)
	srv.Bind = *bind
	if *bind != proxy.LoopbackBind {
		// Worth a line on stderr rather than only in the log: the worker leg
		// authenticates with the key in config.env, so off-box callers need no
		// credentials of their own.
		fmt.Fprintf(os.Stderr, "serve: listening on %s, not loopback — anyone who can reach this port can spend your worker endpoint\n", *bind)
	}

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
