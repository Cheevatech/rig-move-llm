package thin

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Watch follows a run's action log. With no dir it picks the most recent run.
//
// The design target is one question, asked from a second window while a run is
// in its twentieth minute: is it working, or is it stuck? So the loop prints new
// action lines as they land AND, when nothing lands, says so on a timer. Silence
// that is never mentioned looks identical to a dead terminal, which is the state
// this command exists to distinguish.
func Watch(w io.Writer, dir string, follow bool) error {
	if dir == "" {
		latest, err := LatestRun()
		if err != nil {
			return err
		}
		dir = latest
	}
	path := filepath.Join(dir, ActionsFile)

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("no action log at %s: %w", path, err)
	}
	defer f.Close()

	fmt.Fprintf(w, "watching %s\n\n", dir)

	// Everything already written, then the tail.
	_, done, err := drain(w, f)
	if err != nil {
		return err
	}
	if done || !follow {
		return nil
	}

	const (
		poll         = 300 * time.Millisecond
		quietNotice  = 30 * time.Second // how long silence must last before we say so
		quietRepeats = 30 * time.Second // and how often we repeat it
	)
	lastData := time.Now()
	lastNotice := time.Now()
	for {
		time.Sleep(poll)
		moved, done, err := drain(w, f)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if moved {
			lastData, lastNotice = time.Now(), time.Now()
			continue
		}
		if n := time.Now(); n.Sub(lastData) > quietNotice && n.Sub(lastNotice) > quietRepeats {
			// A run inside a long `Bash` (a slow test suite) is legitimately quiet
			// for minutes, so this is a fact, not a warning — and naming the stall
			// ceiling next to it says how long the quiet may still legitimately last.
			fmt.Fprintf(w, "         … no new action for %s (stall guard fires at %s)\n",
				n.Sub(lastData).Round(time.Second), stallCeiling())
			lastNotice = n
		}
	}
}

// drain copies whatever is new, reporting whether anything moved and whether the
// run's closing line has appeared.
func drain(w io.Writer, f *os.File) (moved, finished bool, err error) {
	buf := make([]byte, 32*1024)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			moved = true
			chunk := string(buf[:n])
			if _, werr := io.WriteString(w, chunk); werr != nil {
				return moved, false, werr
			}
			if strings.Contains(chunk, "──") {
				finished = true
			}
		}
		if rerr == io.EOF {
			return moved, finished, nil
		}
		if rerr != nil {
			return moved, finished, rerr
		}
	}
}

// LatestRun returns the newest run directory under the log root.
func LatestRun() (string, error) {
	root, err := runRoot()
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("no runs yet under %s", root)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) == 0 {
		return "", fmt.Errorf("no runs yet under %s", root)
	}
	// Run directories are named with a sortable timestamp prefix, so lexical order
	// is chronological order — no stat calls, and no ambiguity when two runs start
	// in the same second (the random suffix breaks the tie deterministically).
	sort.Strings(dirs)
	return filepath.Join(root, dirs[len(dirs)-1]), nil
}

// ListRuns returns the most recent runs, newest first, for `rig watch --list`.
func ListRuns(limit int) ([]string, error) {
	root, err := runRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("no runs yet under %s", root)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	if limit > 0 && len(dirs) > limit {
		dirs = dirs[:limit]
	}
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, filepath.Join(root, d))
	}
	return out, nil
}

// RunSummary is the one-line description of a run, for the list.
func RunSummary(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, ActionsFile))
	if err != nil {
		return filepath.Base(dir) + "  (no action log)"
	}
	var task, status string
	lines := strings.Split(string(data), "\n")
	for _, l := range lines {
		if strings.HasPrefix(l, "task:") {
			task = strings.TrimSpace(strings.TrimPrefix(l, "task:"))
		}
		if i := strings.Index(l, "──"); i >= 0 {
			status = strings.TrimSpace(l[i+len("──"):])
		}
	}
	if status == "" {
		status = "running"
	}
	return fmt.Sprintf("%s  [%s]  %s", filepath.Base(dir), status, truncateMiddle(task, 60))
}
