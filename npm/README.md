# rig-move-llm

Move the heavy lifting off your paid LLM. A local proxy that lets Claude Code keep
planning on your paid subscription while every code change, edit, test run, and
knowledge lookup is delegated to a **worker model of your choice** — a local model
(Ollama / llama.cpp / ExLlama) or any API endpoint (OpenRouter, …).

```sh
npx rig-move-llm                  # one command → the setup wizard (installs itself on confirm)
claude                            # plain Claude Code — auto-delegates to the worker, no flags
```

`npx rig-move-llm` runs an interactive wizard: pick the scope (`global` = every project,
follows you; or `project`), then set a worker endpoint — or press Enter to **skip** it, which
installs rig inert so Claude Code runs normally until you turn it on (`ENABLED` in `config.env`).
Because the hooks call `rig-move-llm`, the wizard offers to `npm install -g` itself so it stays on
PATH — the single `npx` command sets up everything.
Global scope registers the worker at user scope and auto-creates a `.rig-move-llm/` in each project
on first session, the way Serena creates `.serena/`. Reverse it with `rig-move-llm uninstall`.

This npm package ships a single prebuilt static binary per platform (via
`optionalDependencies`, the esbuild/biome pattern — no postinstall download).
Source, docs, and releases: https://github.com/Cheevatech/rig-move-llm
