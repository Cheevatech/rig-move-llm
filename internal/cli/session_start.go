package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cheevatech/rig-move-llm/internal/config"
)

// cmdSessionStart is the SessionStart hook for a GLOBAL install. It lazily
// materializes a per-project .rig-move-llm/ the first time a session opens in a
// project — the way Serena creates .serena/ — carrying the settings configured
// globally so the project gets its own scope (and stats) with zero manual init.
//
// It is deliberately silent (no additionalContext) so it costs no MAIN tokens,
// and never overwrites an existing per-project config (a cloned repo's own
// .rig-move-llm/config.env is left untouched — it is not auto-trusted).
func cmdSessionStart(r io.Reader, w io.Writer) int {
	_, _ = io.Copy(io.Discard, r) // drain the payload; we only need the cwd

	projDir := os.Getenv("CLAUDE_PROJECT_DIR")
	if projDir == "" {
		projDir, _ = os.Getwd()
	}
	if projDir == "" {
		return 0
	}

	localCfg := filepath.Join(projDir, config.DirName, config.ConfigFile)
	if fileExists(localCfg) {
		return 0 // already has its own scope — nothing to do
	}

	// Only follow the user into a project when there is a global config to carry.
	globalData, err := os.ReadFile(filepath.Join(config.GlobalDir(), config.ConfigFile))
	if err != nil {
		return 0 // no global scope -> not a global install -> stay out of the way
	}

	dir := filepath.Join(projDir, config.DirName)
	if os.MkdirAll(dir, 0o755) != nil {
		return 0
	}
	// Carry the configured settings, but never copy the API key into every project
	// tree — comment it so it inherits from ~/.rig-move-llm/config.env instead.
	if os.WriteFile(localCfg, []byte(inheritKey(string(globalData))), 0o600) != nil {
		return 0
	}
	// Keep the per-project scope (config, logs, stats) out of version control.
	_ = os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*\n"), 0o644)
	return 0
}

// inheritKey comments out the WORKER_API_KEY line so a materialized per-project
// config falls back to the global key rather than duplicating the secret.
func inheritKey(env string) string {
	lines := strings.Split(env, "\n")
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "WORKER_API_KEY=") {
			lines[i] = "# " + ln + "   (inherits ~/.rig-move-llm/config.env)"
		}
	}
	return strings.Join(lines, "\n")
}
