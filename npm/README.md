# rig-move-llm

Move the heavy lifting off your paid LLM. Claude Code keeps planning and scoping on your
paid subscription and hands the implementation to a **worker model of your choice** — a
local model (Ollama / llama.cpp / ExLlama) or any API endpoint (OpenRouter, …). The worker
runs the full Claude Code harness on your endpoint: it reads, edits, and runs your tests,
then hands back the diff.

**You read the diff.** That is the check — rig does not certify the worker's output, and it
does not ask the paid agent to certify it either.

```sh
npx rig-move-llm                  # one command → the setup wizard (installs itself on confirm)
claude                            # plain Claude Code — no flags, no wrapper
rig-move-llm watch                # follow what the worker is doing, live
```

`npx rig-move-llm` runs an interactive wizard: pick the scope (`global` = every project,
follows you; or `project`), then set a worker endpoint — or press Enter to **skip** it, which
installs rig inert so Claude Code runs normally until you turn it on (`ENABLED` in `config.env`).
The wizard offers to `npm install -g` itself so the binary stays on PATH, since the MCP entry
invokes it. Global scope registers the worker at user scope so every project offloads with no
per-project setup. Reverse everything with `rig-move-llm uninstall`.

rig installs **no hooks**. It writes a `CLAUDE.md` explaining why implementation is cheaper on
the other model, and Claude decides — a guide that gets ignored is visible in your transcript,
while a hook that silently stops firing is not.

> **0.8 is a breaking change.** The force-delegate hook, the deterministic gate on the worker's
> result, and the engine selection are gone — along with the savings figure that was measured on
> them. `uninstall` cleans up an older install and `init` writes the new one. See the repo README.

This npm package ships a single prebuilt static binary per platform (via
`optionalDependencies`, the esbuild/biome pattern — no postinstall download).
Source, docs, and releases: https://github.com/Cheevatech/rig-move-llm
