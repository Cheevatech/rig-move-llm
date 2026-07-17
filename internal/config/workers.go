package config

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// WorkersFile is the optional multi-endpoint fallback chain inside a scope dir.
// When present it replaces the scalar WORKER_* config wholesale (no per-entry
// merge); the local file wins over the global one the same way config.env does.
// It is never auto-created: it may carry API keys, so writing it is the user's
// explicit act (chmod 0600).
const WorkersFile = "workers.json"

// WorkerEndpoint is one entry in the worker fallback chain. Endpoints are tried
// in ascending Priority order (ties keep file order); a transport error, header
// timeout, 408/429 or 5xx moves on to the next entry. A Passthrough entry
// instead sends the request to the paid Anthropic main upstream — an honest
// last resort that burns real quota while the workers are down.
type WorkerEndpoint struct {
	Name        string `json:"name,omitempty"`
	Base        string `json:"base,omitempty"`
	Model       string `json:"model,omitempty"`
	Key         string `json:"key,omitempty"`
	BackendName string `json:"backend,omitempty"`
	Priority    int    `json:"priority,omitempty"`
	Passthrough bool   `json:"passthrough,omitempty"`

	Backend Backend `json:"-"` // resolved from BackendName
}

// Label identifies the endpoint in stats, logs and health tracking.
func (e WorkerEndpoint) Label() string {
	if e.Name != "" {
		return e.Name
	}
	if e.Passthrough {
		return "passthrough"
	}
	return e.Base
}

// workersDoc is the top-level shape of workers.json. An object (not a bare
// array) so the format can grow fields without breaking existing files.
type workersDoc struct {
	Endpoints []WorkerEndpoint `json:"endpoints"`
}

// loadWorkers reads the first usable workers.json (local scope first, then
// global) and returns the resolved, priority-sorted chain. A missing file
// yields nil; a malformed or empty one is skipped with a warning so the scalar
// single-worker config still applies (fail-safe — a broken JSON edit must not
// take the whole worker leg down).
func loadWorkers(projectDir string) []WorkerEndpoint {
	var paths []string
	if projectDir != "" {
		paths = append(paths, filepath.Join(projectDir, DirName, WorkersFile))
	}
	paths = append(paths, filepath.Join(GlobalDir(), WorkersFile))

	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var doc workersDoc
		if err := json.Unmarshal(data, &doc); err != nil {
			log.Printf("config: ignoring malformed %s: %v", p, err)
			continue
		}
		eps := resolveWorkers(doc.Endpoints, p)
		if len(eps) == 0 {
			continue
		}
		checkWorkersPerms(p, eps)
		return eps
	}
	return nil
}

// resolveWorkers fills in backend defaults, drops unusable entries, and sorts
// by ascending Priority (stable, so equal priorities keep file order).
func resolveWorkers(eps []WorkerEndpoint, path string) []WorkerEndpoint {
	out := make([]WorkerEndpoint, 0, len(eps))
	for _, e := range eps {
		if e.Passthrough {
			out = append(out, e)
			continue
		}
		e.Backend = LookupBackend(e.BackendName)
		e.Base = strings.TrimRight(e.Base, "/")
		if e.Base == "" {
			e.Base = strings.TrimRight(e.Backend.DefaultBase, "/")
		}
		if e.Base == "" {
			log.Printf("config: %s: skipping endpoint %q — no base URL and backend %q has no default", path, e.Label(), e.BackendName)
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out
}

// checkWorkersPerms warns when a workers.json carrying API keys is readable by
// group/others (it should be chmod 0600). Warn-only: blocking the whole worker
// leg over file modes would trade a leak warning for a dead session. No POSIX
// permission bits on Windows.
func checkWorkersPerms(path string, eps []WorkerEndpoint) {
	if runtime.GOOS == "windows" {
		return
	}
	hasKey := false
	for _, e := range eps {
		if e.Key != "" {
			hasKey = true
			break
		}
	}
	if !hasKey {
		return
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		log.Printf("config: %s contains API keys but is group/world readable — run: chmod 0600 %s", path, path)
	}
}
