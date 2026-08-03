// Package gate holds repo-shape detection: the marker that says what kind of
// project a directory is, and the command that verifies it. It is the ONE home
// for that table — doctor's pre-flight rung and the worker engine both read it
// (#59). They used to keep byte-identical copies, which is a diagnosis waiting
// to go wrong: the day the two drift, doctor blesses a repo shape the worker
// cannot actually gate, and a green pre-flight means nothing.
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
	// Marker is the file whose presence identifies the shape. It may contain a
	// glob (`*.sln`); after detection it is rewritten to the concrete file that
	// matched, so a diagnosis names the real file rather than the pattern.
	Marker string
	Cmd    string
	Verify string
	LookIn []string

	// InRepo marks a command that ships INSIDE the repo rather than being
	// installed on the machine — a build-tool wrapper (`./gradlew`, `./mvnw`) or a
	// vendored binary (`vendor/bin/phpunit`). It is not looked for on PATH,
	// because PATH is not where it lives; the marker check already proved it is
	// there. Without this, every wrapper-based repo reports a missing toolchain.
	InRepo bool

	// RequireTarget, when set, demands that the marker file declare a make-style
	// target of that name. It is what makes the generic `make test` fallback
	// safe: a Makefile with no `test:` target answers `make test` with "No rule to
	// make target", which is the gate being inapplicable, not the code being
	// broken — and a wrong verdict is worse than a missing one.
	RequireTarget string
}

// gateTools is scanned top-down and the first marker found wins, so the order IS
// the specificity rule: a language's own build tool before a wrapper-agnostic
// one, a repo-local wrapper before the same tool on PATH, and the generic
// Makefile fallback last of all. A Rust repo that also ships a Makefile is a Rust
// repo; a Gradle repo that ships `gradlew` should use it, because that is the
// version the project pins.
//
// The bar for adding an entry is not "this ecosystem exists" but "this is the
// conventional gate AND it fails loudly when it does not apply". A command that
// silently passes on a repo it does not fit would turn the engine gate from
// evidence into noise. That is why there is no CMake entry (the build directory
// is a local convention, not a repo fact) and no bare composer.json entry (PHP
// has no single conventional test command; phpunit.xml is the fact that pins one).
var gateTools = []GateTool{
	{Marker: "go.mod", Cmd: "go", Verify: "go build ./... && go test ./...",
		LookIn: []string{"/usr/local/go/bin", "~/go/bin", "/usr/lib/go/bin", "/opt/homebrew/bin"}},
	{Marker: "Cargo.toml", Cmd: "cargo", Verify: "cargo test",
		LookIn: []string{"~/.cargo/bin"}},
	{Marker: "mix.exs", Cmd: "mix", Verify: "mix test",
		LookIn: []string{"/usr/local/bin", "/opt/homebrew/bin"}},
	{Marker: "Package.swift", Cmd: "swift", Verify: "swift test",
		LookIn: []string{"/usr/local/bin", "/usr/bin", "/opt/homebrew/bin"}},
	{Marker: "build.zig", Cmd: "zig", Verify: "zig build test",
		LookIn: []string{"/usr/local/bin", "/opt/homebrew/bin", "~/.local/bin"}},
	{Marker: "pyproject.toml", Cmd: "pytest", Verify: "pytest -q",
		LookIn: []string{".venv/bin", "venv/bin", "~/.local/bin"}},
	{Marker: "setup.py", Cmd: "pytest", Verify: "pytest -q",
		LookIn: []string{".venv/bin", "venv/bin", "~/.local/bin"}},

	// JVM: the repo-pinned wrapper outranks whatever version is on PATH.
	{Marker: "mvnw", Cmd: "./mvnw", Verify: "./mvnw -q -B test", InRepo: true},
	{Marker: "pom.xml", Cmd: "mvn", Verify: "mvn -q -B test",
		LookIn: []string{"/usr/local/bin", "/opt/homebrew/bin", "/opt/maven/bin"}},
	{Marker: "gradlew", Cmd: "./gradlew", Verify: "./gradlew test", InRepo: true},
	{Marker: "build.gradle", Cmd: "gradle", Verify: "gradle test",
		LookIn: []string{"/usr/local/bin", "/opt/homebrew/bin", "/opt/gradle/bin"}},
	{Marker: "build.gradle.kts", Cmd: "gradle", Verify: "gradle test",
		LookIn: []string{"/usr/local/bin", "/opt/homebrew/bin", "/opt/gradle/bin"}},

	{Marker: "*.sln", Cmd: "dotnet", Verify: "dotnet test",
		LookIn: []string{"/usr/local/share/dotnet", "~/.dotnet", "/opt/homebrew/bin"}},
	{Marker: "*.csproj", Cmd: "dotnet", Verify: "dotnet test",
		LookIn: []string{"/usr/local/share/dotnet", "~/.dotnet", "/opt/homebrew/bin"}},

	{Marker: "phpunit.xml", Cmd: "vendor/bin/phpunit", Verify: "vendor/bin/phpunit", InRepo: true},
	{Marker: "phpunit.xml.dist", Cmd: "vendor/bin/phpunit", Verify: "vendor/bin/phpunit", InRepo: true},

	{Marker: ".rspec", Cmd: "bundle", Verify: "bundle exec rspec",
		LookIn: []string{"~/.rbenv/shims", "~/.rvm/bin", "/usr/local/bin", "/opt/homebrew/bin"}},

	{Marker: "package.json", Cmd: "npm", Verify: "npm test",
		LookIn: []string{"/usr/local/bin", "/opt/homebrew/bin"}},
	{Marker: "deno.json", Cmd: "deno", Verify: "deno test -A",
		LookIn: []string{"~/.deno/bin", "/usr/local/bin", "/opt/homebrew/bin"}},
	{Marker: "deno.jsonc", Cmd: "deno", Verify: "deno test -A",
		LookIn: []string{"~/.deno/bin", "/usr/local/bin", "/opt/homebrew/bin"}},

	{Marker: "Rakefile", Cmd: "rake", Verify: "rake test",
		LookIn: []string{"~/.rbenv/shims", "~/.rvm/bin", "/usr/local/bin", "/opt/homebrew/bin"}},

	// Last, and language-agnostic: whatever the project is, if it declares a
	// `test` target then the project itself has said what its gate is.
	{Marker: "Makefile", Cmd: "make", Verify: "make test", RequireTarget: "test",
		LookIn: []string{"/usr/bin", "/usr/local/bin", "/opt/homebrew/bin"}},
	{Marker: "makefile", Cmd: "make", Verify: "make test", RequireTarget: "test",
		LookIn: []string{"/usr/bin", "/usr/local/bin", "/opt/homebrew/bin"}},
}

