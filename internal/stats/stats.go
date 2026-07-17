// Package stats is rig-move-llm's observability recorder: the writing side of the
// `stats` command surface in internal/cli. It owns two artifacts under the active
// scope's data dir:
//
//   - logs/requests.jsonl — one JSON object per served request (metadata by
//     default; full bodies when LOG_BODIES is enabled).
//   - stats.json          — a cumulative token ledger (billed MAIN leg vs
//     offloaded WORKER leg) that survives restarts.
//
// A single daemon process owns the Recorder, so a sync.Mutex is sufficient — no
// file lock. The in-memory ledger is the source of truth; it is loaded on boot,
// flushed to stats.json on an interval and again on Close (wired to SIGTERM by
// the serve command). The package depends only on the standard library.
package stats

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Leg identifies which routing path served a request.
type Leg string

const (
	LegMain   Leg = "MAIN"   // billed passthrough to the paid Anthropic upstream
	LegWorker Leg = "WORKER" // offloaded to the local/self-hosted worker
)

// Routed values distinguish why a request landed on its leg. A "diverted"
// request is a worker-tier call that fell back to the paid Anthropic upstream
// (L2 passthrough entry) — billed like MAIN but not a genuine main-agent call,
// so honest savings math must not lump it in with either pure bucket.
const (
	RoutedMain     = "main"
	RoutedWorker   = "worker"
	RoutedDiverted = "diverted"
)

// ledger mirrors the stats.json shape read by internal/cli.stats. The JSON field
// tags MUST stay in sync with that struct.
type ledger struct {
	Since     string `json:"since"`
	MainIn    int64  `json:"main_in"`
	MainOut   int64  `json:"main_out"`
	WorkerIn  int64  `json:"worker_in"`
	WorkerOut int64  `json:"worker_out"`
	NMain     int64  `json:"n_main"`
	NWorker   int64  `json:"n_worker"`
}

// Record is one request's accounting: appended to the JSONL log and folded into
// the ledger.
type Record struct {
	Leg       Leg
	Project   string // canonical project dir for /p/<id>-prefixed requests ("" otherwise)
	Endpoint  string // worker endpoint label serving the request ("" for plain main)
	Routed    string // RoutedMain | RoutedWorker | RoutedDiverted ("" tolerated as main)
	Model     string
	InTokens  int
	OutTokens int
	Millis    int64
	Status    int
	// Bodies are written to the log only when LOG_BODIES is enabled.
	ReqBody  []byte
	RespBody []byte
}

