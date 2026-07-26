package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testdata/pointers holds a REAL `git diff` (point.diff) plus the post-change
// working tree it was produced from. The fixture is deliberately shaped like the
// cases git's own hunk headers get wrong (see B1 audit): one hunk spanning three
// symbols, a nested def, and a module-level edit.
func loadPointerFixture(t *testing.T) (repo, diff string) {
	t.Helper()
	repo = filepath.Join("testdata", "pointers")
	b, err := os.ReadFile(filepath.Join(repo, "point.diff"))
	if err != nil {
		t.Fatalf("read fixture diff: %v", err)
	}
	return repo, string(b)
}

func TestPointersOnRealDiff(t *testing.T) {
	repo, diff := loadPointerFixture(t)
	got := Pointers(repo, diff)

	want := []Pointer{
		{File: "helpers.py", Line: 1, Symbol: "simplify", Signature: "def simplify(expr):", Kind: "add", Added: 2},
		{File: "point.py", Line: 5, Symbol: "", Signature: "", Kind: "mod", Added: 1, Removed: 1},
		{File: "point.py", Line: 27, Symbol: "Point.__mul__", Signature: "def __mul__(self, factor):", Kind: "mod", Added: 2, Removed: 2},
		{File: "point.py", Line: 36, Symbol: "Point._normalize.pad", Signature: "def pad(seq, n):", Kind: "mod", Added: 2},
		{File: "point.py", Line: 48, Symbol: "Point2D.scale", Signature: "def scale(self, factor):", Kind: "add", Added: 3},
		{File: "point.py", Line: 53, Symbol: "load_points", Signature: "async def load_points(path):", Kind: "mod", Added: 1},
		{File: "point.py", Line: 59, Symbol: "top_level", Signature: "def top_level():", Kind: "mod", Added: 1, Removed: 2},
	}

	if len(got) != len(want) {
		t.Fatalf("pointer count = %d, want %d\ngot: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pointer[%d]\n got  %+v\n want %+v", i, got[i], want[i])
		}
	}
}

// The pointer layer is the "ย่อ" tier: it must name what changed without ever
// carrying a line of the change itself (R4 §3 / R9 §3).
func TestPointersCarryNoBody(t *testing.T) {
	repo, diff := loadPointerFixture(t)
	// Bodies present in the diff that must not survive into the pointer list.
	bodies := []string{
		"simplify(x * factor)",
		"evaluate=False",
		"return list(seq)",
		"await _warm(path)",
		`{"origin": None}`,
	}
	for _, p := range Pointers(repo, diff) {
		blob := p.File + p.Symbol + p.Signature + p.Kind
		for _, b := range bodies {
			if strings.Contains(blob, b) {
				t.Errorf("pointer %+v leaked body %q", p, b)
			}
		}
	}
}

// Anything that is not Python, or that we cannot resolve, degrades to a bare
// file:line pointer rather than a guess (carry: wrong name = soft failure,
// no name = graceful).
func TestPointersDegradeGracefully(t *testing.T) {
	repo, _ := loadPointerFixture(t)
	diff := `diff --git a/README.md b/README.md
index 111..222 100644
--- a/README.md
+++ b/README.md
@@ -1,2 +1,2 @@
 title
-old line
+new line
`
	got := Pointers(repo, diff)
	if len(got) != 1 {
		t.Fatalf("got %d pointers, want 1: %+v", len(got), got)
	}
	if got[0].Symbol != "" || got[0].Signature != "" {
		t.Errorf("non-Python file should yield a bare pointer, got %+v", got[0])
	}
	if got[0].File != "README.md" || got[0].Line != 2 || got[0].Kind != "mod" {
		t.Errorf("bare pointer wrong: %+v", got[0])
	}
}

// Deleted files have no post-change content to resolve against; the pointer
// still has to say the file is gone.
func TestPointersDeletedFile(t *testing.T) {
	repo, _ := loadPointerFixture(t)
	diff := `diff --git a/gone.py b/gone.py
deleted file mode 100644
index 111..0000000
--- a/gone.py
+++ /dev/null
@@ -1,3 +0,0 @@
-def vanished():
-    return 1
-
`
	got := Pointers(repo, diff)
	if len(got) != 1 {
		t.Fatalf("got %d pointers, want 1: %+v", len(got), got)
	}
	if got[0].File != "gone.py" || got[0].Kind != "del" || got[0].Removed != 3 {
		t.Errorf("deleted-file pointer wrong: %+v", got[0])
	}
}

// The scanner is the whole reason we are not using git's hunk headers: it must
// pick the decl that *encloses* the line by indent, not the last decl textually
// above it. Both cases below are ones git answers wrong (B1).
func TestEnclosingSymbolBeatsGitHunkHeader(t *testing.T) {
	src := strings.Split(`class Foo:
    def alpha(self):
        z = 42
        return z

    def beta(self):
        return 1


def top_level():
    c = 42
    return c
`, "\n")

	for _, tc := range []struct {
		name    string
		line    int
		symbol  string
		sigHead string
	}{
		{"inside nested method", 3, "Foo.alpha", "def alpha(self):"}, // git says "class Foo:"
		{"module-level func", 11, "top_level", "def top_level():"},   // git (diff=python) says "def beta(self):"
		{"the decl line itself", 2, "Foo.alpha", "def alpha(self):"}, //
		{"class body attribute", 1, "Foo", "class Foo:"},             //
	} {
		t.Run(tc.name, func(t *testing.T) {
			sym, sig := enclosingSymbol(src, tc.line)
			if sym != tc.symbol || sig != tc.sigHead {
				t.Errorf("line %d → (%q, %q), want (%q, %q)", tc.line, sym, sig, tc.symbol, tc.sigHead)
			}
		})
	}
}
