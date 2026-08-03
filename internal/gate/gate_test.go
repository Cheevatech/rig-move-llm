package gate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The table is the product's answer to "what is this repo's gate". Before #59 it
// knew four ecosystems, which happened to be the four in the M1 volley — so a
// Java, Ruby, Elixir, PHP, Swift or .NET repo got no engine gate at all and the
// caller was left with the worker's own claim about its own work.
func TestDetectGateToolCoversTheCommonEcosystems(t *testing.T) {
	cases := []struct {
		name       string
		marker     string
		wantVerify string
	}{
		{"go", "go.mod", "go build ./... && go test ./..."},
		{"rust", "Cargo.toml", "cargo test"},
		{"elixir", "mix.exs", "mix test"},
		{"swift", "Package.swift", "swift test"},
		{"zig", "build.zig", "zig build test"},
		{"python pyproject", "pyproject.toml", "pytest -q"},
		{"python setup.py", "setup.py", "pytest -q"},
		{"maven", "pom.xml", "mvn -q -B test"},
		{"gradle groovy", "build.gradle", "gradle test"},
		{"gradle kotlin", "build.gradle.kts", "gradle test"},
		{"phpunit", "phpunit.xml", "vendor/bin/phpunit"},
		{"rspec", ".rspec", "bundle exec rspec"},
		{"node", "package.json", "npm test"},
		{"deno", "deno.json", "deno test -A"},
		{"ruby rake", "Rakefile", "rake test"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, c.marker, "")
			tool, ok := DetectGateTool(dir)
			if !ok {
				t.Fatalf("%s was not recognised as a repo shape", c.marker)
			}
			if tool.Verify != c.wantVerify {
				t.Errorf("verify = %q, want %q", tool.Verify, c.wantVerify)
			}
			if tool.Marker != c.marker {
				t.Errorf("marker = %q, want the concrete file %q", tool.Marker, c.marker)
			}
		})
	}
}

// A glob marker must report the file that actually matched, not the pattern —
// a diagnosis naming `*.sln` tells the user nothing about their repo.
func TestGlobMarkerReportsTheConcreteFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "MyApp.sln", "")
	tool, ok := DetectGateTool(dir)
	if !ok {
		t.Fatal("a .sln repo was not recognised")
	}
	if tool.Marker != "MyApp.sln" {
		t.Errorf("marker = %q, want the matched file MyApp.sln", tool.Marker)
	}
	if tool.Verify != "dotnet test" {
		t.Errorf("verify = %q, want dotnet test", tool.Verify)
	}
}

// Order is the specificity rule, and it is load-bearing in both directions.
func TestDetectionOrderIsSpecificity(t *testing.T) {
	t.Run("a repo-pinned wrapper beats the same tool on PATH", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "build.gradle", "")
		write(t, dir, "gradlew", "#!/bin/sh\n")
		tool, _ := DetectGateTool(dir)
		if tool.Verify != "./gradlew test" {
			t.Errorf("verify = %q, want the wrapper the project pins", tool.Verify)
		}
		if !tool.InRepo {
			t.Error("a wrapper lives in the repo, not on PATH — InRepo must be set")
		}
	})

	t.Run("a language marker beats the generic Makefile fallback", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "Cargo.toml", "[package]\n")
		write(t, dir, "Makefile", "test:\n\techo hi\n")
		tool, _ := DetectGateTool(dir)
		if tool.Verify != "cargo test" {
			t.Errorf("verify = %q — a Rust repo that also ships a Makefile is a Rust repo", tool.Verify)
		}
	})

	t.Run("mvnw beats pom.xml", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "pom.xml", "")
		write(t, dir, "mvnw", "#!/bin/sh\n")
		if tool, _ := DetectGateTool(dir); tool.Verify != "./mvnw -q -B test" {
			t.Errorf("verify = %q, want the wrapper", tool.Verify)
		}
	})
}

