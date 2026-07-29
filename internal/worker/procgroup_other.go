//go:build !unix

package worker

import "os/exec"

// setProcGroup is a no-op off unix: there is no portable process-group knob, so
// the stall guard falls back to killing the direct child only.
func setProcGroup(cmd *exec.Cmd) {}

// killProcGroup kills the worker subprocess itself. Descendants may survive on
// this platform — the guard still bounds the round, but the diagnosis is the
// only thing MAIN can rely on to know a leftover may exist.
func killProcGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
