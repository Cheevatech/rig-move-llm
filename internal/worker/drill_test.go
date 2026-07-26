package worker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/config"
)

// --- helpers --------------------------------------------------------------

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// drillRepo commits base and then writes after, leaving the difference in the
// working tree — the exact substrate the tiered return points at.
func drillRepo(t *testing.T, file, base, after string) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, file), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "base")
	if err := os.WriteFile(filepath.Join(dir, file), []byte(after), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// changedLines pulls every +/- line out of a unified diff (excluding the
// ---/+++ file headers), which is what a review must never lose sight of.
func changedLines(diff string) []string {
	var out []string
	for _, l := range strings.Split(diff, "\n") {
		if strings.HasPrefix(l, "+++ ") || strings.HasPrefix(l, "--- ") {
			continue
		}
		if strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-") {
			out = append(out, l)
		}
	}
	return out
}

const drillBase = `import math


def alpha(x):
    return x + 1


def beta(x):
    return x * 2


def omega(x):
    return x - 1
`

const drillAfter = `import math


def alpha(x):
    total = x + 1
    return total


def beta(x):
    return x * 2


def omega(x):
    scale = 3
    return (x - 1) * scale
`

// --- the safety-critical property: the body is raw git, whole -------------

// A drill that paraphrases, or that clips a line off a hunk, makes review blind
// (R1). This pins the body to git's own bytes: it must appear verbatim, as a
// contiguous run, inside `git diff` for that file.
func TestShowChangeBodyIsVerbatimGitBytes(t *testing.T) {
	repo := drillRepo(t, "app.py", drillBase, drillAfter)
	full := gitRun(t, repo, "diff", "-U3", "--", "app.py")

	res, err := ShowChange(context.Background(), repo, "app.py", 5, 6, "diff")
	if err != nil {
		t.Fatalf("ShowChange: %v", err)
	}
	if res.Body == "" {
		t.Fatal("empty body")
	}
	if !strings.Contains(full, res.Body) {
		t.Fatalf("body is not a verbatim run of git output\n--- body ---\n%s\n--- git ---\n%s", res.Body, full)
	}
	if !strings.Contains(res.Body, "+    total = x + 1") {
		t.Errorf("body lost the changed line:\n%s", res.Body)
	}
}

// The R13 probe in miniature: a defect planted in a hunk must reach MAIN's eyes
// through the drill, byte for byte, not as a description of it.
func TestShowChangePlantedDefectSurvivesDrill(t *testing.T) {
	defect := strings.Replace(drillAfter, "    scale = 3", "    scale = 1 / 0  # planted", 1)
	repo := drillRepo(t, "app.py", drillBase, defect)

	diff, _ := (&Engine{}).collectDiff(context.Background(), repo)
	ptrs := Pointers(repo, diff)
	var target *Pointer
	for i := range ptrs {
		if ptrs[i].Symbol == "omega" {
			target = &ptrs[i]
		}
	}
	if target == nil {
		t.Fatalf("no pointer for omega in %+v", ptrs)
	}

	res, err := ShowChange(context.Background(), repo, target.File, target.Line, target.Line, "diff")
	if err != nil {
		t.Fatalf("ShowChange: %v", err)
	}
	if !strings.Contains(res.Body, "+    scale = 1 / 0  # planted") {
		t.Fatalf("planted defect not visible in drilled body:\n%s", res.Body)
	}
}

// Scoped, not the whole diff — the point of the tier. Drilling one symbol must
// not drag the other's changes into MAIN.
func TestShowChangeIsScopedToTheRange(t *testing.T) {
	repo := drillRepo(t, "app.py", drillBase, drillAfter)
	full := gitRun(t, repo, "diff", "-U3", "--", "app.py")

	res, err := ShowChange(context.Background(), repo, "app.py", 5, 6, "diff")
	if err != nil {
		t.Fatalf("ShowChange: %v", err)
	}
	if strings.Contains(res.Body, "scale = 3") {
		t.Errorf("drill leaked the unrelated omega change:\n%s", res.Body)
	}
	if len(res.Body) >= len(full) {
		t.Errorf("drilled body (%d B) is not smaller than the full file diff (%d B)", len(res.Body), len(full))
	}
}

// Over-inclusion is deliberate and bounded: a hunk that intersects the range is
// emitted whole, because clipping it could hide the other half of an edit.
func TestShowChangeNeverClipsAnIntersectingHunk(t *testing.T) {
	repo := drillRepo(t, "app.py", drillBase, drillAfter)

	res, err := ShowChange(context.Background(), repo, "app.py", 5, 5, "diff")
	if err != nil {
		t.Fatalf("ShowChange: %v", err)
	}
	// Line 5 is the added `total = ...`; the `return total` edit belongs to the
	// same hunk and must travel with it.
	for _, want := range []string{"+    total = x + 1", "+    return total", "-    return x + 1"} {
		if !strings.Contains(res.Body, want) {
			t.Errorf("hunk was clipped, missing %q:\n%s", want, res.Body)
		}
	}
}

