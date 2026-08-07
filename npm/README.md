# rig-move-llm

**Move the heavy lifting off your paid LLM.** rig is a switch. It sits between Claude Code
and Anthropic, and it lets you change *which model answers the next turn* — from your paid
Claude to a worker model of your choice (your own local llama.cpp / Ollama / ExLlama, or any
OpenAI-compatible API) — **in the middle of a live session, without restarting it and without
losing the context you have built up.**

Plan and scope on Claude. Type `/worker on`. The next turn is answered by your own model, in
the same conversation, with the same history, driving the same Claude Code toolset. Type
`/worker off` to hand the wheel back.

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
