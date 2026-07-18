package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/internal/stats"
)

// TestBudgetAlternation drives the L4 %-budget through a full cycle at 50%:
// an empty window routes to the worker; once the worker's token share is at or
// above target the next request is diverted to the paid upstream (logged as
// routed=diverted, endpoint=budget); once the diverted tokens outweigh the
// worker's the route flips back to the worker.
func TestBudgetAlternation(t *testing.T) {
	var workerHits, mainHits atomic.Int64
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workerHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, openAIOK) // usage 300/20 -> 320 worker tokens
	}))
	defer worker.Close()
	mainUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mainHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"m","type":"message","usage":{"input_tokens":600,"output_tokens":100}}`)
	}))
	defer mainUp.Close()

	s, dataDir := newFallbackServer(t, config.Config{
		MainUpstreamURL:     mainUp.URL,
		WorkerAPIBase:       worker.URL + "/v1",
		CustomSubagentUsage: 50,
	})
	h := s.Handler()

	// 1: empty window -> worker (fail-cheap).
	if rw := postWorker(t, h); rw.Code != http.StatusOK {
		t.Fatalf("first request: status %d: %s", rw.Code, rw.Body.String())
	}
	if workerHits.Load() != 1 || mainHits.Load() != 0 {
		t.Fatalf("after 1st: worker=%d main=%d, want 1/0", workerHits.Load(), mainHits.Load())
	}

	// 2: share 100% >= 50% -> diverted to main.
	if rw := postWorker(t, h); rw.Code != http.StatusOK {
		t.Fatalf("second request: status %d: %s", rw.Code, rw.Body.String())
	}
	if workerHits.Load() != 1 || mainHits.Load() != 1 {
		t.Fatalf("after 2nd: worker=%d main=%d, want 1/1 (divert)", workerHits.Load(), mainHits.Load())
	}

	// 3: worker 320 vs diverted 700 -> share ~31% < 50% -> back to the worker.
	if rw := postWorker(t, h); rw.Code != http.StatusOK {
		t.Fatalf("third request: status %d: %s", rw.Code, rw.Body.String())
	}
	if workerHits.Load() != 2 || mainHits.Load() != 1 {
		t.Fatalf("after 3rd: worker=%d main=%d, want 2/1 (flip back)", workerHits.Load(), mainHits.Load())
	}

	if err := s.rec.Close(); err != nil {
		t.Fatal(err)
	}
	lines := lastLogLines(t, dataDir)
	if len(lines) != 3 {
		t.Fatalf("want 3 log lines, got %d", len(lines))
	}
	wantRouted := []string{stats.RoutedWorker, stats.RoutedDiverted, stats.RoutedWorker}
	for i, l := range lines {
		if l["routed"] != wantRouted[i] {
			t.Errorf("line %d routed = %v, want %s", i, l["routed"], wantRouted[i])
		}
	}
	if lines[1]["leg"] != string(stats.LegMain) || lines[1]["endpoint"] != budgetEndpoint {
		t.Errorf("diverted line = %v, want leg=MAIN endpoint=%s", lines[1], budgetEndpoint)
	}
}

// TestBudgetDefaultNeverDiverts: 100 and the zero value both skip the budget
// logic entirely — every worker-tier request reaches the worker even when the
// window is saturated with worker tokens.
func TestBudgetDefaultNeverDiverts(t *testing.T) {
	for _, usage := range []int{0, 100} {
		var mainHits atomic.Int64
		worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, openAIOK)
		}))
		mainUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mainHits.Add(1)
		}))

		s, _ := newFallbackServer(t, config.Config{
			MainUpstreamURL:     mainUp.URL,
			WorkerAPIBase:       worker.URL + "/v1",
			CustomSubagentUsage: usage,
		})
		h := s.Handler()
		for i := 0; i < 3; i++ {
			if rw := postWorker(t, h); rw.Code != http.StatusOK {
				t.Fatalf("usage=%d request %d: status %d", usage, i, rw.Code)
			}
		}
		if mainHits.Load() != 0 {
			t.Errorf("usage=%d: main upstream hit %d times, want 0", usage, mainHits.Load())
		}
		worker.Close()
		mainUp.Close()
	}
}