// logLine is the JSON object written per request to requests.jsonl.
type logLine struct {
	TS       string `json:"ts"`
	Leg      string `json:"leg"`
	Project  string `json:"project,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Routed   string `json:"routed,omitempty"`
	Model    string `json:"model"`
	InTok    int    `json:"in_tok"`
	OutTok   int    `json:"out_tok"`
	MS       int64  `json:"ms"`
	Status   int    `json:"status"`
	ReqBody  string `json:"req_body,omitempty"`
	RespBody string `json:"resp_body,omitempty"`
}

// Recorder is the single-process, mutex-guarded owner of the ledger and log.
// All methods are safe on a nil *Recorder (they no-op), so callers can hold an
// optional recorder without guarding every call.
type Recorder struct {
	mu          sync.Mutex
	led         ledger
	statsPath   string
	logPath     string
	logFile     *os.File
	logBodies   bool
	maxLogBytes int64
	dirty       bool

	stop chan struct{}
	done chan struct{}
}

// NewRecorder opens (creating as needed) the log file and loads the ledger from
// dataDir. It returns an error only if the data dir or log file cannot be
// created; the caller may then run without recording.
func NewRecorder(dataDir string, logBodies bool) (*Recorder, error) {
	logDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, err
	}
	logPath := filepath.Join(logDir, "requests.jsonl")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	r := &Recorder{
		statsPath:   filepath.Join(dataDir, "stats.json"),
		logPath:     logPath,
		logFile:     lf,
		logBodies:   logBodies,
		maxLogBytes: 50 << 20,
	}
	r.loadLedger()
	if r.led.Since == "" {
		r.led.Since = time.Now().UTC().Format(time.RFC3339)
		r.dirty = true
	}
	return r, nil
}

// loadLedger reads stats.json into the in-memory ledger, tolerating a missing or
// corrupt file (starts fresh).
func (r *Recorder) loadLedger() {
	data, err := os.ReadFile(r.statsPath)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &r.led)
}

// Record appends one line to the JSONL log and folds the counts into the ledger.
func (r *Recorder) Record(rec Record) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	line := logLine{
		TS:       time.Now().UTC().Format(time.RFC3339),
		Leg:      string(rec.Leg),
		Project:  rec.Project,
		Endpoint: rec.Endpoint,
		Routed:   rec.Routed,
		Model:    rec.Model,
		InTok:    rec.InTokens,
		OutTok:   rec.OutTokens,
		MS:       rec.Millis,
		Status:   rec.Status,
	}
	if r.logBodies {
		line.ReqBody = string(rec.ReqBody)
		line.RespBody = string(rec.RespBody)
	}
	if b, err := json.Marshal(line); err == nil && r.logFile != nil {
		_, _ = r.logFile.Write(append(b, '\n'))
	}

	switch rec.Leg {
	case LegMain:
		r.led.MainIn += int64(rec.InTokens)
		r.led.MainOut += int64(rec.OutTokens)
		r.led.NMain++
	case LegWorker:
		r.led.WorkerIn += int64(rec.InTokens)
		r.led.WorkerOut += int64(rec.OutTokens)
		r.led.NWorker++
	}
	r.dirty = true
}

// SetMaxLogBytes overrides the requests.jsonl size cap (from LOG_MAX_MB).
// Values <= 0 keep the default.
func (r *Recorder) SetMaxLogBytes(n int64) {
	if r == nil || n <= 0 {
		return
	}
	r.mu.Lock()
	r.maxLogBytes = n
	r.mu.Unlock()
}

// StartFlusher spawns a goroutine that flushes the ledger on the given interval
// until Close. Each cycle also compacts the request log when it exceeds its
// cap. It is idempotent-unsafe: call it at most once per Recorder.
func (r *Recorder) StartFlusher(interval time.Duration) {
	if r == nil {
		return
	}
	r.stop = make(chan struct{})
	r.done = make(chan struct{})
	go func() {
		defer close(r.done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				_ = r.Flush()
				_ = r.CompactLog()
			case <-r.stop:
				return
			}
		}
	}()
}

// CompactLog trims requests.jsonl back to roughly half its cap when it has
// grown past the cap, keeping the newest entries: read the tail, cut at the
// first line boundary, write to a tmp file and rename over (atomic, like the
// ledger flush). The file therefore slides between cap/2 and cap — it never
// resets to zero the way logrotate does. The append handle is reopened after
// the rename because the rename swaps the inode.
func (r *Recorder) CompactLog() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.logFile == nil {
		return nil
	}
	fi, err := r.logFile.Stat()
	if err != nil || fi.Size() <= r.maxLogBytes {
		return err
	}

	keep := r.maxLogBytes / 2
	src, err := os.Open(r.logPath)
	if err != nil {
		return err
	}
	if _, err := src.Seek(fi.Size()-keep, 0); err != nil {
		src.Close()
		return err
	}
	tail, err := io.ReadAll(src)
	src.Close()
	if err != nil {
		return err
	}
	// Drop the partial first line so the file always starts on a record boundary.
	if i := bytes.IndexByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	}

	tmp := r.logPath + ".tmp"
	if err := os.WriteFile(tmp, tail, 0o644); err != nil {
		return err
	}
	_ = r.logFile.Close()
	r.logFile = nil
	if err := os.Rename(tmp, r.logPath); err != nil {
		return err
	}
	lf, err := os.OpenFile(r.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	r.logFile = lf
	return nil
}

// WindowTokens sums worker-tier tokens (input+output) from requests.jsonl over
// the trailing window, split by routing: served by the custom worker vs diverted
// to the paid upstream. It reads the log tail backwards in chunks — no index —
// so cost tracks the window's traffic, not the file size (the log is append-only
// chronological, so once a line predates the cutoff everything before it does
// too). Used by the L4 %-budget alternation, where
// custom_share = worker / (worker + diverted).
func (r *Recorder) WindowTokens(window time.Duration) (workerTok, divertedTok int64) {
	if r == nil {
		return 0, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	f, err := os.Open(r.logPath)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, 0
	}

	cutoff := time.Now().Add(-window)
	const chunk = 64 << 10
	var buf []byte
	off := fi.Size()
	for off > 0 {
		n := int64(chunk)
		if off < n {
			n = off
		}
		off -= n
		head := make([]byte, n)
		if _, err := f.ReadAt(head, off); err != nil {
			return 0, 0
		}
		buf = append(head, buf...)
		if off == 0 {
			break
		}
		// The buffer may begin mid-line; its first complete line starts after
		// the first newline. If that line already predates the cutoff, stop.
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			continue
		}
		if ts := lineTS(buf[i+1:]); !ts.IsZero() && ts.Before(cutoff) {
			break
		}
	}
	if off > 0 {
		// Drop the leading partial line so parsing starts on a record boundary.
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		} else {
			buf = nil
		}
	}

	for _, line := range bytes.Split(buf, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var l logLine
		if json.Unmarshal(line, &l) != nil {
			continue
		}
		ts, err := time.Parse(time.RFC3339, l.TS)
		if err != nil || ts.Before(cutoff) {
			continue
		}
		switch l.Routed {
		case RoutedWorker:
			workerTok += int64(l.InTok + l.OutTok)
		case RoutedDiverted:
			divertedTok += int64(l.InTok + l.OutTok)
		}
	}
	return workerTok, divertedTok
}

// lineTS parses the timestamp of the first line of b (which must start on a
// record boundary), returning the zero time when it cannot.
func lineTS(b []byte) time.Time {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		b = b[:i]
	}
	var l struct {
		TS string `json:"ts"`
	}
	if json.Unmarshal(b, &l) != nil {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, l.TS)
	return t
}

// Flush writes the ledger to stats.json atomically (tmp file + rename) when it
// has changed since the last flush.
func (r *Recorder) Flush() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.flushLocked()
}

func (r *Recorder) flushLocked() error {
	if !r.dirty {
		return nil
	}
	b, err := json.MarshalIndent(r.led, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.statsPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, r.statsPath); err != nil {
		return err
	}
	r.dirty = false
	return nil
}

// Close stops the flusher, performs a final flush, and closes the log file.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	if r.stop != nil {
		close(r.stop)
		<-r.done
		r.stop = nil
	}
	err := r.Flush()
	r.mu.Lock()
	if r.logFile != nil {
		_ = r.logFile.Close()
		r.logFile = nil
	}
	r.mu.Unlock()
	return err
}
