package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Anchor is one deterministically-derived candidate file for a task: the
// coverage gate (0b) requires the explore worker to actually read the top
// anchors before its report is accepted.
type Anchor struct {
	Path  string   `json:"path"`
	Score float64  `json:"score"`
	Hits  int      `json:"hits"`
	Via   []string `json:"via"` // which task identifiers matched
}

// identRe matches identifier-like tokens in free task text.
var identRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{2,}`)

// stopwords are English/prompt words that match identRe but carry no anchor
// signal. Lowercased.
var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true, "this": true,
	"from": true, "not": true, "are": true, "should": true, "when": true, "where": true,
	"which": true, "will": true, "would": true, "could": true, "does": true, "into": true,
	"has": true, "have": true, "was": true, "were": true, "been": true, "being": true,
	"but": true, "can": true, "cannot": true, "all": true, "any": true, "than": true,
	"then": true, "there": true, "here": true, "how": true, "why": true, "what": true,
	"who": true, "its": true, "also": true, "only": true, "may": true, "must": true,
	"use": true, "used": true, "using": true, "instead": true, "like": true, "such": true,
	"you": true, "your": true, "our": true, "one": true, "two": true, "new": true,
	"add": true, "fix": true, "bug": true, "issue": true, "error": true, "errors": true,
	"fail": true, "fails": true, "failed": true, "failing": true, "test": true,
	"tests": true, "make": true, "makes": true, "made": true, "code": true, "file": true,
	"files": true, "line": true, "lines": true, "change": true, "changes": true,
	"following": true, "example": true, "expected": true, "actual": true, "result": true,
	"results": true, "instance": true, "case": true, "cases": true, "value": true,
	"values": true, "return": true, "returns": true, "returned": true, "call": true,
	"called": true, "calling": true, "work": true, "works": true, "working": true,
	"does_not": true, "doesn": true, "don": true, "get": true, "gets": true, "set": true,
	"sets": true, "run": true, "running": true, "output": true, "input": true,
	"python": true, "version": true, "current": true, "correct": true, "incorrect": true,
	"behavior": true, "behaviour": true, "problem": true, "description": true,
	"however": true, "because": true, "after": true, "before": true, "same": true,
	"different": true, "first": true, "second": true, "some": true, "more": true,
	"most": true, "each": true, "other": true, "still": true, "just": true, "even": true,
}

// TaskIdentifiers extracts up to max identifier-like tokens from a task text,
// preferring code-shaped tokens (snake_case, CamelCase, dotted names) over plain
// words. Deterministic: same text, same list.
func TaskIdentifiers(task string, max int) []string {
	type cand struct {
		tok   string
		score float64
	}
	seen := map[string]*cand{}
	order := []string{}
	for _, tok := range identRe.FindAllString(task, -1) {
		lower := strings.ToLower(tok)
		if stopwords[lower] || len(tok) < 3 {
			continue
		}
		c, ok := seen[tok]
		if !ok {
			s := float64(len(tok)) * 0.1
			if strings.Contains(tok, "_") {
				s += 3 // snake_case: almost certainly code
			}
			if tok != lower && tok != strings.ToUpper(tok) && !isTitleWord(tok) {
				s += 3 // mixed case (CamelCase / mixedCase)
			}
			c = &cand{tok: tok, score: s}
			seen[tok] = c
			order = append(order, tok)
		}
		c.score += 0.5 // frequency bump
	}
	sort.SliceStable(order, func(i, j int) bool { return seen[order[i]].score > seen[order[j]].score })
	if len(order) > max {
		order = order[:max]
	}
	return order
}

// isTitleWord reports a plain capitalized English word (e.g. "The", "Fix"),
// which is not evidence of code.
func isTitleWord(tok string) bool {
	if len(tok) < 2 {
		return false
	}
	rest := tok[1:]
	return tok[0] >= 'A' && tok[0] <= 'Z' && rest == strings.ToLower(rest)
}

// ComputeAnchors derives the ranked candidate-file set for a task by counting
// per-file matches of the task's identifiers (git grep; falls back to a bounded
// filesystem scan for non-git repos). When an ast-grep sidecar binary is
// available, definition sites for the identifiers add a structural rank boost.
// Returns at most maxAnchors entries, best first.
func ComputeAnchors(ctx context.Context, repo, task string, maxAnchors int) []Anchor {
	idents := TaskIdentifiers(task, 12)
	if len(idents) == 0 {
		return nil
	}
	scores := map[string]*Anchor{}
	for rank, id := range idents {
		weight := 1.0 + float64(len(idents)-rank)*0.15 // earlier = stronger token
		for path, hits := range grepCount(ctx, repo, id) {
			a, ok := scores[path]
			if !ok {
				a = &Anchor{Path: path}
				scores[path] = a
			}
			a.Hits += hits
			a.Score += weight * logish(hits)
			a.Via = append(a.Via, id)
			if strings.Contains(strings.ToLower(filepath.Base(path)), strings.ToLower(id)) {
				a.Score += 2 // filename mentions the identifier
			}
		}
	}
	// Structural boost: files where an identifier is *defined* (not just used)
	// are stronger anchors than usage sites.
	for _, path := range astGrepDefFiles(ctx, repo, idents) {
		if a, ok := scores[path]; ok {
			a.Score += 4
		}
	}
	out := make([]Anchor, 0, len(scores))
	for _, a := range scores {
		sort.Strings(a.Via)
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Path < out[j].Path
	})
	if len(out) > maxAnchors {
		out = out[:maxAnchors]
	}
	return out
}

// logish is a cheap diminishing-returns curve for hit counts.
func logish(n int) float64 {
	s := 0.0
	for v := n; v > 0; v /= 2 {
		s++
	}
	return s
}

// grepCount returns path -> match count for one identifier. Uses `git grep -c`
// (respects .gitignore, fast); non-git repos get a bounded walk.
func grepCount(ctx context.Context, repo, ident string) map[string]int {
	out := map[string]int{}
	c := exec.CommandContext(ctx, "git", "-C", repo, "grep", "-c", "-I", "-F", "-w", "--untracked", ident)
	b, err := c.Output()
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if i := strings.LastIndexByte(line, ':'); i > 0 {
				if n, err := strconv.Atoi(line[i+1:]); err == nil {
					out[line[:i]] = n
				}
			}
		}
		return out
	}
	if _, ok := err.(*exec.ExitError); ok {
		return out // git repo, zero matches (exit 1)
	}
	return walkCount(repo, ident)
}

// walkCount is the non-git fallback: scan at most maxScanFiles source files.
const maxScanFiles = 2000

func walkCount(repo, ident string) map[string]int {
	out := map[string]int{}
	n := 0
	_ = filepath.Walk(repo, func(path string, info os.FileInfo, err error) error {
		if err != nil || n >= maxScanFiles {
			return filepath.SkipDir
		}
		name := info.Name()
		if info.IsDir() {
			if strings.HasPrefix(name, ".") || name == "node_modules" || name == "__pycache__" || name == "venv" {
				return filepath.SkipDir
			}
			return nil
		}
		if info.Size() > 2<<20 {
			return nil
		}
		n++
		data, err := os.ReadFile(path)
		if err != nil || !isMostlyText(data) {
			return nil
		}
		if c := strings.Count(string(data), ident); c > 0 {
			rel, _ := filepath.Rel(repo, path)
			out[filepath.ToSlash(rel)] = c
		}
		return nil
	})
	return out
}

func isMostlyText(b []byte) bool {
	if len(b) > 4096 {
		b = b[:4096]
	}
	for _, c := range b {
		if c == 0 {
			return false
		}
	}
	return true
}

// astGrepBin locates the optional ast-grep sidecar: first a bundled binary next
// to the rig executable (the npm package ships platform binaries there), then
// PATH ("ast-grep", then its short alias "sg"). Empty when absent — every
// caller degrades gracefully, ast-grep is an accuracy boost, never a dep.
func astGrepBin() string {
	if self, err := os.Executable(); err == nil {
		for _, name := range []string{"ast-grep", "sg"} {
			cand := filepath.Join(filepath.Dir(self), name)
			if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
				return cand
			}
		}
	}
	for _, name := range []string{"ast-grep", "sg"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// astGrepDefFiles returns files where any of the identifiers appears as a
// function/class definition, via the ast-grep sidecar. Nil when the sidecar is
// absent or errors (structural boost simply not applied).
func astGrepDefFiles(ctx context.Context, repo string, idents []string) []string {
	bin := astGrepBin()
	if bin == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, id := range idents {
		for _, pat := range []string{"def " + id + "($$$PARAMS)", "class " + id} {
			c := exec.CommandContext(ctx, bin, "run", "--pattern", pat, "--json=compact", repo)
			b, err := c.Output()
			if err != nil {
				continue
			}
			for _, f := range astGrepFiles(b) {
				rel, err := filepath.Rel(repo, f)
				if err != nil {
					rel = f
				}
				rel = filepath.ToSlash(rel)
				if !seen[rel] {
					seen[rel] = true
					out = append(out, rel)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// astGrepFiles pulls the "file" fields out of ast-grep's JSON match array
// without committing to its full schema.
var astGrepFileRe = regexp.MustCompile(`"file"\s*:\s*"((?:[^"\\]|\\.)*)"`)

func astGrepFiles(jsonOut []byte) []string {
	var out []string
	for _, m := range astGrepFileRe.FindAllSubmatch(jsonOut, -1) {
		out = append(out, string(m[1]))
	}
	return out
}
