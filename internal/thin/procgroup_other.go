//go:build !unix

package thin

import "os/exec"

// setProcGroup is a no-op off unix: there is no portable process-group knob, so
// the kill path reaches the direct child only. Descendants may survive; the
// status line says the round was killed either way, and the diff still comes
// back from the tree.
func setProcGroup(cmd *exec.Cmd) {}

func killProcGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func killOwnGroup(child *exec.Cmd) {
	if child != nil && child.Process != nil {
		_ = child.Process.Kill()
	}
}
