# rig-move-llm

**Move the heavy lifting off your paid LLM.** rig is a switch: Claude Code keeps planning and scoping on your paid subscription, and hands the actual implementation to a **worker model of your choice** — your own local model (llama.cpp / Ollama / ExLlama) or any API endpoint (OpenRouter, …). The worker runs the *full* Claude Code harness on your endpoint: it reads, edits, and runs your tests. Then it hands you back the diff.

**You read the diff.** That is the check. rig does not certify the worker's output, and it does not ask the paid agent to certify it either.

> Status: **pre-release, and the architecture just changed.** See [What changed in 0.8](#what-changed-in-08) — the previous design (a hook that denied the paid agent its tools, plus a deterministic gate on the worker's result) has been removed, along with its savings measurement.

## Why

On a subscription, the only lever is **paid-agent output tokens**. rig moves the token-expensive part — investigating, editing, running tests, retrying — onto an endpoint you control.

Three moving parts, and deliberately no fourth:

- **The switch.** One MCP tool, `mcp__worker__implement`. It spawns `claude -p` against your endpoint, in your repo, and returns what happened.
- **The guards.** A ceiling on silence and a ceiling on total time, so a small model that gets lost stops burning instead of running until someone notices.
- **A way to see it and stop it.** `rig watch` follows what the worker is doing, live, one line per action — and cancelling actually kills the process tree (see [Stopping a run](#stopping-a-run)).

Headless / AFK oriented: worker models are slower than the frontier. This is for work you are willing to walk away from, not for interactive latency.

## What this claims, and what it does not

**The mechanism is real and checkable:** worker inference goes to *your* endpoint and never reaches api.anthropic.com. `rig-move-llm doctor` proves each link before you trust any number, and `rig-move-llm stats` separates the legs.

**There is currently no savings number, on purpose.** The −37% figure this README used to carry was measured on the previous architecture — the one with the force-delegate hook and the deterministic gate — and both of those are gone. Quoting a number produced by machinery that no longer ships would be worse than quoting none. A fresh A/B against the current design has not been run yet.

What is still true regardless of architecture:

- **Quota is counted server-side.** Worker generation is 100% outside your Anthropic quota when the worker is a local model.
- **It is not "100% free."** The diff the worker returns becomes paid-agent *input* when the paid agent reads it back. Input is far cheaper than output, but it is not zero.
- **Client-side displays lie a little.** Claude Code's `/cost` counts tokens the client saw, including rerouted worker traffic. That display is cosmetic, not your bill.
- **A local worker is free compute; an API worker is cheaper, not free.**
- **Worker output varies run to run.** Two runs of the same task can produce materially different patches. This is the reason the design ends with a human reading the diff rather than with a gate declaring success — see the worked example in [What changed in 0.8](#what-changed-in-08).

## Architecture (one binary)

```
Claude Code ──> rig-move-llm proxy ─> api.anthropic.com   (main leg: raw passthrough, OAuth untouched, usage metered)
     │          (ANTHROPIC_BASE_URL)
     └─ mcp__worker__implement ─────> your endpoint       (worker leg: out-of-process, off the paid ledger)
```

The offload runs through an MCP tool, not the proxy: on Claude Code 2.1.x native subagents run in-process and never egress to a base-URL proxy, so an MCP server is the only place a second model can actually be substituted. The proxy is the **main-leg observability layer** — it forwards paid traffic verbatim, meters what it spends, and serves the Anthropic-format endpoint the worker points at (`/r/worker`).

One static Go binary, stdlib-only. Cross-compiles to macOS / Linux / Windows (amd64 + arm64) with zero toolchain.

## Configuration (bring-your-own endpoint)

```sh
WORKER_API_BASE=http://localhost:11434/v1        # Ollama / llama.cpp / ExLlama(TabbyAPI) — local or over Tailscale
WORKER_MODEL=qwen2.5-coder:32b
WORKER_API_KEY=...                               # or an OpenRouter key: https://openrouter.ai/api/v1
MAIN_UPSTREAM_URL=https://api.anthropic.com
PORT=4000

RIG_CC_BASE_URL=http://127.0.0.1:4000/r/worker   # REQUIRED: Anthropic-format endpoint for the worker
RIG_CC_MODEL=haiku                               # model name the subprocess runs as
```

`RIG_CC_BASE_URL` is not optional and has no default. The switch **refuses to launch** without it rather than let the worker's inference bill your paid account — the failure mode it prevents is silent and expensive, so it is a hard stop rather than a warning. rig serves a suitable endpoint itself: run `rig-move-llm serve` and point at its `/r/worker` route, which translates Anthropic `/v1/messages` to the OpenAI-compatible endpoint configured above.

The setup wizard collects all of this; you should not need to hand-edit `config.env`. Scope is **global** (all projects, follows you) or **project** (this dir only); `ENABLED` gates the whole thing on and off.

## Guards: what bounds one delegation

Two ceilings, both env-overridable, and they must stay in order — the run has to be the thing that kills itself, so it can return a diagnosis *and the partial diff* instead of leaving the caller with a bare client-side abort.

```sh
RIG_THIN_STALL=600        # seconds of total SILENCE before the run is killed (default 10m)
RIG_THIN_WALL=3000        # seconds of total run time (default 50m)
```

Silence is a real liveness signal: a working agent emits a stream event per tool call. The one honest long silence is a `Bash` call that produces nothing until it returns, which Claude Code lets run about ten minutes — so the stall ceiling sits just past that.

`rig-move-llm doctor` prints the effective ladder (`stall < wall < client`) and the source of any env override, because a silent override is how two runs get compared as though they were the same configuration.

**A killed run still returns its diff.** Work that reached the working tree belongs to you regardless of how the process ended.

## Stopping a run

Cancelling a tool call in Claude Code, `SIGTERM`ing the MCP server, `SIGKILL`ing it, or closing its stdin all kill the worker and its whole process tree, within a second or so.

This is called out explicitly because it did not use to be true, in any of those four ways: the server read stdin synchronously while a tool call ran, so a cancellation sat unread in a pipe buffer until the work it was cancelling had already finished. A worker could keep burning for over an hour after you pressed Esc. rig now reads stdin on its own goroutine, binds each call's context to its request id, and runs the worker under a small supervisor process that kills the tree if it is ever orphaned — which is the only thing that can cover `SIGKILL` of the server itself.

## Install & use

```sh
npx rig-move-llm                   # one command → the setup wizard (installs itself on confirm)
claude                             # plain Claude Code — no flags, no wrapper
```

`npx rig-move-llm` (or `rig-move-llm setup` if already installed) runs an **interactive wizard**: it asks the **scope** (`global` = every project, follows you; or `project` = this dir), then the **worker endpoint** — which you can **skip** by pressing Enter, leaving rig installed but *off*. It auto-detects a local Ollama/llama.cpp and offers it as the default.

The wizard wires Claude Code so a plain `claude` can offload with no flags:

- `.mcp.json` — the `worker` MCP server, pre-approved via `enableAllProjectMcpServers` so a headless `claude -p` never hangs on a trust dialog
- `.claude/settings.json` — a permission grant for the one tool
- `.claude/CLAUDE.md` — the steer (see below)
- `.claude/commands/qwen.md` — a `/qwen <task>` command for handing work over directly
- `.rig-move-llm/config.env` — your configuration

**rig installs no hooks.** The steer is guidance, not enforcement: it explains that implementation is cheaper on the other model and that the human reads the diff. Claude may still edit a file itself, and for a one-line change it probably should. A guide that is ignored is visible in your transcript; a hook that silently stops firing is not, which is the failure this design is built around.

**Global (follows you):** registers the worker at **user scope** in `~/.claude.json`, so every project offloads with no per-project setup, inheriting every setting from the global config — a later change to `~/.rig-move-llm/config.env` propagates everywhere. Add a `KEY=value` in a project's own `.rig-move-llm/config.env` only to override one setting there.

**Is it actually live?** `rig-move-llm doctor` runs the pre-flight ladder and exits non-zero if any rung fails: config + `ENABLED`; a **real completion** against the worker endpoint (a probe against `/v1/models` answers 200 on some servers even when the API key is wrong, so it cannot see an auth failure); the switch itself (the `claude` CLI on PATH and an Anthropic-format endpoint that answers); Claude Code's workspace trust; **whether the command the MCP registration names is on PATH** — a global install can be written perfectly and still leave Claude Code reporting `worker: failed` because the binary was never installed globally; the guard ladder; and whether the paid leg has credentials to open a session at all. Run it before you trust any measurement — a rig that looks configured can still be delegating nothing.

**On / off.** `ENABLED` in `config.env` is the master toggle. Flip it with `rig-move-llm enable` / `rig-move-llm disable` (add `--local` for this project only). `rig-move-llm config` prints the effective configuration and which scope won; `--open` edits it. Reverse everything with `rig-move-llm uninstall` — it restores your `settings.json` verbatim, strips the user-scope registration, reverts a workspace-trust grant rig made, and removes files written by *older* versions too (the enforcement-era output styles and hooks), while leaving hooks and permissions you added yourself untouched.

`rig-move-llm init [--global] [--npx] [flags]` is the non-interactive form for scripts. The npm package ships one prebuilt static binary per platform via `optionalDependencies` (the esbuild/biome pattern — no postinstall download).

## Headless (`claude -p`)

Three separate layers must all be satisfied or a headless run stalls waiting for a human who is not there:

1. **Workspace trust** — Claude Code ignores `permissions.allow` in a directory it does not trust. `rig-move-llm init --trust-workspace` grants it (and `uninstall` reverts it), or open the directory once interactively and accept the prompt.
2. **Tool permission** — the grant for `mcp__worker__implement`, written by init.
3. **MCP server approval** — `enableAllProjectMcpServers`, written by init.

`doctor` checks the first and will tell you when the other two are being ignored because of it.

## Watching a run

```sh
rig-move-llm watch            # follow the newest run, live
rig-move-llm watch --list     # recent runs, newest first, with their status
```

```
+00:39  Read   .../proj/utils.py
+00:49  Edit   .../proj/utils.py
+01:09  Bash   ./.venv/bin/python -m pytest -q
+01:09    ↳    FAILED · 1 failed, 5 passed
+01:24  Bash   ./.venv/bin/python -m pytest -q
+01:24    ↳    ok · 6 passed in 0.01s
+01:30  ──     finished
```

One line per action, stamped with time since the run started, so the question a
20-to-50-minute run actually raises — is it working, or is it stuck? — is answered by
looking. When nothing has happened for a while, watch says so and names the point at
which the stall guard will act. It exits by itself when the run ends.

This is deliberately not the raw event stream, which is also written (`stream.jsonl`)
and is what you dig through afterwards. Roughly 80% of what a worker emits is thinking
tokens; tailing that tells you nothing.

## Reading a run

`implement` returns four things and nothing else:

```
status: finished | killed(<why>) | error: <what>
log:    <path to this run's directory>

--- diff ---
<the diff, or its stat plus a path when it is large>

--- last command ---
<the last shell command the worker ran, and the tail of its output>
```

`status` describes the **subprocess** — whether it exited on its own or rig killed it, and why. It is a fact from the operating system, not a claim by the worker. There is deliberately no field asserting that the change is correct, complete, or tested: no verdict, no score, no pass/fail. Those are for you to conclude from the diff.

The log directory holds the action log, the raw event stream, the exact command that was run, the task as sent, the full diff, and the tool inventory the worker actually received.

## What changed in 0.8

0.8 removes the enforcement layer and everything built on top of it: the PreToolUse hook that denied the paid agent its editing tools, the deterministic gate on the worker's result, the proof-of-flip retry, the per-turn delegation budget, the tiered return, and the `explore`/`triage`/`show_change` tools. `internal/` went from ~25,400 lines to ~9,400.

The reason is a measurement. On a real task, on a real repo, the worker produced a change that passed the entire test suite — because it had quietly rewritten five existing assertions to match its new behaviour. No gate can catch that: the suite was green, and the suite was the thing that had been edited. A human reading the diff catches it in seconds.

Once the gate cannot be the check, the machinery that existed to feed the gate has no purpose — and that machinery was measured consuming about 74% of the tokens the worker generated and roughly half its wall time. The layer built to make delegation cheap was the most expensive thing in the system.

**Migration:** there is none, and none is needed — `uninstall` cleans up an old install, `init` writes the new one, and a `CLAUDE.md` written by an older rig is upgraded in place. Files you wrote yourself are never touched.

**Removed commands:** `rig hook`, `rig worker` (now `rig thin-worker`, invoked by the MCP config, not by hand), `rig cascade`, `rig teammate-exec`. **Removed config keys:** `RIG_WORKER_ENGINE`, `GATE_MODE`, `VERIFY_CMD`, `RIG_MAX_DELEGATE_ROUNDS`.

## Security posture

The worker subprocess runs with `--dangerously-skip-permissions`: it must edit files and run your tests unattended. Its blast radius is the repo checkout it was pointed at, and its inference is on your endpoint. It runs with every `RIG_*` variable stripped from its environment and with no MCP servers of its own, and it does not load your Claude Code settings — so your hooks, output styles, and agent identity do not follow it in. Web access is denied to it by default.

If that is not an acceptable blast radius for a given repo, do not point rig at that repo.

## Layout

```
cmd/rig-move-llm/   entry point
internal/cli/       subcommand dispatch (serve / init / doctor / stats / watch / thin-worker)
internal/thin/      the switch: the MCP server, the spawn, the guards, the kill path
internal/proxy/     main-leg passthrough + metering, Anthropic<->OpenAI translation, /r/<leg> routing
internal/config/    layered config (env > project > global)
internal/service/   OS service install (launchd / systemd)
internal/stats/     the token ledger
pkg/translate/      Anthropic <-> OpenAI message translation
```

## Build

```sh
go build ./cmd/rig-move-llm
# Go 1.21 on macOS only (LC_UUID bug): add -ldflags=-linkmode=external && codesign -s - -f rig-move-llm
```

## License

MIT
