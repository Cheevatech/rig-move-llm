# rig-move-llm

**Move the heavy lifting off your paid LLM.** A local proxy that lets Claude Code keep planning on your paid subscription while every code change, file edit, test run, and knowledge lookup is delegated to a **worker model of your choice** — your own local model (llama.cpp / Ollama / ExLlama) or any API endpoint (OpenRouter, …). The worker's output is verified by a deterministic gate before it ever reaches the paid agent, so you save tokens *without* trading away correctness.

> Status: **early / pre-release.** Proxy, translation, hooks, installer, and the reboot-safe daemon are working and validated end-to-end. The savings numbers below come from a small pilot (n=7) — treat them as evidence the mechanism works, then measure on your own workload with `rig-move-llm stats`.

## Why

On a subscription, the only lever is **paid-agent output tokens**. `rig-move-llm`:

- **Routes** subagent (worker) inference to *your* endpoint; the main agent stays on Anthropic direct.
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
- **Fallback and %-budget can burn real quota.** A `passthrough` endpoint in
  `workers.json` (and any tier you explicitly route to `passthrough`) sends
  worker-tier requests to the paid upstream while your workers are down, and
  `CUSTOM_SUBAGENT_USAGE` below 100 deliberately diverts a share of worker-tier
  traffic there for quality. Every such request is logged as `routed: "diverted"`
  so your savings numbers stay truthful. Default config never does either.
- **Variance disclosure.** Worker patches vary run-to-run (we observed two equivalent
  patches differing by one line with different hidden-test outcomes). With arbitrary
  user endpoints the variance is larger. We therefore claim the *cost floor* and
  *quality parity mechanism*, not absolute resolve rates — **measure on your own
  workload** (`rig-move-llm stats --history`).

## Architecture (one binary)

```
Claude Code ──> rig-move-llm ──┬─ main agent   ─> api.anthropic.com   (raw passthrough, OAuth untouched)
              (ANTHROPIC_BASE_URL)  └─ worker (subagent) ─> your endpoint  (Anthropic <-> OpenAI translation, streaming)
```

The daemon replaces what previously needed a Node shim **and** Python LiteLLM — a single static Go binary, stdlib-only, cross-compiles to macOS / Linux / Windows (amd64 + arm64) with zero toolchain.

## Configuration (bring-your-own endpoint)

```sh
WORKER_API_BASE=http://localhost:11434/v1        # Ollama / llama.cpp / ExLlama(TabbyAPI) — local or over Tailscale
WORKER_MODEL=qwen2.5-coder:32b
WORKER_API_KEY=...                               # or an OpenRouter key: https://openrouter.ai/api/v1
MAIN_UPSTREAM_URL=https://api.anthropic.com
PORT=4000
```

Install **local** (this project only) or **global** (all projects) via the `init` bootstrap.

### Fallback chain (workers.json)

For more than one worker endpoint, add a `workers.json` next to `config.env`
(local or global scope; local replaces global wholesale). It overrides the
`WORKER_*` values with a priority chain — on connection failure, header timeout,
408/429 or 5xx the next endpoint is tried, and a failed endpoint is skipped for
30 s (health gate) so a down worker costs one timeout, not one per request:

```json
{
  "endpoints": [
    { "name": "local",  "backend": "ollama", "model": "qwen2.5-coder:32b", "priority": 1 },
    { "name": "cloud",  "backend": "openrouter", "model": "qwen/qwen-2.5-coder-32b-instruct",
      "key": "sk-or-...", "priority": 2 },
    { "passthrough": true, "priority": 9 }
  ]
}
```

A `passthrough` entry is an honest last resort: it sends worker-tier requests to
the paid Anthropic upstream while your workers are down (logged as
`routed: "diverted"` in the stats, so savings numbers stay truthful). The file
can hold API keys — create it yourself with `chmod 0600`; it is never
auto-created.

### %-budget alternation (CUSTOM_SUBAGENT_USAGE)

`CUSTOM_SUBAGENT_USAGE=N` (1-99) targets a token mix instead of an all-or-nothing
route: over a sliding 15-minute window, when your worker's share of worker-tier
tokens (input+output) is at or above N%, the next worker-tier request is diverted
to the paid Anthropic upstream — trading quota for frontier-model quality on a
slice of the delegated work. **This mode deliberately burns quota.** Every
diverted request is logged as `routed: "diverted"` with `endpoint: "budget"`, so
`stats --history` reproduces exactly what you paid for. An empty window routes to
your worker (never burns quota on missing data). The default (unset or `100`)
never diverts and is byte-identical to not having the feature.

## Install & use

```sh
npx rig-move-llm@latest init      # auto-detects a local Ollama/llama.cpp; wires this project
npx rig-move-llm run -- claude     # launch Claude Code with the proxy in place
```

`init` writes `.rig-move-llm/config.env` and wires Claude Code (hooks + a `rig-worker`
subagent + optional knowledge/search MCP), probing `localhost:11434` (Ollama) and `:8080`
(llama.cpp) so config is near-zero. Add `--global` to install for every project
(`~/.claude` + `~/.rig-move-llm`); local overrides global. `run` sets `ANTHROPIC_BASE_URL`
for that process only (so local scope never leaks) and starts the proxy if it isn't up.
Reverse everything with `rig-move-llm uninstall` (restores your `settings.json` verbatim).

The npm package ships a single prebuilt static binary per platform via
`optionalDependencies` (the esbuild/biome pattern — no postinstall download).

### Permissions posture (headless)

Claude Code's auto-mode runs a model-based safety classifier before auto-approving
Bash commands. That classifier call rides the same rerouted tier as the worker — so
if your worker is down and you have no fallback chain, auto-mode can stall waiting
for it. Two supported answers:

- **Fallback chain** (`workers.json` above) — the classifier always has an endpoint
  to answer from; a `passthrough` entry guarantees it even with all workers down.
- **Headless allowlist/bypass permissions** — in a headless hybrid run the main agent
  is already structurally denied mutating tools by the rig hook, and the worker runs
  on *your* machine against *your* endpoint, so the model classifier is a redundant
  layer there. If you turn it off, understand what that means: you are trusting the
  hook + your own sandboxing instead of a second model opinion. Do this only for
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

## Layout

```
cmd/rig-move-llm/   entrypoint
internal/cli/       subcommand dispatch (serve/hook/init/run/stats)
internal/service/   OS supervision (launchd / systemd --user / Task Scheduler), stdlib-only
internal/proxy/     the routing core (main-leg passthrough + worker-leg translation)
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
