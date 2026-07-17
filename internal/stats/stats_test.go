package stats

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRecordLedgerAndLog verifies the two invariants P6 must hold: the ledger
// counters equal the summed per-request log, and the ledger reloads from disk.
func TestRecordLedgerAndLog(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRecorder(dir, false)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}

	r.Record(Record{Leg: LegWorker, Model: "haiku", InTokens: 100, OutTokens: 40, Status: 200})
	r.Record(Record{Leg: LegWorker, Model: "haiku", InTokens: 10, OutTokens: 5, Status: 200})
	r.Record(Record{Leg: LegMain, Model: "opus", InTokens: 200, OutTokens: 80, Status: 200})
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reload the ledger from disk into a fresh recorder.
	r2, err := NewRecorder(dir, false)
	if err != nil {
		t.Fatalf("reload NewRecorder: %v", err)
	}
	defer r2.Close()
	got := r2.led

	if got.WorkerIn != 110 || got.WorkerOut != 45 || got.NWorker != 2 {
		t.Errorf("worker ledger = %+v, want in=110 out=45 n=2", got)
	}
	if got.MainIn != 200 || got.MainOut != 80 || got.NMain != 1 {
		t.Errorf("main ledger = %+v, want in=200 out=80 n=1", got)
	}
	if got.Since == "" {
		t.Error("Since not initialized")
	}

	// The log must have exactly one line per Record, summing to the ledger.
	lines := readLog(t, dir)
	if len(lines) != 3 {
		t.Fatalf("log has %d lines, want 3", len(lines))
	}
	var wIn, wOut, mIn, mOut int
	for _, l := range lines {
		switch l.Leg {
		case string(LegWorker):
			wIn += l.InTok
			wOut += l.OutTok
		case string(LegMain):
			mIn += l.InTok
			mOut += l.OutTok
		}
	}
	if int64(wIn) != got.WorkerIn || int64(wOut) != got.WorkerOut ||
		int64(mIn) != got.MainIn || int64(mOut) != got.MainOut {
		t.Errorf("summed log (w %d/%d, m %d/%d) != ledger %+v", wIn, wOut, mIn, mOut, got)
	}
}

// TestLogBodiesOptIn verifies bodies are omitted by default and included when enabled.
func TestLogBodiesOptIn(t *testing.T) {
	dir := t.TempDir()

	off, _ := NewRecorder(dir, false)
	off.Record(Record{Leg: LegMain, ReqBody: []byte(`{"x":1}`), RespBody: []byte("secret")})
	off.Close()
	if l := readLog(t, dir)[0]; l.ReqBody != "" || l.RespBody != "" {
		t.Errorf("bodies logged with LOG_BODIES off: %+v", l)
	}

	dir2 := t.TempDir()
	on, _ := NewRecorder(dir2, true)
	on.Record(Record{Leg: LegMain, ReqBody: []byte(`{"x":1}`), RespBody: []byte("secret")})
	on.Close()
	if l := readLog(t, dir2)[0]; l.ReqBody != `{"x":1}` || l.RespBody != "secret" {
		t.Errorf("bodies not logged with LOG_BODIES on: %+v", l)
	}
}

// TestCompactLogSlidesWindow verifies the request log compacts to a sliding
// window: past the cap the oldest half is dropped at a line boundary, the
// newest entries survive, and appends keep working through the reopened handle.
func TestCompactLogSlidesWindow(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRecorder(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	const cap = int64(4096)
	r.SetMaxLogBytes(cap)

	for i := 0; i < 200; i++ {
		r.Record(Record{Leg: LegWorker, Model: "haiku", InTokens: i, Status: 200})
	}
	if err := r.CompactLog(); err != nil {
		t.Fatalf("compact: %v", err)
	}

	logPath := filepath.Join(dir, "logs", "requests.jsonl")
	fi, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() == 0 || fi.Size() > cap/2 {
		t.Errorf("compacted size = %d, want in (0, %d]", fi.Size(), cap/2)
	}

	lines := readLog(t, dir) // every surviving line must still parse
	if last := lines[len(lines)-1]; last.InTok != 199 {
		t.Errorf("newest entry lost: last InTok = %d, want 199", last.InTok)
	}

	// The append handle was reopened onto the new inode — writes must land.
	r.Record(Record{Leg: LegMain, Model: "opus", Status: 200})
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	lines = readLog(t, dir)
	if last := lines[len(lines)-1]; last.Model != "opus" {
		t.Errorf("append after compaction lost: last = %+v", last)
	}

	// Under the cap, compaction is a no-op.
	before, _ := os.Stat(logPath)
	r2, _ := NewRecorder(dir, false)
	r2.SetMaxLogBytes(cap)
	if err := r2.CompactLog(); err != nil {
		t.Fatalf("no-op compact: %v", err)
	}
	after, _ := os.Stat(logPath)
	if before.Size() != after.Size() {
		t.Errorf("no-op compaction changed size %d -> %d", before.Size(), after.Size())
	}
	r2.Close()
}

// TestNilRecorderIsSafe confirms every method no-ops on a nil recorder.
func TestNilRecorderIsSafe(t *testing.T) {
	var r *Recorder
	r.Record(Record{Leg: LegMain})
	r.StartFlusher(0)
	if err := r.Flush(); err != nil {
		t.Errorf("nil Flush: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("nil Close: %v", err)
	}
}

func readLog(t *testing.T, dir string) []logLine {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "logs", "requests.jsonl"))
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()
	var out []logLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l logLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("bad log line %q: %v", sc.Text(), err)
		}
		out = append(out, l)
	}
	return out
}

// TestWindowTokens: the L4 %-budget query sums worker-tier tokens (in+out) over
// the trailing window from the log tail — old entries, main-leg entries and
// garbage lines are all excluded — and the backward chunked read crosses chunk
// boundaries on a log larger than one chunk.
func TestWindowTokens(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}

	line := func(age time.Duration, routed string, in, out int) string {
		l := logLine{
			TS:     time.Now().Add(-age).UTC().Format(time.RFC3339),
			Leg:    "WORKER",
			Routed: routed,
			InTok:  in,
			OutTok: out,
		}
		b, _ := json.Marshal(l)
		return string(b) + "\n"
	}

	var sb []byte
	// Padding older than the window, enough to force multiple 64 KiB chunks.
	for len(sb) < 200<<10 {
		sb = append(sb, line(2*time.Hour, RoutedWorker, 999, 999)...)
	}
	sb = append(sb, line(30*time.Minute, RoutedWorker, 500, 500)...) // outside window
	sb = append(sb, "not json at all\n"...)                          // skipped
	sb = append(sb, line(5*time.Minute, RoutedWorker, 100, 40)...)
	sb = append(sb, line(4*time.Minute, RoutedMain, 1000, 400)...) // main leg: ignored
	sb = append(sb, line(3*time.Minute, RoutedDiverted, 60, 10)...)
	sb = append(sb, line(1*time.Minute, RoutedWorker, 10, 5)...)
	if err := os.WriteFile(filepath.Join(logDir, "requests.jsonl"), sb, 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := NewRecorder(dir, false)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	defer r.Close()

	worker, diverted := r.WindowTokens(15 * time.Minute)
	if worker != 155 || diverted != 70 {
		t.Errorf("WindowTokens = worker %d / diverted %d, want 155 / 70", worker, diverted)
	}

	var nilRec *Recorder
	if w, d := nilRec.WindowTokens(15 * time.Minute); w != 0 || d != 0 {
		t.Errorf("nil recorder WindowTokens = %d/%d, want 0/0", w, d)
	}
}