// DetectGateTool scans dir for known repo-shape markers and returns the matching
// tool entry with Marker set to the concrete file that matched, or
// (GateTool{}, false) when none is found.
func DetectGateTool(dir string) (GateTool, bool) {
	for _, t := range gateTools {
		found, ok := t.match(dir)
		if !ok {
			continue
		}
		t.Marker = found
		return t, true
	}
	return GateTool{}, false
}

// match reports whether this tool's marker is present in dir, and under what
// concrete filename. A glob marker resolves to the first match in sorted order
// so detection is deterministic across machines.
func (t GateTool) match(dir string) (string, bool) {
	name := ""
	if strings.ContainsAny(t.Marker, "*?[") {
		hits, err := filepath.Glob(filepath.Join(dir, t.Marker))
		if err != nil || len(hits) == 0 {
			return "", false
		}
		name = filepath.Base(hits[0])
	} else {
		if _, err := os.Stat(filepath.Join(dir, t.Marker)); err != nil {
			return "", false
		}
		name = t.Marker
	}
	if t.RequireTarget != "" && !declaresTarget(filepath.Join(dir, name), t.RequireTarget) {
		return "", false
	}
	return name, true
}

// declaresTarget reports whether a makefile declares the named target. It reads
// the file rather than asking make, because asking make means running make, and
// this decision is made before rig is willing to run anything.
//
// The check is deliberately shallow: a rule is a line that starts at column zero
// with the target name followed by a colon. Targets produced by an include or a
// generated fragment are missed, and that is the safe direction to be wrong in —
// a missed target means no engine gate, not a wrong verdict.
func declaresTarget(makefile, target string) bool {
	data, err := os.ReadFile(makefile)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		name, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		// `test::` (double-colon rules) and `test: deps` both count; `testdata:`
		// does not, and neither does a variable assignment like `test := x`.
		if strings.TrimSpace(name) != target {
			continue
		}
		if strings.HasPrefix(rest, "=") {
			continue
		}
		return true
	}
	return false
}

// FindOffPath looks for the command in the usual install locations, so the report
// can name the directory to add instead of telling the user to install something
// they already have. Relative candidates (a project's .venv/bin) resolve against
// the REPO, not the process working directory — the worker runs in the repo, and
// doctor may not. A candidate that exists but is not executable is skipped, never
// reported.
//
// An InRepo command has no off-PATH story to tell: it lives in the repo by
// definition, so there is nothing to go looking for.
func FindOffPath(t GateTool, repo string) string {
	if t.InRepo {
		return ""
	}
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

// Available reports whether this tool's command can actually be run for repo,
// and where it resolved to. For an InRepo command that means an executable file
// inside the repo; for everything else it means PATH — the distinction #35 is
// about, since the worker inherits PATH from whatever spawned it.
func (t GateTool) Available(repo string, lookPath func(string) (string, error)) (string, bool) {
	if t.InRepo {
		p := filepath.Join(repo, filepath.FromSlash(strings.TrimPrefix(t.Cmd, "./")))
		st, err := os.Stat(p)
		if err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
			return "", false
		}
		return p, true
	}
	p, err := lookPath(t.Cmd)
	if err != nil {
		return "", false
	}
	return p, true
}
