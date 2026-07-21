package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/Cheevatech/rig-move-llm/internal/config"
	"github.com/Cheevatech/rig-move-llm/pkg/translate"
)

// P2: the implement loop must stop at the iteration cap and surface an explicit
// hit_iteration_cap flag + warning summary for MAIN's review.
func TestImplementHitsIterationCap(t *testing.T) {
	t.Setenv("RIG_WORKER_MAX_ITERS", "2")
	dir := gitRepo(t)
	srv := fakeBackend(t, []translate.OpenAIResponse{
		toolCallResp("c1", "read_file", `{"path":"app.py"}`),
		toolCallResp("c2", "read_file", `{"path":"app.py"}`),
	})
	defer srv.Close()
	e := NewEngine(config.Config{WorkerAPIBase: srv.URL})
	res := e.Implement(context.Background(), dir, "loop forever", "")
	if res.Stopped != "max_iters" || !res.HitIterationCap {
		t.Fatalf("expected max_iters + hit flag, got %+v", res)
	}
	if !strings.Contains(res.Summary, "iteration cap") {
		t.Fatalf("summary must warn about the cap: %q", res.Summary)
	}
	if res.Iterations != 2 {
		t.Fatalf("iterations = %d, want 2", res.Iterations)
	}
}
