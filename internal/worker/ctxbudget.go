package worker

// Context-budget watcher — the single knob + check both the explore round loop
// and the implement loop consult to keep a worker's conversation from outgrowing
// its context window (the failure mode film observed: a bloated context makes the
// worker hallucinate — one giant edit instead of small, verified steps).
//
// The watcher is deliberately dumb: every e.chat() returns usage.prompt_tokens =
// the real size of the context the endpoint just saw. When that crosses the
// budget, the caller checkpoints — explore emits its round digest and starts a
// fresh round; implement resets to a rig-assembled digest (see
// reassembleImplementMsgs). Neither asks the worker to summarize itself; rig owns
// the ground truth (files read, git diff, last test), so it reconstructs the
// digest deterministically. The worker stays a plain OpenAI endpoint that requires
// nothing special — the same principle as the resumable explore loop.

// defaultCtxLimit is the prompt-token ceiling that triggers a context checkpoint.
// It must sit BELOW the worker endpoint's real context window so the next turn's
// completion still fits: our server runs qwen at 64k, so 48k leaves headroom.
// Cloud models with 200k windows can raise it via RIG_WORKER_CTX_LIMIT.
const defaultCtxLimit = 48000

// ctxLimit resolves the configured prompt-token budget (RIG_WORKER_CTX_LIMIT).
// A value <= 0 (env "0" is ignored by envInt, but callers may pass one) disables
// the watcher, leaving only the iteration cap as the safety net.
func ctxLimit() int { return envInt("RIG_WORKER_CTX_LIMIT", defaultCtxLimit) }

// defaultExploreCtxLimit is a TIGHTER budget for the explore read loop than the
// implement loop's 48k. Explore is a read-only fan-out — it accumulates file
// contents fast — and its per-turn prompt is what the worker must PREFILL every
// iteration. On a mid-size local model a 48k prompt prefilled 26× is what made a
// real-repo explore take ~30 min (measured: 799k cumulative prompt tokens). A
// smaller ceiling makes each turn cheaper and checkpoints partial evidence more
// often, so a time-bounded explore still hands back something useful. Raise it
// for fast/cloud endpoints via RIG_EXPLORE_CTX_LIMIT.
const defaultExploreCtxLimit = 16000

// exploreCtxLimit is the explore loop's own context budget. It never exceeds the
// global ctxLimit (an operator who tightens RIG_WORKER_CTX_LIMIT tightens explore
// too), and defaults below it.
func exploreCtxLimit() int {
	lim := envInt("RIG_EXPLORE_CTX_LIMIT", defaultExploreCtxLimit)
	if g := ctxLimit(); g > 0 && lim > g {
		lim = g
	}
	return lim
}

// overCtxBudget reports whether promptTokens (the real context size the endpoint
// saw last turn) has reached the budget. This is the one predicate both legs use
// to decide when to checkpoint.
func overCtxBudget(promptTokens, limit int) bool {
	return limit > 0 && promptTokens >= limit
}
