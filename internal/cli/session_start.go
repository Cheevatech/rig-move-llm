package cli

import (
	"io"
	"os"
	"path/filepath"

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

	_ = globalData // presence already checked above; contents are intentionally not copied

	dir := filepath.Join(projDir, config.DirName)
	if os.MkdirAll(dir, 0o755) != nil {
		return 0
	}
	// Inherit-not-copy (the Serena model): write a marker config that carries NO
	// settings, so the project inherits endpoint/model/key/ENABLED from the global
	// scope and a later change to ~/.rig-move-llm/config.env propagates here. The
	// folder's presence alone gives the project its own stats/logs (see config
	// dataDir resolution). Add a KEY here only to override the global value for this
	// project (e.g. ENABLED=false to turn the hybrid off in this project only).
	if os.WriteFile(localCfg, []byte(projectMarkerEnv), 0o600) != nil {
		return 0
	}
	// Keep the per-project scope (config, logs, stats) out of version control.
	_ = os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*\n"), 0o644)
	return 0
}

// projectMarkerEnv is the per-project config a global install materializes: no
// settings, only guidance. Empty of values so everything inherits the global
// scope and stays in sync with it; a user adds a KEY=value line here to override
// one setting for this project.
const projectMarkerEnv = `# rig-move-llm — this project inherits its settings from the global scope
# (~/.rig-move-llm/config.env). Precedence: process env > this file > global.
#
# It is intentionally empty of settings, so changing the global config (endpoint,
# model, ENABLED on/off) propagates here automatically. Add a line such as
#   ENABLED=false
# ONLY to override the global value for THIS project.
`
