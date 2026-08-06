package cli

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Cheevatech/rig-move-llm/internal/config"
)

// stats is the cumulative token ledger persisted at <dataDir>/stats.json.
// The recording side (proxy instrumentation + MAIN-leg SSE token extraction) is
// implemented in ticket P6; this command is the read/reset surface it writes to.
type stats struct {
	Since string `json:"since"`
	// The paid leg's input arrives in three separately-priced parts. Reporting only
	// the first is how this ledger used to show 2 tokens for a turn whose real input
	// was 57,951 — on a warm Claude Code session nearly the whole prompt is a cache
	// read. They are kept apart here because a cache read costs about a tenth of
	// fresh input: summing them would misstate money, dropping them misstates volume.
	MainIn        int64 `json:"main_in"`          // uncached input (Anthropic input_tokens)
	MainCacheRead int64 `json:"main_cache_read"`  // cache_read_input_tokens (~0.1x price)
	MainCacheWrit int64 `json:"main_cache_write"` // cache_creation_input_tokens (~1.25x)
	MainOut       int64 `json:"main_out"`         // billed output tokens
	WorkerIn      int64 `json:"worker_in"`        // offloaded prompt tokens (no cache split)
	WorkerOut     int64 `json:"worker_out"`       // offloaded output tokens
	NMain         int64 `json:"n_main"`           // main-leg request count
	NWorker       int64 `json:"n_worker"`         // worker-leg request count
}

func cmdStats(args []string) int {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	reset := fs.Bool("reset", false, "clear the token ledger and request log")
	history := fs.Bool("history", false, "print the per-request log (requests.jsonl)")
	_ = fs.Parse(args)

	dir := config.Load().DataDir
	statsPath := filepath.Join(dir, "stats.json")
	logPath := filepath.Join(dir, "logs", "requests.jsonl")

	if *reset {
		_ = os.Remove(statsPath)
		_ = os.Remove(logPath)
		fmt.Println("cleared token ledger and request log")
		return 0
	}

	if *history {
		f, err := os.Open(logPath)
		if err != nil {
			fmt.Println("no request log yet")
			return 0
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			fmt.Println(sc.Text())
		}
		return 0
	}

	data, err := os.ReadFile(statsPath)
	if err != nil {
		fmt.Println("no records yet — run some traffic through `serve` first")
		return 0
	}
	var s stats
	if err := json.Unmarshal(data, &s); err != nil {
		fmt.Fprintln(os.Stderr, "stats: corrupt ledger:", err)
		return 1
	}
	printStats(s)
	return 0
}

func printStats(s stats) {
	offloaded := s.WorkerIn + s.WorkerOut
	mainInTotal := s.MainIn + s.MainCacheRead + s.MainCacheWrit
	billed := mainInTotal + s.MainOut
	total := offloaded + billed
	var ratio float64
	if total > 0 {
		ratio = float64(offloaded) / float64(total) * 100
	}
	fmt.Printf("rig-move-llm token accounting (since %s)\n", orMain(s.Since))
	fmt.Printf("  worker (offloaded, off-quota): %d in + %d out = %d tok over %d req\n",
		s.WorkerIn, s.WorkerOut, offloaded, s.NWorker)
	fmt.Printf("  main   (billed, Anthropic):    %d in + %d out = %d tok over %d req\n",
		mainInTotal, s.MainOut, billed, s.NMain)
	// The input split is printed because the parts are priced differently — a cache
	// read is roughly a tenth of fresh input. Without it the "in" total reads as if
	// it were all full-price, which is the opposite of the error it replaced.
	fmt.Printf("           input split:          %d fresh + %d cache-read + %d cache-write\n",
		s.MainIn, s.MainCacheRead, s.MainCacheWrit)
	fmt.Printf("  offload ratio: %.1f%% of tokens routed to the worker\n", ratio)
}

func orMain(v string) string {
	if v == "" {
		return "start"
	}
	return v
}
