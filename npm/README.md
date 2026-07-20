# rig-move-llm

Move the heavy lifting off your paid LLM. A local proxy that lets Claude Code keep
planning on your paid subscription while every code change, edit, test run, and
knowledge lookup is delegated to a **worker model of your choice** — a local model
(Ollama / llama.cpp / ExLlama) or any API endpoint (OpenRouter, …).

```sh
npm install -g rig-move-llm       # the hooks need the binary on PATH
rig-move-llm                       # guided setup wizard: scope + worker + wiring
claude                             # plain Claude Code — auto-delegates to the worker, no flags
```

A bare `rig-move-llm` runs an interactive wizard: pick the scope (`global` = every project,
follows you; or `project`), then set a worker endpoint — or press Enter to **skip** it, which
installs rig inert so Claude Code runs normally until you turn it on (`ENABLED` in `config.env`).
Global scope registers the worker at user scope and auto-creates a `.rig-move-llm/` in each project
on first session, the way Serena creates `.serena/`. Reverse it with `rig-move-llm uninstall`.

This npm package ships a single prebuilt static binary per platform (via
`optionalDependencies`, the esbuild/biome pattern — no postinstall download).
Source, docs, and releases: https://github.com/Cheevatech/rig-move-llm
