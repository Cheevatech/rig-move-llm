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

### Worker engine: loop (default) or cc (experimental)

The worker MCP's `implement` tool has two interchangeable engines; MAIN sees the same result
shape either way.

- **`loop`** (default) — the built-in 3-tool loop (read / edit / run) driving your OpenAI-compatible
  endpoint. No extra dependencies.
- **`cc`** (experimental) — the worker runs as a native `claude -p` subprocess whose inference is
  pointed at **your** endpoint, so it brings the full Claude Code harness (investigate, edit, test,
  self-correct) to one delegation instead of bouncing rounds back to the main agent. Requires the
  `claude` CLI on PATH and an **Anthropic-format** endpoint for the worker model.

```sh
RIG_WORKER_ENGINE=                   # auto (default): cc when RIG_CC_BASE_URL is set, loop otherwise | cc | loop
RIG_CC_BASE_URL=http://127.0.0.1:4000/r/worker  # REQUIRED for cc: Anthropic-format endpoint (rig serves one — see below)
RIG_CC_MODEL=haiku                   # model name the subprocess runs as (default haiku)
```

**Engine selection is automatic** (v0.6.1, after the cc engine passed its evidence gate on
catch-rate, a fresh repo, and a second endpoint): configuring `RIG_CC_BASE_URL` is the opt-in —
the cc engine runs whenever it is set. An install without it keeps the loop, and
`RIG_WORKER_ENGINE=loop` forces the loop even with a base URL configured.

**Where do you get an Anthropic-format endpoint?** You already have one: the rig proxy itself.
`rig-move-llm serve` translates Anthropic `/v1/messages` (streaming included) to your
OpenAI-compatible `WORKER_API_BASE` / `WORKER_MODEL` on its **`/r/worker`** route, so the cc
engine runs entirely in-product — no extra translator to install:

```sh
rig-move-llm serve --port 4000                    # the proxy the wizard already runs
RIG_CC_BASE_URL=http://127.0.0.1:4000/r/worker    # cc subprocess -> rig -> your OpenAI endpoint
```

The subprocess traffic lands in the worker ledger like any other worker call. Any other
Anthropic-format endpoint (e.g. an external LiteLLM translation layer, or a provider that
speaks the Anthropic API natively) works the same way.

`RIG_CC_BASE_URL` is a hard requirement, not a default: with it empty the engine **refuses to
launch**, because the subprocess would otherwise bill its inference to your paid Anthropic account —
the account this tool exists to protect. The subprocess authenticates with a dummy key (your
OAuth/keychain identity is never exposed to the worker leg) and runs with web tools disabled. If your
`claude` binary is too old to speak `--output-format stream-json`, the run fails with an explicit
version-skew error rather than an empty result. The setup wizard offers this choice under
"Worker engine"; the default stays `loop`.

**Known limitation — a green gate is not proof of correctness.** On an unselected
SWE-bench_Lite sample, the delegated worker sometimes produced fixes that removed the reported
symptom and kept every existing test green, yet still failed the issue's hidden acceptance
tests — and the orchestrator signed off on the worker's "gate passed" report. The savings
claim holds when the task's expected behaviour is stated precisely enough that the worker's
own reproduction doubles as an acceptance test; when a bug report only describes a symptom,
treat the worker's success report as unverified until a test you own covers the fix.

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
  `enableAllProjectMcpServers` so the worker server loads without a prompt — see
  [Headless](#headless-claude--p) for the third trust layer a never-opened project still needs).

**On / off switch.** `ENABLED` in `config.env` is the master toggle: `false` (the default when you
skip the worker) means the hook passes every tool through and Claude Code behaves normally; set a
worker endpoint and `ENABLED=true` to activate the offload — no re-install needed. Flip it from the
CLI without touching the hidden dir: `rig-move-llm enable` / `rig-move-llm disable` (add `--local` to
scope the flip to this project only). `rig-move-llm config` prints the effective configuration — which
scope wins, the resolved worker endpoint, and the on/off state — and `rig-move-llm config --open`
opens the target scope's `config.env` in your `$EDITOR`. `rig-move-llm run
-- claude` remains available when you also want the proxy's observability on the main leg. Reverse
everything with `rig-move-llm uninstall` (restores your `settings.json` verbatim; strips the
user-scope worker registration; reverts a workspace-trust grant rig made).
`rig-move-llm init [--global] [--npx] [flags]` is the
non-interactive form for scripts (`--npx` spawns the worker via `npx -y rig-move-llm worker`, no
global binary needed for that leg).

The npm package ships a single prebuilt static binary per platform via
`optionalDependencies` (the esbuild/biome pattern — no postinstall download).

### Headless (`claude -p`)

An unattended run has **three** separate gates in Claude Code, and rig's wiring covers them
in two steps:

| layer | what it gates | how it is granted |
|---|---|---|
| server trust | may the worker MCP server load at all | `enableAllProjectMcpServers`, written by `init` |
| tool permission | may the agent call `mcp__worker__implement` without asking | `permissions.allow`, written by `init` |
| **workspace trust** | is this directory trusted at all | the trust dialog — **or** `init --trust-workspace` |

The third one is the trap: on a project that has never been opened interactively, Claude Code
**discards the `permissions.allow` entry entirely** (`Ignoring 1 permissions.allow entry … this
workspace has not been trusted`), so a headless run stops to ask a human and the run is wasted.
`init` prints a notice when it sees this, with both ways out:

```sh
claude                                   # once, interactively — accept the trust dialog
rig-move-llm init --trust-workspace      # or grant it directly (RIG_INIT_TRUST=1 for scripts)
```

`--trust-workspace` sets `projects["<this dir>"].hasTrustDialogAccepted` in your `~/.claude.json` —
the same flag the dialog sets. It is **never** implied by a plain `init`: that dialog is Claude
Code's safeguard against a repo you have not looked at yet, so switching it off stays an explicit
act. The wizard asks the same question interactively, and `rig-move-llm uninstall` reverts a grant
**rig** made — never one you accepted yourself.

### Guards: what bounds one delegation

A delegated round is bounded on both axes, so a worker that stops making progress fails loudly
instead of hanging or looping:

| knob | default | what it does |
|---|---|---|
| `RIG_CC_STALL_TIMEOUT` | `600s` | kills a cc-engine round whose stream has gone completely silent (a live worker emits an event per tool call; the longest honest silence is a Bash call, which Claude Code caps around 10 minutes). `0` disables |
| `RIG_WORKER_RUN_TIMEOUT` | `3000s` | the wall for one `implement` call. It must stay **below** the per-call `timeout` rig writes into the worker entry of `.mcp.json` (this value + 5 min), or the round dies without rig getting to explain why. That written timeout also raises Claude Code's stdio **idle** floor — without it a worker that answers only at the end of a long round is aborted after 30 min of "idleness" while it is still working (#33). Raising this wall means re-running `rig-move-llm init` so the `.mcp.json` timeout moves with it |
| `RIG_MAX_DELEGATE_ROUNDS` | `3` | how many times the main agent may delegate within **one turn**. `0` disables |

A killed round returns `stopped: "timeout"` with what the worker was last seen doing and any
partial diff, and the output style tells the main agent not to re-send the same spec. When the
round budget is spent the hook denies the call and the agent reports to you instead — your next
message resets the budget, so "just keep going" is a reply, not a flag.

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
rig-move-llm init  [--global] [--npx] [--service] [--trust-workspace] [flags]
                                             non-interactive bootstrap
                                             (--service: OS-supervised, survives reboots;
                                              --trust-workspace: accept CC's workspace trust here)
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
