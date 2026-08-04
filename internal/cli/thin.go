package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/internal/thin"
)

// cmdThinWorker runs the map17 switch: the stdio MCP server with one tool and a
// kill path that works. It lives beside `worker` rather than replacing it while
// S4 is still open — the old path is deleted as one package, in one commit that
// can be read at a glance, not eroded from inside.
func cmdThinWorker(args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "thin-worker takes no arguments")
		return 2
	}
	cfg := config.Load()
	// The engine reads its knobs from the environment, so config-file values are
	// promoted here. Load already resolved precedence env-first, so re-setting a
	// resolved value never overrides an explicitly-set variable.
	for k, v := range map[string]string{
		"RIG_CC_BASE_URL": cfg.CCBaseURL,
		"RIG_CC_MODEL":    cfg.CCModel,
	} {
		if v != "" {
			os.Setenv(k, v)
		}
	}

	// SIGTERM is one of the four ways G1 tried to stop a run, and the only one
	// this process can turn into an orderly stop: cancel the context, which kills
	// the worker tree, and let Serve return. The grace window is short because
	// the work being cancelled is already dead by then — this only waits for the
	// reply to be written.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- thin.Serve(ctx, os.Stdin, os.Stdout) }()

	select {
	case err := <-done:
		if err != nil {
			fmt.Fprintln(os.Stderr, "thin-worker:", err)
			return 1
		}
		return 0
	case <-ctx.Done():
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
		return 0
	}
}

// cmdThinSupervise is the watchdog half of the kill path, invoked by rig on
// itself. It is not a user-facing command: see supervise.go for why it must be a
// separate process.
func cmdThinSupervise(args []string) int { return thin.Supervise(args) }