// The Makefile fallback is what makes the table language-agnostic, and it is
// also the entry most able to produce a WRONG verdict — `make test` on a
// Makefile with no test target exits non-zero for a reason that has nothing to
// do with the change. So the target has to be there before the gate is claimed.
func TestMakefileFallbackRequiresATestTarget(t *testing.T) {
	t.Run("a test target is the gate, whatever the language", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "Makefile", "all: build\n\ntest: build\n\t./run-tests\n")
		tool, ok := DetectGateTool(dir)
		if !ok || tool.Verify != "make test" {
			t.Fatalf("ok=%v verify=%q, want make test", ok, tool.Verify)
		}
	})

	t.Run("lowercase makefile counts too", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "makefile", "test:\n\t./run-tests\n")
		if _, ok := DetectGateTool(dir); !ok {
			t.Error("a lowercase makefile is still a makefile")
		}
	})

	t.Run("no test target means no gate, not a wrong gate", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "Makefile", "all: build\nbuild:\n\tgo build\n")
		if tool, ok := DetectGateTool(dir); ok {
			t.Errorf("claimed gate %q on a Makefile with no test target", tool.Verify)
		}
	})

	t.Run("a lookalike target does not count", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "Makefile", "testdata:\n\tmkdir testdata\n")
		if _, ok := DetectGateTool(dir); ok {
			t.Error("`testdata:` is not `test:`")
		}
	})

	t.Run("a variable assignment does not count", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "Makefile", "test := go test\nall:\n\t$(test)\n")
		if _, ok := DetectGateTool(dir); ok {
			t.Error("`test :=` is an assignment, not a rule")
		}
	})

	t.Run("a recipe line that mentions test does not count", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "Makefile", "all:\n\ttest: not a rule\n")
		if _, ok := DetectGateTool(dir); ok {
			t.Error("an indented recipe line is not a rule declaration")
		}
	})
}

func TestUnknownShapeIsReportedAsUnknown(t *testing.T) {
	if tool, ok := DetectGateTool(t.TempDir()); ok {
		t.Errorf("invented a gate (%q) for a directory with no markers", tool.Verify)
	}
}

// #35's distinction, extended to wrappers: "can the worker actually run this?"
// For a normal tool that means PATH; for a repo-local wrapper PATH is the wrong
// question entirely, and asking it reported a missing toolchain for every
// wrapper-based repo.
func TestAvailable(t *testing.T) {
	notOnPath := func(string) (string, error) { return "", errors.New("not found") }

	t.Run("a repo wrapper is found in the repo, not on PATH", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "gradlew", "#!/bin/sh\n")
		if err := os.Chmod(filepath.Join(dir, "gradlew"), 0o755); err != nil {
			t.Fatal(err)
		}
		tool, _ := DetectGateTool(dir)
		p, ok := tool.Available(dir, notOnPath)
		if !ok {
			t.Fatal("an executable wrapper in the repo must count as available")
		}
		if p != filepath.Join(dir, "gradlew") {
			t.Errorf("resolved to %q, want the in-repo wrapper", p)
		}
	})

	t.Run("a wrapper without the executable bit is not available", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "gradlew", "#!/bin/sh\n") // 0644
		tool, _ := DetectGateTool(dir)
		if _, ok := tool.Available(dir, notOnPath); ok {
			t.Error("a non-executable wrapper cannot run the gate")
		}
	})

	t.Run("a vendored binary is resolved under the repo", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "phpunit.xml", "")
		write(t, dir, "vendor/bin/phpunit", "#!/bin/sh\n")
		if err := os.Chmod(filepath.Join(dir, "vendor", "bin", "phpunit"), 0o755); err != nil {
			t.Fatal(err)
		}
		tool, _ := DetectGateTool(dir)
		if _, ok := tool.Available(dir, notOnPath); !ok {
			t.Error("vendor/bin/phpunit is in the repo and executable — it is available")
		}
	})

	t.Run("a normal tool still asks PATH", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "go.mod", "module x\n")
		tool, _ := DetectGateTool(dir)
		if _, ok := tool.Available(dir, notOnPath); ok {
			t.Error("go is not on this PATH; it must not report available")
		}
		found := func(string) (string, error) { return "/usr/local/go/bin/go", nil }
		if p, ok := tool.Available(dir, found); !ok || p != "/usr/local/go/bin/go" {
			t.Errorf("Available = %q,%v — want the PATH resolution", p, ok)
		}
	})
}

// A wrapper lives in the repo by definition, so there is no "installed
// elsewhere" story to tell about it, and telling one would send the user
// hunting for something that is already there.
func TestFindOffPathSkipsInRepoTools(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "gradlew", "#!/bin/sh\n")
	tool, _ := DetectGateTool(dir)
	if got := FindOffPath(tool, dir); got != "" {
		t.Errorf("FindOffPath = %q, want empty for an in-repo command", got)
	}
}
