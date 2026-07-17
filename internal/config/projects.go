// projects.json is the fail-closed allowlist of project directories the global
// daemon may serve per-project config for. `init` (local scope) registers the
// project; the daemon reads the file fresh on every prefixed request and denies
// anything not listed. It lives in our own data dir (like direnv's grant store) —
// never in ~/.claude.json — because the reader is our daemon, not Claude, and
// "opened the repo in Claude" must not imply "opted into a rig override".
package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// ProjectsFile is the allowlist file inside the global scope dir.
const ProjectsFile = "projects.json"

// ProjectsPath returns ~/.rig-move-llm/projects.json.
func ProjectsPath() string {
	return filepath.Join(GlobalDir(), ProjectsFile)
}

type projectsFile struct {
	Version  int      `json:"version"`
	Projects []string `json:"projects"`
}

// LoadProjects reads the allowlist fresh from disk. A missing or unreadable
// file yields an empty list (fail-closed for the daemon's membership check).
func LoadProjects() []string {
	data, err := os.ReadFile(ProjectsPath())
	if err != nil {
		return nil
	}
	var pf projectsFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return nil
	}
	return pf.Projects
}

// ProjectAllowed reports whether the canonical project dir is registered.
func ProjectAllowed(canonical string) bool {
	for _, p := range LoadProjects() {
		if p == canonical {
			return true
		}
	}
	return false
}

// RegisterProject adds a canonical project dir to the allowlist (idempotent,
// atomic tmp+rename write, like the stats ledger flush).
func RegisterProject(canonical string) error {
	pf := projectsFile{Version: 1, Projects: LoadProjects()}
	for _, p := range pf.Projects {
		if p == canonical {
			return nil
		}
	}
	pf.Projects = append(pf.Projects, canonical)
	return writeProjects(pf)
}

// UnregisterProject removes a canonical project dir from the allowlist
// (idempotent — a no-op when absent).
func UnregisterProject(canonical string) error {
	old := LoadProjects()
	kept := make([]string, 0, len(old))
	for _, p := range old {
		if p != canonical {
			kept = append(kept, p)
		}
	}
	if len(kept) == len(old) {
		return nil
	}
	return writeProjects(projectsFile{Version: 1, Projects: kept})
}

func writeProjects(pf projectsFile) error {
	if err := os.MkdirAll(GlobalDir(), 0o755); err != nil {
		return err
	}
	out, err := json.MarshalIndent(pf, "", "  ")
	if err != nil {
		return err
	}
	path := ProjectsPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// CanonicalPath resolves dir to its canonical absolute form (symlinks resolved,
// no trailing slash). Both `run`/`init` and the daemon canonicalize through this
// so allowlist membership is an exact string match.
func CanonicalPath(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// EncodeProjectID encodes a canonical project dir for use in the daemon's
// /p/<id>/... base URL path prefix. base64url is bijective — no registry, no
// collisions; an unknown or stale id simply fails the allowlist check.
func EncodeProjectID(canonical string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(canonical))
}

// DecodeProjectID reverses EncodeProjectID, rejecting non-UTF-8 and relative
// results so a malformed prefix can never name a surprising path.
func DecodeProjectID(id string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", err
	}
	dir := string(b)
	if !utf8.ValidString(dir) || !filepath.IsAbs(dir) {
		return "", errors.New("project id does not decode to an absolute path")
	}
	return dir, nil
}
