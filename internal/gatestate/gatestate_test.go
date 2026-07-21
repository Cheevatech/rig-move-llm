package gatestate

import (
	"testing"
	"time"
)

func TestRoundTripAndTTL(t *testing.T) {
	dir := t.TempDir()

	if _, fresh := ReadExplore(dir); fresh {
		t.Fatal("missing explore must not be fresh")
	}
	_ = WriteExplore(dir, Explore{Repo: "/r", NSites: 2, At: time.Now()})
	if ev, fresh := ReadExplore(dir); !fresh || ev.NSites != 2 {
		t.Fatalf("explore round-trip failed: %+v fresh=%v", ev, fresh)
	}
	_ = WriteExplore(dir, Explore{Repo: "/r", At: time.Now().Add(-ExploreTTL - time.Minute)})
	if _, fresh := ReadExplore(dir); fresh {
		t.Fatal("expired explore must not be fresh")
	}

	_ = WriteRepair(dir, Repair{EditsLeft: 0, OpenedAt: time.Now()})
	if _, open := ReadRepair(dir); open {
		t.Fatal("zero budget must read as closed")
	}
	_ = WriteRepair(dir, Repair{EditsLeft: 1, OpenedAt: time.Now().Add(-RepairTTL - time.Minute)})
	if _, open := ReadRepair(dir); open {
		t.Fatal("expired window must read as closed")
	}
}

func TestClearTurnKeepsExplore(t *testing.T) {
	dir := t.TempDir()
	_ = WriteExplore(dir, Explore{Repo: "/r", At: time.Now()})
	_ = WriteTriage(dir, Triage{Decision: "solo", At: time.Now()})
	_ = WriteRepair(dir, Repair{EditsLeft: 3, OpenedAt: time.Now()})
	ClearTurn(dir)
	if _, fresh := ReadTriage(dir); fresh {
		t.Fatal("triage must be cleared on a new turn")
	}
	if _, open := ReadRepair(dir); open {
		t.Fatal("repair window must be cleared on a new turn")
	}
	if _, fresh := ReadExplore(dir); !fresh {
		t.Fatal("explore evidence must survive a new turn")
	}
}
