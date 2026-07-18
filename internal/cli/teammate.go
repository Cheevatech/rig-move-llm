package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// cmdTeammateExec is rig's CLAUDE_CODE_TEAMMATE_COMMAND launcher. Terminal-backend
// agent teams (tmux/iterm2) spawn each teammate as a fresh `claude` process whose
// hook payloads carry NO agent_id — so the force-delegate hook would mistake the
// teammate for the paid MAIN and deny its tools. We stamp the teammate's identity
// into RIG_AGENT_ID (which the hook treats as agent_id) and, by default, pin the
// teammate to the worker tier so its heavy lifting runs on your endpoint, then
// exec the real claude with the flags claude handed us (untouched save the model
// pin). The lead process is never launched through here, so it keeps no marker.
//
// The default in-process backend never invokes this: those teammates share the
// lead's process and already carry agent_id in their payloads. This path is
// exercised only by the opt-in tmux/iterm2 team backends.
func cmdTeammateExec(claudeArgs []string) int {
	agentID, args := teammatePlan(claudeArgs, os.Getenv("RIG_TEAMMATE_MODEL"))

	claudeBin := resolveClaude()
	if claudeBin == "" {
		fmt.Fprintln(os.Stderr, "teammate-exec: could not locate the claude binary (set CLAUDE_CODE_EXECPATH or put claude on PATH)")
		return 127
	}

	env := os.Environ()
	if agentID != "" {
		env = append(env, "RIG_AGENT_ID="+agentID)
	}
	argv := append([]string{claudeBin}, args...)
	if err := syscall.Exec(claudeBin, argv, env); err != nil {
		fmt.Fprintln(os.Stderr, "teammate-exec: exec claude:", err)
		return 1
	}
	return 0 // unreachable: exec replaces this process on success
}

// teammatePlan computes the teammate's identity and the claude flags to launch it
// with. modelOverride is RIG_TEAMMATE_MODEL: empty means default to the worker
// tier ("haiku"); "inherit" leaves the lead's requested model untouched.
func teammatePlan(claudeArgs []string, modelOverride string) (agentID string, args []string) {
	agentID = flagValue(claudeArgs, "--agent-id")
	model := modelOverride
	if model == "" {
		model = "haiku"
	}
	if model == "inherit" {
		return agentID, claudeArgs
	}
	return agentID, setFlag(claudeArgs, "--model", model)
}

// resolveClaude finds the real claude binary. claude exposes its own path to
// spawned teammates via CLAUDE_CODE_EXECPATH; PATH is the fallback.
func resolveClaude() string {
	if p := os.Getenv("CLAUDE_CODE_EXECPATH"); p != "" {
		return p
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	return ""
}

// looksLikeTeammateSpawn reports whether args are a claude teammate-spawn argv
// (claude appends the teammate's flags, led by --agent-id, to whatever we set as
// CLAUDE_CODE_TEAMMATE_COMMAND). No rig subcommand starts with "-", so a leading
// flag plus --agent-id unambiguously identifies the terminal-backend spawn path.
func looksLikeTeammateSpawn(args []string) bool {
	return len(args) > 0 && strings.HasPrefix(args[0], "-") && containsFlag(args, "--agent-id")
}

func containsFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}

// flagValue returns the value of `--name value` or `--name=value`, "" if absent.
func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v
		}
	}
	return ""
}

// setFlag returns args with `--name` set to value, replacing an existing
// occurrence (either `--name v` or `--name=v`) in place or appending if absent.
func setFlag(args []string, name, value string) []string {
	out := make([]string, 0, len(args)+2)
	replaced := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == name {
			out = append(out, name, value)
			if i+1 < len(args) {
				i++ // consume the old value
			}
			replaced = true
			continue
		}
		if strings.HasPrefix(a, name+"=") {
			out = append(out, name+"="+value)
			replaced = true
			continue
		}
		out = append(out, a)
	}
	if !replaced {
		out = append(out, name, value)
	}
	return out
}
