package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type testLedger struct {
	Since     string `json:"since"`
	MainIn    int64  `json:"main_in"`
	MainOut   int64  `json:"main_out"`
	WorkerIn  int64  `json:"worker_in"`
	WorkerOut int64  `json:"worker_out"`
	NMain     int64  `json:"n_main"`
	NWorker   int64  `json:"n_worker"`
}

func readLedger(t *testing.T, dir string) testLedger {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "stats.json"))
	if err != nil {
		t.Fatalf("read stats.json: %v", err)
	}
	var l testLedger
	if err := json.Unmarshal(data, &l); err != nil {
		t.Fatalf("parse stats.json: %v", err)
	}
	return l
}
