package worker

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Pointer is one entry of the tiered-return manifest: it names *what* changed
// without carrying any of the change itself. The raw substrate stays in the
// target repo's git working tree, so a pointer is lossless in the only sense
// that matters here — MAIN can drill back to the bytes through it (R4 §3, R9 §3).
type Pointer struct {
	File      string `json:"file"`
	Line      int    `json:"line"` // first changed line, post-change numbering
	Symbol    string `json:"symbol,omitempty"`
	Signature string `json:"signature,omitempty"`
	Kind      string `json:"kind"` // add | mod | del
	Added     int    `json:"added"`
	Removed   int    `json:"removed"`
}

// Pointers turns a unified `git diff` of repo into the manifest's pointer list.
//
// Resolution is per changed *line*, not per hunk: git's own hunk header names
// the last decl textually above the hunk without looking at indentation, so it
// reports the enclosing class for a method edit and goes stale across def
// boundaries (measured in B1). A single hunk routinely spans several symbols.
//
// Accuracy is best-effort by design. Drill fetches by file plus line range, not
// by symbol name, so a mislabelled symbol is a bad caption, not a broken fetch —
// and when the scanner cannot resolve a line it emits a bare file:line pointer
// rather than a guess.
func Pointers(repo, diff string) []Pointer {
	var out []Pointer
	src := newSourceCache(repo)

	for _, f := range parseDiffFiles(diff) {
		if f.deleted {
			out = append(out, Pointer{File: f.path, Line: 1, Kind: "del", Removed: f.removed})
			continue
		}
		lines := src.get(f.path)

		// Group changed lines by the symbol enclosing them; a symbol touched by
		// two hunks is one pointer.
		type group struct {
			Pointer
			declLine int
		}
		var order []string
		groups := map[string]*group{}
		for _, ch := range f.changes {
			sym, sig, decl := resolveSymbol(lines, ch.line)
			key := sym + "\x00" + strconv.Itoa(decl)
			g := groups[key]
			if g == nil {
				g = &group{Pointer: Pointer{
					File: f.path, Line: ch.line, Symbol: sym, Signature: sig, Kind: "mod",
				}, declLine: decl}
				groups[key] = g
				order = append(order, key)
			}
			if ch.line < g.Line || g.Line == 0 {
				g.Line = ch.line
			}
			if ch.added {
				g.Added++
			} else {
				g.Removed++
			}
		}

		for _, key := range order {
			g := groups[key]
			switch {
			case f.isNew || (g.declLine > 0 && f.addedLines[g.declLine]):
				g.Kind = "add" // the declaration itself is new
			case g.Added == 0 && g.Removed > 0:
				g.Kind = "del"
			}
			out = append(out, g.Pointer)
		}
	}
	return out
}

// --- unified diff parsing -------------------------------------------------

type diffChange struct {
	line  int // post-change line number ("-" lines take the position they left)
	added bool
}

type diffFile struct {
	path       string
	isNew      bool
	deleted    bool
	inHunk     bool
	removed    int
	changes    []diffChange
	addedLines map[int]bool
}

func parseDiffFiles(diff string) []*diffFile {
	var files []*diffFile
	var cur *diffFile
	newLine := 0

	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			cur = &diffFile{path: gitDiffPath(l), addedLines: map[int]bool{}}
			files = append(files, cur)
			newLine = 0
		case cur == nil:
			// preamble noise
		case strings.HasPrefix(l, "new file mode"):
			cur.isNew = true
		case strings.HasPrefix(l, "deleted file mode"), l == "+++ /dev/null":
			cur.deleted = true
		case strings.HasPrefix(l, "@@"):
			cur.inHunk = true
			newLine = hunkNewStart(l)
		case strings.HasPrefix(l, "\\"):
			// "\ No newline at end of file"
		case strings.HasPrefix(l, "-") && cur.inHunk:
			// Counted even for a deleted file, whose new side has no lines at all.
			cur.removed++
			if newLine > 0 {
				cur.changes = append(cur.changes, diffChange{line: newLine})
			}
		case newLine == 0:
			// header noise, or a hunk with no new side
		case strings.HasPrefix(l, "+"):
			cur.changes = append(cur.changes, diffChange{line: newLine, added: true})
			cur.addedLines[newLine] = true
			newLine++
		default: // context line (leading space, or the empty tail line)
			newLine++
		}
	}
	return files
}

