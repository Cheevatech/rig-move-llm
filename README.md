# rig-move-llm

**Move the heavy lifting off your paid LLM.** A local proxy that lets Claude Code keep planning on your paid subscription while every code change, file edit, test run, and knowledge lookup is delegated to a **worker model of your choice** — your own local model (llama.cpp / Ollama / ExLlama) or any API endpoint (OpenRouter, …). The worker's output is verified by a deterministic gate before it ever reaches the paid agent, so you save tokens *without* trading away correctness.

> Status: **early / pre-release.** Proxy, translation, hooks, installer, and the reboot-safe daemon are working and validated end-to-end. The savings number below is a single measured instance (n=1) — treat it as evidence the mechanism produces a real saving on a real fix, then measure your own workload with `rig-move-llm stats`.

## Why

On a subscription, the only lever is **paid-agent output tokens**. `rig-move-llm`:

- **Offloads** every code change to a worker tool (`mcp__worker__implement`) that runs on *your* endpoint, out-of-process; the main agent stays on Anthropic direct and only plans/reviews.
- **Forces delegation** structurally — the paid agent plans/reviews only; a hook denies it the mutating/heavy tools so the work goes to the free/cheap worker.
- **Gates the result** deterministically (frozen fail-before repro + compile/lint floor + scoped regression) so a "cheaper" answer can't be a wrong answer.
- **Brings your own model** — local (free compute) or API (cheap, not free). Nothing points at anyone else's compute.

Headless / AFK oriented (worker models are slower than the frontier); not aimed at interactive latency.

## What you save

Measured on one hard SWE-bench-style Python instance (flask-4045), main agent = Claude
Sonnet, worker = a local 27B model, delegation enforced 100% by the hook:

- **Paid-agent output tokens: −37%** — 2,579 billed output tokens vs 4,069 solving the
  same fix solo. Every edit and test run — the heavy work — executed on the local
  worker, off your Anthropic quota. (A terser configuration reached −52% but skipped the
  review round that catches worker fallout; the shipping default keeps a mandatory
  regression check, so −37% is the number you actually get.)
- **Correctness held.** The target test passed and no pre-existing test regressed — and
  a solo baseline left this same instance *unresolved*, so here the hybrid solved what
  solo could not. This is not the general rule (see variance disclosure below); it is
  one honest data point.
- **Wall time: slower** — ~20 min here vs a few minutes solo (worker throughput). This
  tool is for headless / AFK / overnight work, not interactive latency.

This is **n=1**. It shows the mechanism produces a real output-token saving on a real
fix; it is not a benchmark. An earlier 7-task pilot of the predecessor (base-URL)
design showed the same *shape* — quality roughly at parity and output savings that are
**bimodal** (heavy tasks save the most; trivial tasks can cost more than they save,
because the fixed plan/review overhead dominates the little work delegated). That is why
this tool targets heavy, headless work. Measure your own with `rig-move-llm stats`.

### What "savings" means, precisely

- **Quota is counted server-side.** Worker requests go to *your* endpoint and never
  reach api.anthropic.com — worker generation is 100% outside your Anthropic quota.
