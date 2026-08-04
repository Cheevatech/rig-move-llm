package thin

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// The supervisor exists for exactly one case, and it is the one no amount of
// server-side code can cover: `kill -9` of the MCP server. Everything softer —
// notifications/cancelled, SIGTERM, stdin EOF — the server can observe and act
// on. SIGKILL runs no handler, so if the server is the only thing holding the
// leash, the tree is orphaned and keeps burning. G1 measured that at ~70 minutes.
//
// So rig does not spawn `claude` directly. It spawns ITSELF as
// `rig thin-supervise --parent <server pid> -- claude ...`, in a fresh process
// group, and the supervisor starts claude inside that same group. The supervisor
// then watches one number: its own parent pid. When the server dies by any
// means, the supervisor is reparented (getppid changes), notices within a
// polling interval, and SIGKILLs the group — taking claude, its test runner and
// every grandchild with it, and itself last.
//
// The check is deliberately the dumbest one available. No pipes to keep open, no
// protocol to agree on, no state to get out of sync: an orphan is an orphan.

// superviseWatchInterval is how often the supervisor asks whether it has been
// orphaned. The harness bound is "dead within 5 seconds"; 200ms leaves that
// bound almost entirely to process teardown.
const superviseWatchInterval = 200 * time.Millisecond

// superviseArgs is the parsed form of the supervisor's own command line.
type superviseArgs struct {
	Parent  int      // pid to watch; 0 disables the watchdog
	Command []string // the command to run, after --
}

// parseSuperviseArgs reads `--parent <pid> -- <cmd> <args...>`. It is hand-rolled
// rather than flag-based because everything after -- belongs to the child and
// must survive verbatim, including things that look like our own flags.
func parseSuperviseArgs(args []string) (superviseArgs, error) {
	var out superviseArgs
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--parent" && i+1 < len(args):
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				return out, fmt.Errorf("thin-supervise: bad --parent %q: %w", args[i+1], err)
			}
			out.Parent = n
			i++
		case args[i] == "--":
			out.Command = args[i+1:]
			return out, validateSupervise(out)
		default:
			return out, fmt.Errorf("thin-supervise: unexpected argument %q", args[i])
		}
	}
	return out, validateSupervise(out)
}

func validateSupervise(a superviseArgs) error {
	if len(a.Command) == 0 {
		return errors.New("thin-supervise: no command after --")
	}
	return nil
}

// Supervise runs the child and dies with it — or kills it and dies, whichever
// comes first. It returns the process exit code for the caller to hand to
// os.Exit.
func Supervise(args []string) int {
	parsed, err := parseSuperviseArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	cmd := exec.Command(parsed.Command[0], parsed.Command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// No setProcGroup here: the child must stay in OUR group, which the server
	// already made fresh for us. That is what makes one negative-pid kill enough.
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "thin-supervise: launch %s: %v\n", parsed.Command[0], err)
		return 127
	}

	// The soft path: a SIGTERM aimed at the group (or at us alone) still means
	// stop, and we can turn it into a hard kill of the whole tree.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(superviseWatchInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-sig:
				killOwnGroup(cmd)
				return
			case <-ticker.C:
				if parsed.Parent != 0 && os.Getppid() != parsed.Parent {
					// Orphaned: whoever asked for this work can no longer be told
					// about it, and nobody is reading the output. Take the tree down.
					killOwnGroup(cmd)
					return
				}
			}
		}
	}()

	err = cmd.Wait()
	close(done)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "thin-supervise: %v\n", err)
		return 1
	}
	return 0
}

// supervisorArgv builds the argv prefix that wraps the worker command. The
// binary is normally rig itself; RIG_THIN_SUPERVISOR_BIN overrides it so tests
// can supervise with the test binary instead of a freshly built rig.
//
// If the executable cannot be resolved there is no supervisor: the run still
// happens (a switch that refuses to switch is worse than one whose SIGKILL story
// is weaker), and the server-side kill path is unaffected.
func supervisorArgv(parent int) []string {
	exe := os.Getenv("RIG_THIN_SUPERVISOR_BIN")
	if exe == "" {
		resolved, err := os.Executable()
		if err != nil {
			return nil
		}
		exe = resolved
	}
	return []string{exe, "thin-supervise", "--parent", strconv.Itoa(parent), "--"}
}
