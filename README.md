# rig-move-llm

**Move the heavy lifting off your paid LLM.** A local proxy that lets Claude Code keep planning on your paid subscription while every code change, file edit, test run, and knowledge lookup is delegated to a **worker model of your choice** — your own local model (llama.cpp / Ollama / ExLlama) or any API endpoint (OpenRouter, …). The worker's output is verified by a deterministic gate before it ever reaches the paid agent, so you save tokens *without* trading away correctness.

> Status: **early / pre-release.** Proxy, translation, hooks, installer, and the reboot-safe daemon are working and validated end-to-end. The savings numbers below come from a small pilot (n=7) — treat them as evidence the mechanism works, then measure on your own workload with `rig-move-llm stats`.

## Why

On a subscription, the only lever is **paid-agent output tokens**. `rig-move-llm`:

- **Offloads** every code change to a worker tool (`mcp__worker__implement`) that runs on *your* endpoint, out-of-process; the main agent stays on Anthropic direct and only plans/reviews.
- **Forces delegation** structurally — the paid agent plans/reviews only; a hook denies it the mutating/heavy tools so the work goes to the free/cheap worker.
- **Gates the result** deterministically (frozen fail-before repro + compile/lint floor + scoped regression) so a "cheaper" answer can't be a wrong answer.
- **Brings your own model** — local (free compute) or API (cheap, not free). Nothing points at anyone else's compute.

Headless / AFK oriented (worker models are slower than the frontier); not aimed at interactive latency.

## What you save — honest numbers

Measured on a 7-task SWE-bench-style Python pilot (main agent = Claude Sonnet both
sides, worker = a local 27B model, delegation enforced 100% with zero leaks):

- **Quality: parity, not magic.** 5/7 resolved hybrid vs 5/7 solo. (An earlier run
  showed 6/7 — one apparent win did not reproduce under re-scoring, so we quote the
  conservative number.)
- **Paid-agent output tokens: −28% blended per resolved task** (−14% across the whole
  run), with **~136k output tokens moved off quota** onto the worker. But the
  distribution is **bimodal**: heavy tasks saved up to −64%, while light tasks
  originally *inverted* (+46%, +112% — the fixed plan/review overhead cost more than
  the work saved).
- **The cut-review gate is the cost floor.** It replaces the paid agent's re-review of
  worker output with a deterministic gate (frozen fail-before repro + compile/lint
  floor + scoped regression). Re-running the two inverted tasks with it: **+46% → −4.9%**
  and **+112% → −26%**, same resolution outcomes, **zero fail-open** across all verdict
  paths. This floor is structural — it does not depend on how good your worker model is.
- **Wall time: ~8× slower** than solo (worker throughput). This tool is for headless /
  AFK / overnight work, not interactive sessions.

### What "savings" means, precisely

- **Quota is counted server-side.** Worker requests go to *your* endpoint and never
  reach api.anthropic.com — worker generation is 100% outside your Anthropic quota.
- **It is not "100% free."** Worker output that the main agent reads back counts as
  main-agent *input* tokens. Input is ~5× cheaper than output, and cut-review shrinks
  what the main agent reads to a short verdict — but it is not zero.
- **Client-side displays lie a little.** Claude Code's `/cost` (and tools like ccusage)
  count tokens the client saw, including rerouted worker traffic — that display is
  cosmetic, not your bill. `rig-move-llm stats` separates the legs honestly; its
  main-leg counts are validated to match Anthropic's own reported `input_tokens` /
  `output_tokens` exactly on a live instance. (Prompt-cache reads/writes are billed
  by Anthropic but not yet tracked in the ledger — the savings claims above are
  output-token-centric, which caching doesn't touch.)
- **Two worker tiers, two claims.** A **local** worker (llama.cpp / Ollama / vLLM) is
  free compute — heavy work leaves your quota entirely. An **API** worker (OpenRouter
  etc.) is *cheaper, not free*: you pay per token, typically well below Claude output
  rates, and tool-call/streaming support varies by upstream model (which can affect
  how much actually gets delegated).
- **Variance disclosure.** Worker patches vary run-to-run (we observed two equivalent
  patches differing by one line with different hidden-test outcomes). With arbitrary
  user endpoints the variance is larger. We therefore claim the *cost floor* and
  *quality parity mechanism*, not absolute resolve rates — **measure on your own
  workload** (`rig-move-llm stats --history`).

## Architecture (one binary)

```
Claude Code ──> rig-move-llm proxy ─> api.anthropic.com   (main leg: raw passthrough, OAuth untouched, usage metered)
     │          (ANTHROPIC_BASE_URL)
     └─ mcp__worker__implement ─────> your endpoint         (worker leg: out-of-process MCP tool, Anthropic<->OpenAI, off the paid ledger)
```

The offload runs through a worker MCP tool, not the proxy: on Claude Code 2.1.x native
subagents run in-process and never egress to a base-URL proxy, so the main agent delegates
code work to `mcp__worker__implement` (out-of-process, guaranteed to reach your worker
endpoint). The proxy is the **main-leg observability layer** — it forwards the paid traffic
verbatim and meters what it spends. One static Go binary, stdlib-only, replaces what
previously needed a Node shim **and** Python LiteLLM; cross-compiles to macOS / Linux /
Windows (amd64 + arm64) with zero toolchain.

## Configuration (bring-your-own endpoint)

```sh
WORKER_API_BASE=http://localhost:11434/v1        # Ollama / llama.cpp / ExLlama(TabbyAPI) — local or over Tailscale
WORKER_MODEL=qwen2.5-coder:32b
WORKER_API_KEY=...                               # or an OpenRouter key: https://openrouter.ai/api/v1
MAIN_UPSTREAM_URL=https://api.anthropic.com
PORT=4000
```

Install **local** (this project only) or **global** (all projects) via the `init` bootstrap.

## Install & use

```sh
npx rig-move-llm@latest init      # auto-detects a local Ollama/llama.cpp; wires this project
claude                             # plain Claude Code — auto-delegates to the worker, no flags
```

`init` auto-wires Claude Code so a **plain `claude`** (no flags, no wrapper) offloads to the
worker: it writes a project-root `.mcp.json` (the `mcp__worker__implement` tool, auto-discovered),
pre-approves it in `.claude/settings.json` (`enableAllProjectMcpServers`, so headless `-p` does not
hang on the trust prompt), installs the force-delegate + gate hooks, and drops a terse
plan→delegate→review output style (`.claude/output-styles/rig-delegate.md`) that keeps the paid
agent's output small. It also writes `.rig-move-llm/config.env`, probing `localhost:11434` (Ollama)
and `:8080` (llama.cpp) so config is near-zero. Add `--global` for every project (`~/.claude` +
`~/.rig-move-llm`); local overrides global. `rig-move-llm run -- claude` remains available when you
also want the proxy's per-project routing / observability on the main leg (it sets
`ANTHROPIC_BASE_URL` for that process only, so local scope never leaks). Reverse everything with
`rig-move-llm uninstall` (restores your `settings.json` verbatim).