- **It is not "100% free."** Worker output that the main agent reads back counts as
  main-agent *input* tokens. Input is ~5× cheaper than output, and the deterministic
  gate shrinks what the main agent reads back to a short verdict — but it is not zero.
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
WORKER_HEALTH_PATH=/v1/models                    # health-check probed each message; set off to disable
```

The setup wizard collects these for you — this is what it writes to `config.env`. Scope is
**global** (all projects, follows you) or **project** (this dir only); `ENABLED` gates the whole
thing on/off.

### Automatic worker fallback (zero-token)

The worker endpoint is bring-your-own, so it can be down when Claude Code is not. At the **start of
every message** rig fires a **health check** — a plain HTTP `GET` on `WORKER_HEALTH_PATH` (no LLM
tokens). If it **passes**, offload runs normally. If it **fails**, that turn automatically degrades
to plain Claude Code (same as `ENABLED=false`): the force-delegate hooks pass through so the main
agent edits and runs tests locally instead of blocking on a dead worker, and a one-line notice
(`⚠️ worker healthcheck failed … falling back to local`) shows in the process stream. When the
worker comes back, the next message resumes offload — all automatic, nothing to toggle.

`WORKER_HEALTH_PATH` defaults to `/v1/models` (the universal, free liveness probe on any
OpenAI-compatible endpoint); point it at `/health` for a server that exposes one, or set it to `off`
to skip the pre-flight probe. Even with the probe off, a worker call that errors mid-turn falls back
to local automatically. Tune with `WORKER_HEALTH_TIMEOUT_MS` (default 2000) and
`WORKER_HEALTH_CACHE_SEC` (default 15, reuses a recent probe result across rapid turns).

### Worker context budget (anti-hallucination checkpoint)

A long implement or explore run can outgrow the worker model's context window — and an over-long
context is exactly when a local model starts to hallucinate (one giant unverified edit instead of
small, tested steps). rig watches the **real** context size (`usage.prompt_tokens` returned by every
chat turn) and, when it crosses `RIG_WORKER_CTX_LIMIT` (default **48000** tokens — sized for a 64k
local window; raise it for 128k/200k models), **checkpoints**: the conversation is reset and reseeded
with a rig-assembled digest — the task, the current `git diff` from disk (the work so far — nothing
is lost), the last test output, and the files already read. The worker is never asked to summarize
itself, so the reset removes the bloated context rather than distilling it through a confused model.
The worker stays a plain OpenAI-compatible endpoint; nothing special is required of it. The result's
`checkpoints` field reports how many times this fired.

## Install & use

```sh
npx rig-move-llm                   # one command → the setup wizard (installs itself on confirm)
claude                             # plain Claude Code — auto-delegates to the worker, no flags
```

`npx rig-move-llm` (or `rig-move-llm setup` if already installed) runs an **interactive wizard**: it
asks the **scope** (`global` = every project, follows you; or `project` = this dir), then the
**worker endpoint** — which you can **skip** by pressing Enter (rig installs but stays *off*, so
Claude Code runs exactly as normal; turn it on later). It auto-detects a local Ollama/llama.cpp and
offers it as the default. No config file to hand-edit. Because the hooks call `rig-move-llm`, the
wizard offers to `npm install -g` itself so it stays on your PATH — so the single `npx` command sets
up everything.

The wizard wires Claude Code so a **plain `claude`** (no flags, no wrapper) offloads to the worker:
the `mcp__worker__implement` tool, the force-delegate + gate hooks, and a terse
plan→delegate→review output style (`.claude/output-styles/rig-delegate.md`) that keeps the paid
agent's output small, plus `.rig-move-llm/config.env`.

- **Global (follows you):** registers the worker at **user scope** in `~/.claude.json` and installs
  global hooks + output style + a `SessionStart` hook — so **every** project offloads with no
  per-project setup. On first session in a project the `SessionStart` hook lazily creates a
  `.rig-move-llm/` there — the way Serena creates `.serena/`. It **inherits** every setting from the
  global config (nothing is copied), so a later change to `~/.rig-move-llm/config.env` (endpoint,
  model, `ENABLED` on/off) propagates to all projects. Add a `KEY=value` line in a project's
  `.rig-move-llm/config.env` only to override one setting there (e.g. `ENABLED=false` to turn the
  hybrid off in that project alone).
- **Project:** wires only this directory (a project-root `.mcp.json`, pre-approved by
  `enableAllProjectMcpServers` so headless `-p` never hangs on the trust prompt).

**On / off switch.** `ENABLED` in `config.env` is the master toggle: `false` (the default when you
skip the worker) means the hook passes every tool through and Claude Code behaves normally; set a
worker endpoint and `ENABLED=true` to activate the offload — no re-install needed. Flip it from the
CLI without touching the hidden dir: `rig-move-llm enable` / `rig-move-llm disable` (add `--local` to
scope the flip to this project only). `rig-move-llm config` prints the effective configuration — which
scope wins, the resolved worker endpoint, and the on/off state — and `rig-move-llm config --open`
opens the target scope's `config.env` in your `$EDITOR`. `rig-move-llm run
-- claude` remains available when you also want the proxy's observability on the main leg. Reverse
everything with `rig-move-llm uninstall` (restores your `settings.json` verbatim; strips the
user-scope worker registration). `rig-move-llm init [--global] [--npx] [flags]` is the
non-interactive form for scripts (`--npx` spawns the worker via `npx -y rig-move-llm worker`, no
global binary needed for that leg).

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
rig-move-llm  (no args) | setup             guided setup wizard (scope + worker + wiring)
                                             (arrow/space-select TUI on a terminal;
                                              numbered line prompts when piped/headless)
rig-move-llm enable  [--local]               turn offload ON  (flip ENABLED in config.env)
rig-move-llm disable [--local]               turn offload OFF (Claude Code runs normally)
rig-move-llm config  [--local] [--open]      show the effective config / open it in $EDITOR
rig-move-llm serve [--port N] [--status]     run the routing proxy / report state
rig-move-llm hook  pre-tool|post-tool|session-start  Claude Code hooks (force-delegate + gate + auto-materialize)
rig-move-llm init  [--global] [--npx] [--service] [flags]  non-interactive bootstrap
                                             (--service: OS-supervised, survives reboots)
rig-move-llm uninstall [--global] [--purge]  reverse init for a scope (incl. OS service)
rig-move-llm run   [--] <command...>         launch a command with the proxy wired in
rig-move-llm stats [--reset|--history]       token accounting / savings
```

### Agent teams (experimental)

Claude Code's experimental agent teams work under rig with no extra setup. In the
default **in-process** backend each teammate shares the lead's process and its tool
calls already carry an `agent_id`, so the force-delegate hook treats teammates as the
workers (allows their tools) while the lead stays plan/delegate/review-only.

The **terminal backends** (`--teammate-mode tmux|iterm2`) spawn each teammate as a
separate `claude` process whose hook payloads have no `agent_id` — they would
otherwise be mistaken for the paid lead and denied every tool. `rig-move-llm run`
points `CLAUDE_CODE_TEAMMATE_COMMAND` at itself so it can stamp the teammate's
identity (`RIG_AGENT_ID`, which the hook honors like `agent_id`) and, by default,
pin the teammate to a cheaper model tier (`--model haiku`). Set
`RIG_TEAMMATE_MODEL=inherit` to keep the model the lead requested, or
`RIG_TEAMMATE_MODEL=<name>` to force a specific one. A launcher you set yourself in
`CLAUDE_CODE_TEAMMATE_COMMAND` is never overwritten.

Note the scope honestly: model-pinning selects a cheaper **Anthropic** tier, it does
not route teammate inference to your worker endpoint — off-quota offload goes through
`mcp__worker__implement`, not the team path. Teams are interactive-only (headless `-p`
has no teams), so this path is not exercised by rig's CI smokes; treat it as
experimental.

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

MIT — see [LICENSE](LICENSE).