// The sharpest completeness check available: git's own @@ header declares how
// many old and new lines the hunk has, so a hunk body that does not tally with
// its header has lost (or gained) a line. This holds regardless of fixture.
func TestShowChangeHunkBodyTalliesWithItsHeader(t *testing.T) {
	repo := fixtureRepo(t)
	diff, _ := (&Engine{}).collectDiff(context.Background(), repo)

	for _, p := range Pointers(repo, diff) {
		res, err := ShowChange(context.Background(), repo, p.File, p.Line, p.Line, "diff")
		if err != nil {
			t.Fatalf("drill %s:%d: %v", p.File, p.Line, err)
		}
		assertHunksTally(t, p.File, res.Body)
	}
}

func assertHunksTally(t *testing.T, file, body string) {
	t.Helper()
	var oldWant, newWant, oldGot, newGot int
	var header string
	flush := func() {
		if header == "" {
			return
		}
		if oldGot != oldWant || newGot != newWant {
			t.Errorf("%s: hunk %q carries %d old / %d new lines, its header declares %d / %d",
				file, header, oldGot, newGot, oldWant, newWant)
		}
	}
	for _, l := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(l, "@@"):
			flush()
			header = l
			oldWant, newWant = hunkOldSpan(l), hunkNewCount(l)
			oldGot, newGot = 0, 0
		case header == "", strings.HasPrefix(l, "\\"):
			// file header, or "\ No newline at end of file" (counts as neither)
		case strings.HasPrefix(l, "-"):
			oldGot++
		case strings.HasPrefix(l, "+"):
			newGot++
		case strings.HasPrefix(l, " "):
			oldGot++
			newGot++
		}
	}
	flush()
}

// hunkOldSpan / hunkNewCount read the counts the production parser deliberately
// does not need, so the assertion is not checking the code against itself.
func hunkOldSpan(l string) int  { return spanCount(l, "-") }
func hunkNewCount(l string) int { return spanCount(l, "+") }

