// Package gate holds repo-shape detection logic shared by the doctor
// pre-flight ladder and the worker engine (both need it, and the cycle
// cli->worker->cli makes a single home impossible).
package gate

import (
	"os"
	"path/filepath"
	"strings"
)

// GateTool is a repo-shape marker paired with the command that verifies that
// shape, plus the places the command commonly lives when it is installed but not
// on PATH.
type GateTool struct {
	Marker string
	Cmd    string
	Verify string
	LookIn []string
}

var gateTools = []GateTool{
	{Marker: "go.mod", Cmd: "go", Verify: "go build ./... && go test ./...",
		LookIn: []string{"/usr/local/go/bin", "~/go/bin", "/usr/lib/go/bin", "/opt/homebrew/bin"}},
	{Marker: "pyproject.toml", Cmd: "pytest", Verify: "pytest -q",
		LookIn: []string{".venv/bin", "venv/bin", "~/.local/bin"}},
	{Marker: "setup.py", Cmd: "pytest", Verify: "pytest -q",
		LookIn: []string{".venv/bin", "venv/bin", "~/.local/bin"}},
	{Marker: "package.json", Cmd: "npm", Verify: "npm test",
		LookIn: []string{"/usr/local/bin", "/opt/homebrew/bin"}},
	{Marker: "Cargo.toml", Cmd: "cargo", Verify: "cargo test",
		LookIn: []string{"~/.cargo/bin"}},
}

// DetectGateTool scans dir for known repo-shape markers (go.mod, pyproject.toml,
// etc.) and returns the matching tool entry, or (false) when none is found.
func DetectGateTool(dir string) (GateTool, bool) {
	for _, t := range gateTools {
		if _, err := os.Stat(filepath.Join(dir, t.Marker)); err == nil {
			return t, true
		}
	}
	return GateTool{}, false
}

// FindOffPath looks for the command in the usual install locations, so the report
// can name the directory to add instead of telling the user to install something
// they already have. Relative candidates (a project's .venv/bin) resolve against
// the REPO, not the process working directory — the worker runs in the repo, and
// doctor may not.
func FindOffPath(t GateTool, repo string) string {
	home, _ := os.UserHomeDir()
	for _, dir := range t.LookIn {
		switch {
		case strings.HasPrefix(dir, "~/"):
			if home == "" {
				continue
			}
			dir = filepath.Join(home, dir[2:])
		case !filepath.IsAbs(dir):
			dir = filepath.Join(repo, dir)
		}
		p := filepath.Join(dir, t.Cmd)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}
