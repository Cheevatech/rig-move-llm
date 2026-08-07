# rig-move-llm

**Run Claude Code on your own model.** rig sits between Claude Code and Anthropic and answers
turns from a worker endpoint of your choice — your own llama.cpp / Ollama / ExLlama, or any
OpenAI-compatible API — with Claude Code itself unmodified and your subscription untouched for
every turn it serves.

**Check whether you need it first.** If your endpoint already speaks the Anthropic Messages
API — recent llama.cpp does — set `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN` and
`ANTHROPIC_MODEL` and point Claude Code straight at it. No proxy, nothing to install. rig is
for endpoints that speak OpenAI and nothing else: Ollama, OpenRouter, Tabby, older llama.cpp.

It can also swap the model *mid-session* — `/worker on`, and the next turn is answered by your
model in the same conversation with the same history. That switch is why this tool exists, and
it is the part that did not survive measurement: see below.

```sh
npx rig-move-llm                  # interactive setup wizard (offers to install itself)
rig-move-llm run -- claude        # launch Claude Code with the proxy wired in
```

```
/worker on                        # (inside the session) next turn runs on your worker
/worker off                       # (inside the session) next turn runs on Claude again
```

Either model can throw the switch itself, and so can you: `rig-move-llm worker on|off|status`
from a second terminal is the one path that works no matter which model is currently driving,
because it does not go through a model at all.

`run` is required — it is what sets `ANTHROPIC_BASE_URL`, and it sets it for that one process
only, so it never leaks into your other projects. A bare `claude` does not go through rig.

There is **no second agent, no subprocess, and no delegation**. Claude Code never learns the
model behind the endpoint changed; it keeps its own tools, permissions, and transcript.

**Nothing certifies the worker's output.** rig has no gate, no verdict, no pass/fail. You read
the diff — that is the check. It is worth taking literally: in a backtest on one real
repository, most of what came back needed rejecting, and twice the worker's own tests passed
while the code was wrong. A gate would have reported what those tests reported.

There is **no savings number here, on purpose.** Offloaded turns are not billed to your
Anthropic quota, and that is verifiable at the wire. Whether it is *worth it* depends on how
much of the offloaded work you accept — and the one measurement of both halves points in
opposite directions, so the repo README quotes neither figure.

**The recommended use case is now measured, and it is narrow.** Sixteen runs over four
commits from one real repository, replayed from the parent with the author's own test planted
as the acceptance file, every diff read by a human: doing it with no offloading at all
accepted 4/4 in 14 minutes; the worker from the first turn accepted 3/4 in 55 minutes without
touching quota; handing off mid-session — the switch, the whole idea — also accepted 3/4, took
37% longer, failed the identical task in the identical way, and spent paid quota getting
there. A frozen acceptance test did not reduce false success either; it moved it, from
rewriting the test to silently skipping the half of the task the test did not cover.

**So: if your endpoint already speaks the Anthropic Messages API — recent llama.cpp does —
you do not need this.** Set `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN` and
`ANTHROPIC_MODEL` and point Claude Code straight at it. rig is for the endpoint that speaks
OpenAI and nothing else: Ollama, OpenRouter, Tabby, older llama.cpp. Expect about four times
the wall clock, expect to read every diff, and do not expect the mid-session switch to earn
its keep.

> **0.8 is a breaking change.** The delegate arm is gone: the force-delegate hook, the MCP
> delegate tool, `rig-move-llm watch`, the deterministic gate on the worker's result, and
> engine selection — along with the savings figure that was measured on them. `ENABLED` now
> gates routing for real (before 0.8, `disable` printed success and routed anyway), and
> `rig-move-llm qwen` is now `worker`. Every token ledger written before 0.8 under-reports
> input by orders of magnitude; run `rig-move-llm stats --reset` after upgrading.
> To upgrade: `rig-move-llm uninstall`, then `init`.

This npm package ships a single prebuilt static binary per platform (via
`optionalDependencies`, the esbuild/biome pattern — no postinstall download).
Source, docs, and releases: https://github.com/Cheevatech/rig-move-llm
