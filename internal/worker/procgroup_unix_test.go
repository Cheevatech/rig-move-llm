//go:build unix

package worker

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The orphan half of #18: `claude -p` spawns its own children, so killing the
// direct child alone leaves a test runner (or another agent) editing the same
// checkout after the round was abandoned. The kill must reach the group.
func TestKillProcGroupReachesDescendants(t *testing.T) {
	// The child prints its own background grandchild's pid, then waits.
	cmd := exec.Command("sh", "-c", "sleep 120 & echo $!; sleep 120")
	setProcGroup(cmd)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 32)
	n, err := out.Read(buf)
	if err != nil {
		t.Fatalf("reading grandchild pid: %v", err)
	}
	grandchild, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		t.Fatalf("grandchild pid %q: %v", buf[:n], err)
	}

	if err := killProcGroup(cmd); err != nil {
		t.Fatalf("killProcGroup: %v", err)
	}
	_ = cmd.Wait()

	// signal 0 probes existence; the grandchild must be gone (allow a moment for
	// the kernel to reap it).
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := syscall.Kill(grandchild, 0); err != nil {
			return // gone
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived the group kill", grandchild)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
