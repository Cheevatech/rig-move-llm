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
	WorkerModel     string // model name the worker MCP tool sends to the worker endpoint
	Backend         Backend
	Enabled         bool   // master on/off: when false the hook passes every tool through (Claude Code behaves normally, no offload/force-delegate). Defaults to true when a worker endpoint is set, false when it is skipped; an explicit ENABLED overrides.
	LogBodies       bool   // opt-in full request/response logging (default: metadata only)
	LogMaxMB        int    // size cap for logs/requests.jsonl before compaction (default 50)
	DataDir         string // scope dir where logs/stats are written (resolved local|global)
	// WorkerHealthPath is the path probed at UserPromptSubmit to decide whether the
	// worker endpoint is reachable. Default "/v1/models" (universal + free across
	// OpenAI-compatible servers). Set to "off"/"none"/"-" to disable pre-flight
	// health-checking (call-time error fallback still applies).
	WorkerHealthPath string
	HealthTimeoutMs  int // per-probe HTTP timeout (default 2000)
	HealthCacheSec   int // reuse a probe result for this many seconds (default 15)
	// GateMode selects the delegation posture (map6 cost-aware gate):
	//   "hard" (default) — MAIN is plan/delegate/review only; every Edit is denied.
	//   "soft"           — explore-first: Stage-0 evidence + a triage declaration can
	//                      open a bounded solo edit window, and a small Gate B repair
	//                      window opens after each worker return. Rollback = flip the key.
	GateMode string
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

// FileValues reads a single scope's config.env into a KEY=VALUE map (empty when the
// file is absent). It lets the CLI (e.g. `rig-move-llm config`) show which layer set
// a value, without duplicating the env-file parser.
func FileValues(path string) map[string]string {
	return parseEnvFile(path)
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

	// getOK reads a key with precedence env > local file > global file, reporting
	// whether it was set in any layer (so an absent key can default differently
	// from an explicitly-empty one).
	getOK := func(key string) (string, bool) {
		if v, ok := os.LookupEnv(key); ok {
			return v, true
		}
		if v, ok := local[key]; ok {
			return v, true
		}
		if v, ok := global[key]; ok {
			return v, true
		}
		return "", false
	}
	get := func(key string) string { v, _ := getOK(key); return v }

	port := get("PORT")
	if port == "" {
		port = "4000"
	}

	backend := LookupBackend(get("WORKER_BACKEND"))

	workerBase := strings.TrimRight(get("WORKER_API_BASE"), "/")
	if workerBase == "" {
		workerBase = strings.TrimRight(backend.DefaultBase, "/")
	}

	// A project owns its own data dir (stats/logs/gate state) whenever it has a
	// .rig-move-llm/ directory — even when its config.env only inherits (all
	// comments). Settings still layer local-over-global; this only decides WHERE
	// per-project state is written. Keying on the directory (not on the file having
	// values) is what lets a materialized project inherit every setting from global
	// yet keep its own stats — and lets a global config change propagate here.
	dataDir := GlobalDir()
	if projectDir != "" {
		if d := filepath.Join(projectDir, DirName); dirExists(d) {
			dataDir = d
		}
	}

	logMaxMB := 50
	if n, err := strconv.Atoi(strings.TrimSpace(get("LOG_MAX_MB"))); err == nil && n > 0 {
		logMaxMB = n
	}

	// Health-check: default to the universal /v1/models probe unless the key was
	// explicitly set (an explicit empty value disables it, same as "off").
	healthPath := "/v1/models"
	if v, ok := getOK("WORKER_HEALTH_PATH"); ok {
		healthPath = strings.TrimSpace(v)
	}
	healthTimeout := 2000
	if n, err := strconv.Atoi(strings.TrimSpace(get("WORKER_HEALTH_TIMEOUT_MS"))); err == nil && n > 0 {
		healthTimeout = n
	}
	healthCache := 15
	if n, err := strconv.Atoi(strings.TrimSpace(get("WORKER_HEALTH_CACHE_SEC"))); err == nil && n >= 0 {
		healthCache = n
	}

	// Master switch. Absent -> enabled only when a worker endpoint is configured
	// (skipping the worker in setup leaves it off, so Claude Code runs normally);
	// an explicit ENABLED wins either way, so a user can pre-wire everything and
	// flip it on later without re-running init.
	enabled := workerBase != ""
	if v, ok := getOK("ENABLED"); ok {
		enabled = truthy(v)
	}

	gateMode := strings.ToLower(strings.TrimSpace(get("GATE_MODE")))
	if gateMode != "soft" {
		gateMode = "hard"
	}

	return Config{
		Port:             port,
		MainUpstreamURL:  strings.TrimRight(get("MAIN_UPSTREAM_URL"), "/"),
		WorkerAPIBase:    workerBase,
		WorkerAPIKey:     get("WORKER_API_KEY"),
		WorkerModel:      get("WORKER_MODEL"),
		Backend:          backend,
		Enabled:          enabled,
		LogBodies:        truthy(get("LOG_BODIES")),
		LogMaxMB:         logMaxMB,
		DataDir:          dataDir,
		WorkerHealthPath: healthPath,
		HealthTimeoutMs:  healthTimeout,
		HealthCacheSec:   healthCache,
		GateMode:         gateMode,
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

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