// gitDiffPath pulls the post-change path out of a `diff --git a/x b/x` header.
func gitDiffPath(header string) string {
	rest := strings.TrimPrefix(header, "diff --git ")
	i := strings.Index(rest, " b/")
	if i < 0 {
		return strings.TrimPrefix(rest, "a/")
	}
	return strings.TrimPrefix(rest[i+1:], "b/")
}

// hunkNewStart reads C out of "@@ -a,b +c,d @@ …". Returns 0 when unparseable
// or when the new side is empty (a deleted file), which suppresses resolution.
func hunkNewStart(l string) int {
	i := strings.Index(l, "+")
	if i < 0 {
		return 0
	}
	num := l[i+1:]
	if j := strings.IndexAny(num, ", @"); j >= 0 {
		num = num[:j]
	}
	n, err := strconv.Atoi(num)
	if err != nil {
		return 0
	}
	return n
}

// --- the scanner ----------------------------------------------------------

// resolveSymbol is enclosingSymbol plus the line the innermost decl sits on,
// which Pointers needs to tell a brand-new symbol from an edited one.
func resolveSymbol(lines []string, line int) (symbol, signature string, declLine int) {
	if len(lines) == 0 || line < 1 || line > len(lines) {
		return "", "", 0
	}
	// Baseline: the indentation of the block the line lives in. Blank lines
	// carry none, so borrow it from the nearest code line above.
	i := line - 1
	for i >= 0 && strings.TrimSpace(lines[i]) == "" {
		i--
	}
	if i < 0 {
		return "", "", 0
	}

	limit := indentOf(lines[i])
	var names []string
	if name, ok := declName(lines[i]); ok {
		names = append(names, name)
		signature = strings.TrimSpace(lines[i])
		declLine = i + 1
		if limit == 0 {
			return name, signature, declLine
		}
		i--
	}

	// Walk up collecting each decl that opens a strictly shallower block: in
	// Python indentation *is* the block structure, which is exactly what git's
	// hunk header ignores.
	for ; i >= 0 && limit > 0; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		ind := indentOf(lines[i])
		if ind >= limit {
			continue
		}
		name, ok := declName(lines[i])
		if !ok {
			continue
		}
		names = append(names, name)
		if signature == "" {
			signature = strings.TrimSpace(lines[i])
			declLine = i + 1
		}
		limit = ind
	}

	for l, r := 0, len(names)-1; l < r; l, r = l+1, r-1 {
		names[l], names[r] = names[r], names[l]
	}
	return strings.Join(names, "."), signature, declLine
}

// enclosingSymbol returns the dotted path of the Python symbol containing the
// given 1-based line, plus that symbol's declaration line, or ("", "") when the
// line sits at module level or cannot be resolved.
func enclosingSymbol(lines []string, line int) (symbol, signature string) {
	s, sig, _ := resolveSymbol(lines, line)
	return s, sig
}

func indentOf(l string) int {
	n := 0
	for _, r := range l {
		switch r {
		case ' ':
			n++
		case '\t':
			n += 4
		default:
			return n
		}
	}
	return n
}

// declName reports the name a `def` / `async def` / `class` line declares.
func declName(l string) (string, bool) {
	t := strings.TrimSpace(l)
	t = strings.TrimPrefix(t, "async ")
	for _, kw := range []string{"def ", "class "} {
		if !strings.HasPrefix(t, kw) {
			continue
		}
		name := strings.TrimSpace(t[len(kw):])
		if i := strings.IndexAny(name, "(: \t"); i >= 0 {
			name = name[:i]
		}
		if name == "" {
			return "", false
		}
		return name, true
	}
	return "", false
}

// --- source access --------------------------------------------------------

// sourceCache reads post-change files off disk once each. Only Python resolves
// today; every other extension degrades to a bare file:line pointer (multi-lang
// is deferred behind the R13 gate).
type sourceCache struct {
	repo  string
	files map[string][]string
}

func newSourceCache(repo string) *sourceCache {
	return &sourceCache{repo: repo, files: map[string][]string{}}
}

func (c *sourceCache) get(rel string) []string {
	if lines, ok := c.files[rel]; ok {
		return lines
	}
	var lines []string
	if strings.EqualFold(filepath.Ext(rel), ".py") {
		if abs, err := safeJoin(c.repo, rel); err == nil {
			if b, err := os.ReadFile(abs); err == nil {
				lines = strings.Split(string(b), "\n")
			}
		}
	}
	c.files[rel] = lines
	return lines
}
