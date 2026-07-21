package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// exploreProgress is the resumable state of a multi-call Stage-0 exploration,
// persisted at <stateDir>/explore_progress.json. It lets a later explore call
// pick up where the previous one paused: which files are already read, which
// candidate sites are already verified, and how many rounds have run. Keyed on
// task+repo — a different task discards it.
type exploreProgress struct {
	Task          string     `json:"task"`
	Repo          string     `json:"repo"`
	ReadFiles     []string   `json:"read_files"`
	Sites         []EditSite `json:"sites"`
	RelevantFiles []string   `json:"relevant_files"`
	Entrypoints   []string   `json:"entrypoints"`
	OpenQuestions []string   `json:"open_questions"`
	Round         int        `json:"round"`
	At            time.Time  `json:"at"`
}

// progressTTL bounds how long a paused exploration can be resumed before it is
// treated as stale (a new intake).
const progressTTL = 1 * time.Hour

func (e *Engine) progressPath() string { return filepath.Join(e.stateDir(), "explore_progress.json") }

// loadProgress returns the saved progress for this task+repo and whether we are
// resuming (same task, same repo, still fresh). A mismatch or stale file yields
// a zero progress and resuming=false.
func (e *Engine) loadProgress(repo, task string) (exploreProgress, bool) {
	var p exploreProgress
	b, err := os.ReadFile(e.progressPath())
	if err != nil || json.Unmarshal(b, &p) != nil {
		return exploreProgress{Task: task, Repo: repo}, false
	}
	if p.Task != task || p.Repo != repo || time.Since(p.At) > progressTTL {
		return exploreProgress{Task: task, Repo: repo}, false
	}
	return p, true
}

func (e *Engine) saveProgress(p exploreProgress) {
	p.At = time.Now()
	b, err := json.Marshal(p)
	if err != nil {
		return
	}
	_ = os.WriteFile(e.progressPath(), b, 0o644)
}

func (e *Engine) clearProgress() { _ = os.Remove(e.progressPath()) }

// mergeProgress folds one call's reads + report into the accumulated progress,
// de-duplicating files and candidate sites (by path:line).
func mergeProgress(prog exploreProgress, task, repo string, read map[string]bool, report *exploreReport) exploreProgress {
	prog.Task, prog.Repo = task, repo

	files := map[string]bool{}
	for _, f := range prog.ReadFiles {
		files[f] = true
	}
	for f := range read {
		files[f] = true
	}
	prog.ReadFiles = sortedKeys(files)

	if report == nil {
		return prog
	}

	siteKey := func(s EditSite) string { return s.Path + ":" + itoa(s.Line) }
	seen := map[string]bool{}
	var sites []EditSite
	for _, s := range append(prog.Sites, report.CandidateEditSites...) {
		k := siteKey(s)
		if !seen[k] {
			seen[k] = true
			sites = append(sites, s)
		}
	}
	prog.Sites = sites
	prog.RelevantFiles = union(prog.RelevantFiles, report.RelevantFiles)
	prog.Entrypoints = union(prog.Entrypoints, report.EntrypointsOrRepro)
	prog.OpenQuestions = union(prog.OpenQuestions, report.OpenQuestions)
	return prog
}

// progressReport assembles the accumulated progress into the report shape
// returned to MAIN, so a paused call still hands back everything found so far.
func progressReport(prog exploreProgress) exploreReport {
	return exploreReport{
		RelevantFiles:      prog.RelevantFiles,
		CandidateEditSites: prog.Sites,
		EntrypointsOrRepro: prog.Entrypoints,
		OpenQuestions:      prog.OpenQuestions,
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string{}, a...), b...) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
