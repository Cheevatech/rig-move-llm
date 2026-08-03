package worker

import (
	"sort"
	"strings"
)

// Conflict markers are what a `git stash pop` leaves in a file when it half
// applies. The proof retry tells the worker in plain words: if the pop
// conflicts, STOP, do not resolve it, name the file. Measured 2026-08-03 (the
// probe behind #54/#62), the worker did the opposite — it deleted the file in
// the way, re-popped, resolved by hand, and then reported "the proof-of-flip is
// complete". map9 D3 already paid for this lesson: prose can be ignored, so the
// engine reads the fact off the tree instead.
//
// A round whose own diff introduces markers is returning a broken tree. That is
// worse than returning nothing, because the summary above it will read like a
// success, so it is reported as loudly as the engine can report anything short
// of refusing the result — which it must not do: the diff still holds the
// round's work, and dropping it would destroy the thing the user needs to look
// at.
const (
	conflictStart = "<<<<<<< "
	conflictEnd   = ">>>>>>> "
)

// conflictMarkerFiles returns the files whose ADDED lines in diff carry git
// conflict markers, sorted. Only added lines count: a marker that was already in
// the tree is not this round's doing, and a diff that merely removes one is a
// round cleaning up.
func conflictMarkerFiles(diff string) []string {
	hit := map[string]bool{}
	var file string
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ b/"):
			file = strings.TrimPrefix(line, "+++ b/")
		case strings.HasPrefix(line, "+++ "):
			// /dev/null on a deletion, or a diff without the b/ prefix.
			file = strings.TrimPrefix(line, "+++ ")
		case strings.HasPrefix(line, "+"):
			body := line[1:]
			if strings.HasPrefix(body, conflictStart) || strings.HasPrefix(body, conflictEnd) {
				if file != "" && file != "/dev/null" {
					hit[file] = true
				}
			}
		}
	}
	out := make([]string, 0, len(hit))
	for f := range hit {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// noteConflictMarkers records, on the result, that this round's own diff carries
// conflict markers.
func noteConflictMarkers(res *Result) {
	files := conflictMarkerFiles(res.Diff)
	if len(files) == 0 {
		return
	}
	logStderr("cc: the round's diff carries conflict markers: %s", strings.Join(files, " "))
	res.ConflictMarkers = files
	res.Summary += "\n\nCONFLICT MARKERS IN THE DIFF: " + strings.Join(files, ", ") +
		" — this round wrote git conflict markers into the tree, so the working tree is broken however " +
		"the summary above reads. Do not accept this round: review the named file(s) and re-delegate. " +
		"(The retry instruction says to stop and name the file when a `git stash pop` conflicts; a round " +
		"that reaches here did not.)"
}
