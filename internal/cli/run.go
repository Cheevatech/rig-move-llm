package cli

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/rigmovellm/rig-move-llm/internal/config"
)

// cmdRun launches a command (typically `claude`) with the proxy wired into its
// environment for THIS process only — the crux of local scope: ANTHROPIC_BASE_URL
// is a per-process env, so it never leaks to other projects. If the proxy is not
// already listening, a best-effort `serve` is started in the background.
func cmdRun(args []string) int {
	// Allow an optional `--` separator: `run -- claude ...`.
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "run: expected a command, e.g. rig-move-llm run -- claude")
		return 2
	}

	cfg := config.Load()
	addr := "127.0.0.1:" + cfg.Port
	baseURL := "http://" + addr

	// Registered projects get their identity embedded in the base URL path
	// prefix so a global daemon (cwd=/) can load THIS project's config per
	// request. Unregistered projects keep the plain base URL (global config).
	cwd, _ := os.Getwd()
	if canon, err := config.CanonicalPath(cwd); err == nil {
		if config.ProjectAllowed(canon) {
			baseURL += "/p/" + config.EncodeProjectID(canon)
		} else if fileExists(filepath.Join(config.LocalDir(), config.ConfigFile)) {
			fmt.Fprintln(os.Stderr, "run: local config.env found but this project is not registered — run 'rig-move-llm init' here to activate the per-project override")
		}
	}

	if !portOpen(addr) {
		if err := startServe(); err != nil {
			fmt.Fprintln(os.Stderr, "run: could not start proxy:", err)
			return 1
		}
		waitPort(addr, 10*time.Second)
	}

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(),
		"ANTHROPIC_BASE_URL="+baseURL,
		"CLAUDE_CODE_SUBAGENT_MODEL=haiku", // pins subagents to the worker leg
	)
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "run:", err)
		return 1
	}
	return 0
}

// startServe spawns `<self> serve` detached, logging to the scope data dir.
func startServe() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "serve")
	cmd.Stdout, cmd.Stderr = nil, nil
	return cmd.Start() // released; survives for the session
}

func portOpen(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func waitPort(addr string, d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if portOpen(addr) {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
}