func spanCount(l, sign string) int {
	i := strings.Index(l, sign)
	if i < 0 {
		return 0
	}
	rest := l[i+1:]
	if j := strings.IndexAny(rest, " @"); j >= 0 {
		rest = rest[:j]
	}
	j := strings.IndexByte(rest, ',')
	if j < 0 {
		return 1 // an omitted count means exactly one line
	}
	n := 0
	for _, c := range rest[j+1:] {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// The round trip that R9 claims is lossless: every pointer in the manifest,
// drilled, must together reproduce every changed line of the real diff. This
// runs on testdata/pointers — an actual `git diff`, not a hand-written one.
func TestShowChangeRoundTripLosesNoChangedLine(t *testing.T) {
	repo := fixtureRepo(t)

	diff, _ := (&Engine{}).collectDiff(context.Background(), repo)
	ptrs := Pointers(repo, diff)
	if len(ptrs) == 0 {
		t.Fatal("fixture produced no pointers")
	}

	var drilled strings.Builder
	for _, p := range ptrs {
		res, err := ShowChange(context.Background(), repo, p.File, p.Line, p.Line, "diff")
		if err != nil {
			t.Fatalf("drill %s:%d: %v", p.File, p.Line, err)
		}
		drilled.WriteString(res.Body)
		drilled.WriteString("\n")
	}

	for _, line := range changedLines(diff) {
		if !strings.Contains(drilled.String(), line) {
			t.Errorf("changed line never reachable by drilling the manifest: %q", line)
		}
	}
}

// fixtureRepo rebuilds testdata/pointers as a real checkout: the stored tree is
// the post-change side, so reverse-applying the stored diff yields the base to
// commit. `git diff` then reproduces the fixture from live git.
func fixtureRepo(t *testing.T) string {
	t.Helper()
	src := filepath.Join("testdata", "pointers")
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")

	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	copyIn := func() {
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".py" {
				continue
			}
			b, err := os.ReadFile(filepath.Join(src, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	copyIn()
	diffPath, err := filepath.Abs(filepath.Join(src, "point.diff"))
	if err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "apply", "-R", diffPath)
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "base")
	copyIn()
	// Intent-to-add so a brand-new file shows up in `git diff`, which is the
	// source collectDiff (and therefore the manifest) reads.
	gitRun(t, dir, "add", "-N", ".")
	return dir
}

// The economic claim behind the tier: drilling a handful of entries costs a
// fraction of the diff it replaced. If three drills cost as much as the whole
// diff, the manifest bought nothing and B6 has nothing to measure.
func TestShowChangeThreeDrillsCostFarLessThanTheDiff(t *testing.T) {
	repo := fixtureRepo(t)
	diff, _ := (&Engine{}).collectDiff(context.Background(), repo)
	ptrs := Pointers(repo, diff)
	if len(ptrs) < 3 {
		t.Fatalf("fixture has %d pointers, need 3", len(ptrs))
	}

	total := 0
	for _, p := range ptrs[:3] {
		res, err := ShowChange(context.Background(), repo, p.File, p.Line, p.Line, "diff")
		if err != nil {
			t.Fatalf("drill %s:%d: %v", p.File, p.Line, err)
		}
		if res.Hunks == 0 {
			t.Errorf("pointer %s:%d drilled to nothing", p.File, p.Line)
		}
		total += len(res.Body)
	}
	t.Logf("3 drills = %d B, full diff = %d B (%.0f%%)", total, len(diff), 100*float64(total)/float64(len(diff)))
	if total >= len(diff) {
		t.Errorf("3 drills cost %d B against a %d B diff — the tier saved nothing", total, len(diff))
	}
}

// A deleted file has no post-change lines to intersect, and the manifest points
// at it with a bare line 1. The drill must still show what was removed.
func TestShowChangeOnADeletedFile(t *testing.T) {
	repo := drillRepo(t, "app.py", drillBase, drillAfter)
	if err := os.Remove(filepath.Join(repo, "app.py")); err != nil {
		t.Fatal(err)
	}

	res, err := ShowChange(context.Background(), repo, "app.py", 1, 1, "diff")
	if err != nil {
		t.Fatalf("ShowChange: %v", err)
	}
	if res.Hunks == 0 {
		t.Fatal("deleted file drilled to nothing")
	}
	for _, want := range []string{"-def alpha(x):", "-def omega(x):"} {
		if !strings.Contains(res.Body, want) {
			t.Errorf("deleted body is missing %q:\n%s", want, res.Body)
		}
	}
	assertHunksTally(t, "app.py", res.Body)
}

// --- verify-log drill (R9 §7) --------------------------------------------

func TestShowChangeServesTheParkedTestLog(t *testing.T) {
	repo := drillRepo(t, "app.py", drillBase, drillAfter)
	log := "collected 3 items\ntest_a PASSED\ntest_b FAILED\nE   assert 1 == 2\n1 failed, 2 passed\n"
	abs := filepath.Join(repo, ".rig-move-llm", "last_test.log")
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := ShowChange(context.Background(), repo, ".rig-move-llm/last_test.log", 1, 6, "test_log")
	if err != nil {
		t.Fatalf("ShowChange: %v", err)
	}
	if res.Body != log {
		t.Fatalf("log body is not byte-identical\n got %q\nwant %q", res.Body, log)
	}
}

func TestShowChangeSlicesTheTestLog(t *testing.T) {
	repo := drillRepo(t, "app.py", drillBase, drillAfter)
	log := "one\ntwo\nthree\nfour\n"
	abs := filepath.Join(repo, ".rig-move-llm", "last_test.log")
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := ShowChange(context.Background(), repo, ".rig-move-llm/last_test.log", 2, 3, "test_log")
	if err != nil {
		t.Fatalf("ShowChange: %v", err)
	}
	if res.Body != "two\nthree\n" {
		t.Fatalf("body = %q, want %q", res.Body, "two\nthree\n")
	}
}

// --- guards ---------------------------------------------------------------

func TestShowChangeRefusesPathEscape(t *testing.T) {
	repo := drillRepo(t, "app.py", drillBase, drillAfter)
	for _, p := range []string{"../outside.py", "/etc/passwd"} {
		if _, err := ShowChange(context.Background(), repo, p, 1, 5, "test_log"); err == nil {
			t.Errorf("drill escaped the repo with %q", p)
		}
	}
}

func TestShowChangeRefusesGatePath(t *testing.T) {
	repo := drillRepo(t, "app.py", drillBase, drillAfter)
	abs := filepath.Join(repo, ".gate", "spec.md")
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("frozen\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ShowChange(context.Background(), repo, ".gate/spec.md", 1, 1, "test_log"); err == nil {
		t.Error("drill served a frozen gate path")
	}
}

func TestShowChangeReportsAnEmptyRange(t *testing.T) {
	repo := drillRepo(t, "app.py", drillBase, drillAfter)
	res, err := ShowChange(context.Background(), repo, "app.py", 900, 910, "diff")
	if err != nil {
		t.Fatalf("ShowChange: %v", err)
	}
	if res.Body != "" || res.Hunks != 0 {
		t.Fatalf("expected no hunks for an out-of-range drill, got %d hunk(s): %q", res.Hunks, res.Body)
	}
}

// B3 rolls the manifest up to file granularity when the diff is wide, and a
// file-granularity range can cover the whole file. Over the cap the drill must
// refuse — never hand back a body that has been quietly cut down to fit.
func TestShowChangeRefusesRatherThanTruncating(t *testing.T) {
	repo := drillRepo(t, "app.py", drillBase, drillAfter)
	t.Setenv("RIG_DRILL_MAX_BYTES", "40")

	_, err := ShowChange(context.Background(), repo, "app.py", 1, 999, "diff")
	if err == nil {
		t.Fatal("an over-cap drill was served instead of refused")
	}
	// The refusal has to be actionable: it names the ranges worth asking for.
	if !strings.Contains(err.Error(), "drill one of these narrower ranges") {
		t.Errorf("refusal is not actionable: %v", err)
	}
	for _, span := range []string{"2-9", "11-15"} {
		if !strings.Contains(err.Error(), span) {
			t.Errorf("refusal omits hunk span %s: %v", span, err)
		}
	}
}

func TestShowChangeCapDoesNotMeterARefusal(t *testing.T) {
	repo := drillRepo(t, "app.py", drillBase, drillAfter)
	t.Setenv("RIG_DRILL_MAX_BYTES", "40")
	beforeCalls, beforeBytes := DrilledSoFar()

	if _, err := ShowChange(context.Background(), repo, "app.py", 1, 999, "diff"); err == nil {
		t.Fatal("expected a refusal")
	}
	calls, bytes := DrilledSoFar()
	if calls != beforeCalls || bytes != beforeBytes {
		t.Errorf("a refused drill was metered: %d calls / %d B entered the count", calls-beforeCalls, bytes-beforeBytes)
	}
}

func TestShowChangeRejectsUnknownKind(t *testing.T) {
	repo := drillRepo(t, "app.py", drillBase, drillAfter)
	if _, err := ShowChange(context.Background(), repo, "app.py", 1, 5, "summary"); err == nil {
		t.Error("unknown kind accepted")
	}
}

// --- the B6 meter ---------------------------------------------------------

// B6 has to weigh drilled bytes against the bytes the tier saved, and against a
// direct Read (which the PreTool hook never denies). Drills therefore self-report.
func TestShowChangeMetersDrilledBytes(t *testing.T) {
	repo := drillRepo(t, "app.py", drillBase, drillAfter)
	beforeCalls, beforeBytes := DrilledSoFar()

	res, err := ShowChange(context.Background(), repo, "app.py", 5, 6, "diff")
	if err != nil {
		t.Fatalf("ShowChange: %v", err)
	}
	calls, bytes := DrilledSoFar()
	if calls != beforeCalls+1 {
		t.Errorf("calls = %d, want %d", calls, beforeCalls+1)
	}
	if bytes-beforeBytes != int64(len(res.Body)) {
		t.Errorf("metered %d B, body is %d B", bytes-beforeBytes, len(res.Body))
	}
}

// --- MCP surface ----------------------------------------------------------

func TestToolListAdvertisesShowChange(t *testing.T) {
	var tool map[string]any
	for _, tl := range toolList() {
		if tl["name"] == "show_change" {
			tool = tl
		}
	}
	if tool == nil {
		t.Fatal("show_change missing from tools/list")
	}
	schema, _ := tool["inputSchema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	for _, arg := range []string{"file", "start_line", "end_line", "kind"} {
		if _, ok := props[arg]; !ok {
			t.Errorf("inputSchema is missing %q — the manifest points back with it", arg)
		}
	}
}

func TestOnToolsCallRoutesShowChange(t *testing.T) {
	repo := drillRepo(t, "app.py", drillBase, drillAfter)
	s := &Server{engine: NewEngine(config.Config{})}

	args, _ := json.Marshal(map[string]any{
		"name": "show_change",
		"arguments": map[string]any{
			"file": "app.py", "start_line": 5, "end_line": 6, "kind": "diff", "repo": repo,
		},
	})
	res, rerr := s.onToolsCall(args)
	if rerr != nil {
		t.Fatalf("rpc error: %+v", rerr)
	}
	if res["isError"] == true {
		t.Fatalf("tool reported an error: %+v", res)
	}
	text := drillResultText(t, res)
	if !strings.Contains(text, "+    total = x + 1") {
		t.Fatalf("drilled body did not reach the MCP result:\n%s", text)
	}
}

func drillResultText(t *testing.T, res map[string]any) string {
	t.Helper()
	content, ok := res["content"].([]map[string]any)
	if !ok || len(content) == 0 {
		t.Fatalf("bad tool result: %+v", res)
	}
	s, _ := content[0]["text"].(string)
	return s
}
