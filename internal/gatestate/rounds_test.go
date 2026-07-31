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

// The refund gives back a slot for a round the engine flagged unproductive —
// and its cap is the termination argument for the whole rule, so both sides
// are pinned here.
func TestRefundRound(t *testing.T) {
	dir := t.TempDir()

	// Nothing spent yet: nothing to refund.
	if _, ok := RefundRound(dir); ok {
		t.Fatal("refund with no counter must be refused")
	}

	BumpRound(dir)
	BumpRound(dir)
	BumpRound(dir)

	r, ok := RefundRound(dir)
	if !ok || r.Refunded != 1 || r.Effective() != 2 {
		t.Fatalf("first refund: got %+v ok=%v, want refunded=1 effective=2", r, ok)
	}
	r, ok = RefundRound(dir)
	if !ok || r.Refunded != 2 || r.Effective() != 1 {
		t.Fatalf("second refund: got %+v ok=%v, want refunded=2 effective=1", r, ok)
	}

	// The cap: a third refund in the same turn is refused, whatever happened.
	r, ok = RefundRound(dir)
	if ok || r.Refunded != MaxUnproductiveRefunds {
		t.Fatalf("refund past the cap must be refused: got %+v ok=%v", r, ok)
	}

	// The refund state persists and re-reads.
	got, fresh := ReadRounds(dir)
	if !fresh || got.Count != 3 || got.Refunded != 2 || got.Effective() != 1 {
		t.Fatalf("ReadRounds after refunds = %+v (fresh=%v)", got, fresh)
	}
}

// ClearTurn is the human's escape hatch and must reset the refunds WITH the
// count — a leftover Refunded on a fresh turn would hand out free rounds.
func TestClearTurnResetsRefunds(t *testing.T) {
	dir := t.TempDir()
	BumpRound(dir)
	if _, ok := RefundRound(dir); !ok {
		t.Fatal("setup refund failed")
	}

	ClearTurn(dir)

	if got := BumpRound(dir); got != 1 {
		t.Fatalf("fresh turn must start at 1, got %d", got)
	}
	r, _ := ReadRounds(dir)
	if r.Refunded != 0 {
		t.Fatalf("fresh turn must carry no refunds, got %+v", r)
	}
}

// Effective never goes negative, even if a corrupt state file over-refunds.
func TestEffectiveClampsAtZero(t *testing.T) {
	if got := (Rounds{Count: 1, Refunded: 5}).Effective(); got != 0 {
		t.Fatalf("Effective = %d, want 0", got)
	}
}
