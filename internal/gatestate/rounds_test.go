package gatestate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBumpRoundCounts(t *testing.T) {
	dir := t.TempDir()
	for want := 1; want <= 3; want++ {
		if got := BumpRound(dir); got != want {
			t.Fatalf("BumpRound = %d, want %d", got, want)
		}
	}
	r, ok := ReadRounds(dir)
	if !ok || r.Count != 3 {
		t.Fatalf("ReadRounds = %+v (ok=%v), want count 3", r, ok)
	}
}

// A counter left behind by a session that never reached its next UserPromptSubmit
// must not lock MAIN out of delegating for good.
func TestRoundsExpire(t *testing.T) {
	dir := t.TempDir()
	stale, err := json.Marshal(Rounds{Count: 9, At: time.Now().Add(-RoundsTTL - time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, roundsFile), stale, 0o644); err != nil {
		t.Fatal(err)
	}

	if r, ok := ReadRounds(dir); ok || r.Count != 0 {
		t.Fatalf("a stale counter must read as absent, got %+v (ok=%v)", r, ok)
	}
	if got := BumpRound(dir); got != 1 {
		t.Fatalf("a stale counter must restart at 1, got %d", got)
	}
}

// The human's next message is the budget's escape hatch, and ClearTurn is what
// runs on it.
func TestClearTurnResetsRounds(t *testing.T) {
	dir := t.TempDir()
	BumpRound(dir)
	BumpRound(dir)

	ClearTurn(dir)

	if r, ok := ReadRounds(dir); ok || r.Count != 0 {
		t.Fatalf("ClearTurn must drop the counter, got %+v (ok=%v)", r, ok)
	}
}
