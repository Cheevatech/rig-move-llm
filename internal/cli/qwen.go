package cli

import (
	"flag"
	"fmt"
	"os"

	"github.com/Cheevatech/rig-move-llm/internal/config"
)

// cmdQwen flips which brain answers the NEXT turn of a Claude Code session.
//
// This is the switch in its plainest form: one session, one context, no delegation and
// no return value to trust. The proxy re-reads a registered project's config on every
// request, so writing the flag lands on the very next turn of a session that is already
// running — you do not restart anything and you do not lose what has been said so far.
//
// Scope defaults to LOCAL because that is the scope a live session actually reads: `run`
// puts the project id in the base URL path, and the proxy resolves that project's config
// per request. A global flip only reaches sessions launched outside a registered project.
func cmdQwen(args []string) int {
	fs := flag.NewFlagSet("qwen", flag.ExitOnError)
	global := fs.Bool("global", false, "flip the global scope (~/.rig-move-llm) instead of this project")
	_ = fs.Parse(args)

	rest := fs.Args()
	action := "status"
	if len(rest) > 0 {
		action = rest[0]
	}

	path := scopeConfigPath(!*global)
	if !fileExists(path) {
		fmt.Fprintf(os.Stderr, "no config at %s — run `rig-move-llm init` here first\n", path)
		return 1
	}

	scope := "this project"
	if *global {
		scope = "all projects"
	}

	switch action {
	case "on":
		if err := setConfigKey(path, "RIG_ROUTE_ALL_TO_WORKER", "true"); err != nil {
			fmt.Fprintln(os.Stderr, "qwen:", err)
			return 1
		}
		fmt.Printf("qwen ON for %s — the next turn runs on the worker\n%s\n", scope, path)
		// A flag with nothing behind it turns every following turn into an error the
		// user has to read mid-task, so say it now instead.
		cfg := config.Load()
		if cfg.WorkerAPIBase == "" {
			fmt.Println("WARNING: no worker endpoint resolves — set WORKER_API_BASE first (`rig-move-llm config --open`)")
		}
		// ENABLED outranks this flag in the proxy, so an ON that cannot take effect
		// has to say so here — otherwise it silently reads as a flip that happened.
		if !cfg.Enabled {
			fmt.Println("WARNING: ENABLED=false, so turns still run on your paid model — run `rig-move-llm enable` to let the switch take effect")
		}
	case "off":
		if err := setConfigKey(path, "RIG_ROUTE_ALL_TO_WORKER", "false"); err != nil {
			fmt.Fprintln(os.Stderr, "qwen:", err)
			return 1
		}
		fmt.Printf("qwen OFF for %s — the next turn runs on your paid model\n%s\n", scope, path)
	case "status":
		cfg := config.Load()
		state := "OFF (paid model)"
		if cfg.RouteAllToWorker {
			state = "ON (worker)"
			if !cfg.Enabled {
				state = "ON, but ENABLED=false so turns still run on the paid model"
			}
		}
		fmt.Printf("qwen: %s\nworker: %s\nconfig: %s\n",
			state, orNone(cfg.WorkerAPIBase), path)
	default:
		fmt.Fprintf(os.Stderr, "qwen: unknown action %q — use on, off, or status\n", action)
		return 2
	}
	return 0
}

func orNone(s string) string {
	if s == "" {
		return "(none set)"
	}
	return s
}
