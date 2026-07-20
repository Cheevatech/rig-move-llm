# rig-move-llm

Move the heavy lifting off your paid LLM. A local proxy that lets Claude Code keep
planning on your paid subscription while every code change, edit, test run, and
knowledge lookup is delegated to a **worker model of your choice** — a local model
(Ollama / llama.cpp / ExLlama) or any API endpoint (OpenRouter, …).

```sh
npx rig-move-llm@latest init      # auto-detects a local worker; wires this project
claude                             # plain Claude Code — auto-delegates to the worker, no flags
```

`init` wires Claude Code so a plain `claude` offloads code work to the worker with no
flags or wrapper. `npx rig-move-llm run -- claude` also works when you want the proxy's
per-project routing / observability on the main leg. Add `--global` to `init` to install
for every project; reverse it with `npx rig-move-llm uninstall`.

This npm package ships a single prebuilt static binary per platform (via
`optionalDependencies`, the esbuild/biome pattern — no postinstall download).
Source, docs, and releases: https://github.com/Cheevatech/rig-move-llm