The npm package ships a single prebuilt static binary per platform via
`optionalDependencies` (the esbuild/biome pattern — no postinstall download).

### Permissions posture (headless)

Claude Code's auto-mode runs a model-based safety classifier before auto-approving
Bash commands. In a headless hybrid run the main agent is already structurally denied
mutating tools by the rig hook, and the worker's own tools run out-of-process on *your*
endpoint — so the classifier is a redundant layer on the main leg. If you turn it off
(headless allowlist / bypass permissions), understand what that means: you are trusting
the hook + your own sandboxing instead of a second model opinion. Do this only for
unattended runs in an environment you'd let a CI job loose in.

Subcommands:

```
rig-move-llm serve [--port N] [--status]     run the routing proxy / report state
rig-move-llm hook  pre-tool|post-tool        Claude Code hook (force-delegate + gate)
rig-move-llm init  [--global] [--service] [flags]  bootstrap config + Claude Code wiring
                                             (--service: OS-supervised, survives reboots)
rig-move-llm uninstall [--global] [--purge]  reverse init for a scope (incl. OS service)
rig-move-llm run   [--] <command...>         launch a command with the proxy wired in
rig-move-llm stats [--reset|--history]       token accounting / savings
```

### Agent teams (experimental)

Claude Code's experimental agent teams work under rig with no extra setup. In the
default **in-process** backend each teammate shares the lead's process and its tool
calls already carry an `agent_id`, so the force-delegate hook treats teammates as
workers automatically while the lead stays plan/delegate/review-only.

The **terminal backends** (`--teammate-mode tmux|iterm2`) spawn each teammate as a
separate `claude` process whose hook payloads have no `agent_id` — they would
otherwise be mistaken for the paid lead and denied every tool. `rig-move-llm run`
points `CLAUDE_CODE_TEAMMATE_COMMAND` at itself so it can stamp the teammate's
identity (`RIG_AGENT_ID`, which the hook honors like `agent_id`) and, by default,
pin the teammate to the worker tier so its inference runs on your endpoint. Set
`RIG_TEAMMATE_MODEL=inherit` to keep the model the lead requested, or
`RIG_TEAMMATE_MODEL=<name>` to force a specific one. A launcher you set yourself in
`CLAUDE_CODE_TEAMMATE_COMMAND` is never overwritten. Teams are interactive-only
(headless `-p` has no teams), so this path is not exercised by rig's CI smokes.

## Layout

```
cmd/rig-move-llm/   entrypoint
internal/cli/       subcommand dispatch (serve/hook/init/run/stats)
internal/service/   OS supervision (launchd / systemd --user / Task Scheduler), stdlib-only
internal/proxy/     main-leg observability (raw Anthropic passthrough + usage metering)
internal/worker/    the worker MCP tool (mcp__worker__implement): agentic loop on your endpoint
internal/hook/      force-delegate + deterministic-gate hooks (Go, no shell)
internal/config/    layered .env config + backend registry (Ollama first-class)
pkg/translate/      Anthropic <-> OpenAI translation library (importable, 27 conformance tests)
```

## Build

```sh
go build -o rig-move-llm ./cmd/rig-move-llm     # Go 1.22+: works as-is on all platforms
# Go 1.21 on macOS only (LC_UUID bug): add -ldflags=-linkmode=external && codesign -s - -f rig-move-llm
```

Cross-compile is pure-Go (`CGO_ENABLED=0`); CI builds all six targets
(darwin/linux/windows × amd64/arm64) — see `.github/workflows/build.yml`.

## License

TBD.
