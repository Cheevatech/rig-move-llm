// Package config loads rig-move-llm's runtime configuration by layering, in
// increasing precedence: a global config file (~/.rig-move-llm/config.env), a
// local one (./.rig-move-llm/config.env), and finally the process environment.
// Local overrides global; an explicit env var overrides both. This mirrors the
// install-scope model (global = all projects, local = this project only).
package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DirName is the per-scope config/data directory (holds config.env, logs, stats).
const DirName = ".rig-move-llm"

// ConfigFile is the env-format config file inside a scope dir.
const ConfigFile = "config.env"

// Config holds all runtime configuration for the proxy.
type Config struct {
	Port            string
	MainUpstreamURL string // e.g. https://api.anthropic.com
	WorkerAPIBase   string // e.g. http://host:8000/v1 (already includes /v1)
	WorkerAPIKey    string
	WorkerModel     string // overrides the inbound haiku model when talking to the worker
	Backend         Backend
	// Workers is the optional multi-endpoint fallback chain from workers.json
	// (priority-sorted). Empty means the scalar Worker* fields above are the
	// single endpoint — the pre-L2 behavior, unchanged.
	Workers []WorkerEndpoint
	// CustomSubagentUsage is the L4 %-budget: the target share (1-99) of
	// worker-tier tokens served by the custom worker over a sliding window;
	// requests above the target are diverted to the paid upstream (logged as
	// routed=diverted). 100 — the default, and what any out-of-range or
	// unparsable value falls back to — routes every worker-tier request to the
	// worker: the pre-L4 behavior, and the direction that never burns quota.
	CustomSubagentUsage int
	LogBodies           bool   // opt-in full request/response logging (default: metadata only)
	LogMaxMB            int    // size cap for logs/requests.jsonl before compaction (default 50)
	DataDir             string // scope dir where logs/stats are written (resolved local|global)
}

// GlobalDir returns ~/.rig-move-llm.
func GlobalDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return DirName
	}
	return filepath.Join(home, DirName)
}

// LocalDir returns ./.rig-move-llm relative to the current working directory.
func LocalDir() string {
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		return DirName
	}
	return filepath.Join(cwd, DirName)
}

// Load resolves configuration as seen from the current working directory.
func Load() Config {
	cwd, _ := os.Getwd()
	return LoadFrom(cwd)
}

// LoadFrom resolves configuration from the layered sources described in the
// package doc, treating projectDir as the local scope (its .rig-move-llm/
// config.env layers over the global one). It reads the files fresh on every
// call — no cache — so a config edit takes effect on the next request without a
// restart. It never returns an error: missing files are simply skipped, and
// unset values keep their zero/default.
func LoadFrom(projectDir string) Config {
	global := parseEnvFile(filepath.Join(GlobalDir(), ConfigFile))
	local := map[string]string{}
	if projectDir != "" {
		local = parseEnvFile(filepath.Join(projectDir, DirName, ConfigFile))
	}

	// get reads a key with precedence env > local file > global file.
	get := func(key string) string {
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		if v, ok := local[key]; ok {
			return v
		}
		return global[key]
	}

	port := get("PORT")
	if port == "" {
		port = "4000"
	}

	backend := LookupBackend(get("WORKER_BACKEND"))

	workerBase := strings.TrimRight(get("WORKER_API_BASE"), "/")
	if workerBase == "" {
		workerBase = strings.TrimRight(backend.DefaultBase, "/")
	}

	// The scope that actually owns the local config file also owns the data dir;
	// fall back to global when this project has no local config.
	dataDir := GlobalDir()
	if len(local) > 0 && projectDir != "" {
		dataDir = filepath.Join(projectDir, DirName)
	}

	logMaxMB := 50
	if n, err := strconv.Atoi(strings.TrimSpace(get("LOG_MAX_MB"))); err == nil && n > 0 {
		logMaxMB = n
	}

	// Only 1-99 enables alternation; 0, 100, and garbage all mean "all custom"
	// (fail-cheap: a bad value must not silently divert traffic to paid quota).
	customUsage := 100
	if n, err := strconv.Atoi(strings.TrimSpace(get("CUSTOM_SUBAGENT_USAGE"))); err == nil && n >= 1 && n <= 99 {
		customUsage = n
	}

	return Config{
		Port:                port,
		MainUpstreamURL:     strings.TrimRight(get("MAIN_UPSTREAM_URL"), "/"),
		WorkerAPIBase:       workerBase,
		WorkerAPIKey:        get("WORKER_API_KEY"),
		WorkerModel:         get("WORKER_MODEL"),
		Backend:             backend,
		Workers:             loadWorkers(projectDir),
		CustomSubagentUsage: customUsage,
		LogBodies:           truthy(get("LOG_BODIES")),
		LogMaxMB:            logMaxMB,
		DataDir:             dataDir,
	}
}

// parseEnvFile reads a KEY=VALUE file (# comments, blank lines, optional `export`
// prefix, optional surrounding quotes on the value). Missing files yield an empty
// map. This is a deliberately small stdlib parser — no dotenv dependency.
func parseEnvFile(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if key != "" {
			out[key] = val
		}
	}
	return out
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
