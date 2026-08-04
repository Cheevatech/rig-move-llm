//go:build unix

package thin

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcGroup puts the supervisor (and therefore everything it starts) in its
// own process group, so one kill(2) with a negative pid reaches the whole tree:
// `claude -p` spawns test runners, git and package managers, and killing only
// the direct child leaves them editing the same checkout.
//
// G1 recorded the cost of this: a subprocess in its own group is immune to the
// group signals that reach the server, which is why SIGTERM/SIGKILL of the
// server left qwen running. The answer is not to drop Setpgid — we still need
// it to kill grandchildren — it is to kill on purpose from both ends: the server
// kills down (killProcGroup), and the supervisor kills sideways when the server
// can no longer act (supervise.go).
func setProcGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcGroup SIGKILLs the whole group, falling back to the single process
// when the group signal fails (the child died between Start and the kill).
func killProcGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// killOwnGroup is the supervisor's half: SIGKILL every process in the group it
// leads, itself included. The leader check is a safety catch — a supervisor run
// outside its own group (a developer invoking it by hand, a test) must not
// SIGKILL the shell that started it, so it settles for the child it owns.
func killOwnGroup(child *exec.Cmd) {
	if pgrp, err := syscall.Getpgid(0); err == nil && pgrp == os.Getpid() {
		_ = syscall.Kill(-pgrp, syscall.SIGKILL)
		return
	}
	if child != nil && child.Process != nil {
		_ = child.Process.Kill()
	}
}
