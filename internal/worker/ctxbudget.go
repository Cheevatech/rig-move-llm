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

// overCtxBudget reports whether promptTokens (the real context size the endpoint
// saw last turn) has reached the budget. This is the one predicate both legs use
// to decide when to checkpoint.
func overCtxBudget(promptTokens, limit int) bool {
	return limit > 0 && promptTokens >= limit
}
