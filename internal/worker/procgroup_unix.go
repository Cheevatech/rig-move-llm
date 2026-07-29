//go:build unix

package worker

import (
	"os/exec"
	"syscall"
)

// setProcGroup puts the worker subprocess in its own process group. The stall
// guard must be able to kill the whole tree: `claude -p` spawns its own children
// (test runners, git, package managers), and killing only the direct child
// leaves them running against the same checkout — an abandoned round still
// editing the tree while MAIN starts the next one (#18).
func setProcGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcGroup SIGKILLs the whole group (kill(2) with a negative pid). It falls
// back to the single process when the group signal fails — e.g. the child died
// between Start and the kill, so the group no longer exists.
func killProcGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil {
		return nil
	}
	return cmd.Process.Kill()
}
