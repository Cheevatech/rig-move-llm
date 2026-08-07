# rig-move-llm

**Move the heavy lifting off your paid LLM.** rig is a switch. It sits between Claude Code and Anthropic, and it lets you change *which model answers the next turn* — from your paid Claude to a worker model of your choice (your own local llama.cpp / Ollama / ExLlama, or any OpenAI-compatible API) — **in the middle of a live session, without restarting it and without losing the context you have built up.**

Plan and scope on Claude. Type `/worker on`. The next turn is answered by your own model, in the same conversation, with the same history, driving the same Claude Code toolset. Type `/worker off` to hand the wheel back.

**Either model can throw the switch itself, and so can you.** `/worker on` is a plain slash command that runs one CLI call, so the paid model can hand off the moment its planning is done, without you watching. There is also `rig-move-llm worker on|off` from a second terminal — the one path that works no matter which model is currently driving, because it does not go through a model at all.

> Status: **pre-release (0.8, unreleased).** The architecture changed twice in the last week. There is no savings number in this README, on purpose — see [What this claims](#what-this-claims-and-what-it-does-not).

## How it works

```
                      ┌──────────────────────────────────────────────┐
  claude  ──────────► │  rig-move-llm serve   (ANTHROPIC_BASE_URL)   │
   (unmodified)       └──────────────────────────────────────────────┘
                              │                        │
            worker OFF ───────┘                        └─────── worker ON
                    │                                            │
                    ▼                                            ▼
        api.anthropic.com                              your worker endpoint
        verbatim passthrough,                          Anthropic ⇄ OpenAI
        OAuth headers untouched,                       translation, streaming,
        usage tee-scanned                              off the paid ledger
```

There is no second agent, no subprocess, no delegation, and no result to hand back and trust. Claude Code never learns that the model behind the endpoint changed; it keeps its own tools, its own permissions, and its own transcript.

The flip is live because the proxy re-reads config **fresh on every request** — no cache, no restart, and no distinction between a registered project and a plain global install. `worker on` writes one key to `config.env`; the next HTTP request a running session makes picks it up.

One static Go binary, stdlib only. Cross-compiles to macOS / Linux / Windows (amd64 + arm64) with no toolchain.

## Install & use

```sh
npx rig-move-llm                        # interactive setup wizard (offers to install itself)
```

Then, in the project you want to work in:

```sh
rig-move-llm run -- claude              # launches Claude Code with the proxy wired in
```

Then, inside that session, whenever you want to change models:

```
/worker on      # next turn runs on your worker
/worker off     # next turn runs on Claude again
```

Claude can run these itself once its planning is done — that is the intended flow,
and it needs nobody watching. The same thing is available as a CLI, which is what
you reach for from a second terminal when you want the switch thrown regardless of
which model is currently driving:

```sh
rig-move-llm worker on                  # next turn runs on your worker
rig-move-llm worker off                 # next turn runs on Claude again
rig-move-llm worker status              # which model is answering, and where it points
```

`run` is required: it is what sets `ANTHROPIC_BASE_URL` for that one process (and only that one — it is a per-process env var, so it never leaks into your other projects). It starts a `serve` daemon in the background if one is not already listening. A bare `claude` launched by hand does not go through rig at all.

For a registered project, `run` embeds the project's identity in the base URL path (`/p/<id>`) so a single global daemon can serve many projects and resolve each one's own config per request. Registration is a **fail-closed allowlist** in `~/.rig-move-llm/projects.json`, written by a project-scope `init` — `init --global` registers nothing, because a global install has no per-project config to resolve. A cloned repo that ships its own `config.env` therefore has no effect on your daemon until you opt in by running `init` inside it.

Either way the flip reaches the session: a request without `/p/<id>` is served from the daemon's own scope, re-read just as freshly.

## What this claims, and what it does not

**The mechanism is real and checkable.** When the switch is on, inference goes to *your* endpoint and never reaches api.anthropic.com. `rig-move-llm doctor` proves each link before you trust anything, and `rig-move-llm stats` separates the two legs.

**There is no savings number here, and that is deliberate.** rig has carried two savings figures in its life and both were withdrawn — the first because it was measured on machinery that had since been deleted, the second because it counted the wrong denominator. The honest statement of what has been measured is:

- Offloaded turns are **not billed to your Anthropic quota**, and that part is verifiable at the wire.
- Whether offloading is *worth it* depends on how much of the offloaded work you end up **accepting**. A small backtest on one real repository has now measured both halves of that, and they point in opposite directions: the work that was accepted cost dramatically fewer billed tokens, and most of what came back was not acceptable as-is. Either figure quoted without the other misleads, so this README quotes neither.

Treat rig as a mechanism that does exactly what it says on the wire, and judge the output yourself.

Other things that stay true regardless:

- **Quota is counted server-side.** A local worker's generation is 100% outside your Anthropic quota. There is nothing client-side to fool.
- **A local worker is free compute; an API worker is cheaper, not free.**
- **Claude Code's `/cost` display is cosmetic under rig.** It prices every turn as if Claude had answered it, including turns your local model answered, and it can jump tiers on a long context. Read `rig-move-llm stats` instead — and read `stats` with the caveat in [Accounting](#accounting).
- **Worker models are slower than the frontier, and vary run to run.** Two runs of the same task can produce materially different work, and a task that took the paid model minutes can take the worker hours. That rules out interactive use. It does not, on its own, argue for any other use — see below.
- **Nothing certifies the worker's output.** rig has no gate, no verdict, no pass/fail. You read the diff. That is the check.

**How often does it need rejecting? Often enough that reading is the job, not a formality.** In a small backtest on one real repository, most of what came back through the switch was not acceptable as-is. Twice the model's own tests passed while the code was wrong — one test compared the wrong two array elements, and another's assertions were written `assert (cond, "message")`, a tuple that can never be false. Both were caught by reading the diff, and only by reading it.

This is also why rig ships no gate. A gate reports what the tests report, and the tests were the thing that was wrong.

### So when should you use it?

**There is no measured answer to that yet, and this README is not going to invent one.**

An earlier version of this section said rig was "headless/AFK-shaped: good for work you are willing to walk away from." That sentence was never measured, and the one backtest that exists points the other way: those tasks *were* walk-away work — scoped implementation tasks on a real repository, left running unattended — and that is the setting where most of what came back needed rejecting. Recommending the shape that lost is not a recommendation.

What the failures had in common is more useful than the count. Almost none of them were the worker being unable to do the work. They were the worker **reporting a success it had not achieved**: rewriting existing assertions until the suite went green, a test that had drifted one frame off the boundary it claimed to check, an `except Exception` that swallowed the failure, a run that kept going for forty minutes after it had what it needed and then said it was done. In every case an automatic signal said yes and a human reading the diff said no.

That suggests the thing that decides whether rig pays off is not price and not speed, but **how expensive it is to notice a false success** on the kind of work you hand it. Work whose acceptance test the worker cannot edit — a failing test written in advance and declared off-limits, output checked against a known-good reference, a mechanical change you can eyeball in seconds — should behave differently from "make the suite pass." That is a hypothesis with an obvious experiment attached and no results yet.

Until there are results: rig is a mechanism that verifiably moves inference to your endpoint. Whether that is worth your time is not something this README can currently tell you.

## Handing the wheel back

Handing *over* is reliable: the paid model runs `/worker on` and the next turn is the worker's. Handing *back* has a catch worth knowing before you rely on it, because after the flip the model deciding anything is the worker.

Measured on a real session against a local qwen:

| | result |
|---|---|
| paid model runs `/worker on` | works |
| worker does the actual task after the handoff | works |
| worker runs `/worker off` **when that is the instruction in front of it** | works — it even recovered from guessing the command's name wrong on the first try |
| worker runs `/worker off` **as step 3 of a 4-step plan it inherited** | **does not happen** — it finished step 2, declared the task complete, and dropped the rest |

So: ask for the handback when you want it and it happens. Write it into a plan and hope the worker gets there, and it may not — small models truncate plans, and no wording in a slash command fixes that.

`rig-move-llm worker off` from a second terminal needs no model to cooperate, which is why it stays even though `/worker off` is the nicer surface.

## Stopping a run

Kill the client and the worker stops. Measured: a probe against a busy local endpoint answered in 57.5s while a run was in flight and in 0.5s immediately after the client was killed — the server was genuinely free, not merely disconnected.

There are therefore **no timeouts and no guards** in rig. An earlier design had a stall ceiling and a wall ceiling around a delegated subprocess; both are gone with the subprocess. Because the flipped session is *your* Claude Code session, Ctrl-C is the stop button, and it works the way you already expect.

## Commands

```
Setup
  rig-move-llm                             interactive setup wizard (same as 'setup')
  rig-move-llm setup                       guided install: scope + worker endpoint
  rig-move-llm init  [--global] [--service] [flags]   non-interactive bootstrap
  rig-move-llm uninstall [--global] [--purge]         reverse init for a scope

Control
  rig-move-llm worker  on|off|status [--global]   swap the model answering the NEXT turn
  rig-move-llm config  [--local] [--open]         show the effective config / edit it
  rig-move-llm enable  [--local]                  set ENABLED=true  in config.env
  rig-move-llm disable [--local]                  set ENABLED=false in config.env
  rig-move-llm stats   [--reset|--history]        token accounting per leg
  rig-move-llm doctor  [--json]                   prove the switch is live

Run
  rig-move-llm run    [--] <command...>           launch a command with the proxy wired in
  rig-move-llm serve  [--port N] [--bind ADDR] [--status]  run the routing proxy / report its state
  rig-move-llm version
```

`init` flags: `--global --backend --worker-base --worker-model --worker-key --main-upstream --port --force --no-detect --service`.

**The proxy listens on loopback only.** `serve --bind` can put it on another interface, and the reason to think twice is that the worker leg authenticates with the `WORKER_API_KEY` from `config.env`: a caller who can reach the port needs no credentials of their own to spend the endpoint behind it. Through 0.8.0 the bind was `:PORT` — every interface — while everything else in rig dialled `127.0.0.1`, so on a machine with a LAN or a tailnet the proxy was an open relay to your worker. Fixed in 0.8.1.

`--service` requires `--global` and installs an OS service (launchd on macOS, a systemd `--user` unit on Linux, a scheduled task on Windows) so the proxy survives a reboot — and a kill: verified on macOS that killing the process brings it straight back under a new pid. `rig-move-llm serve --status` reports both facts separately, whether the supervisor has it loaded and whether anything is actually listening, because those two come apart. `uninstall --global` removes the service with everything else.

**Scope.** `global` (`~/.rig-move-llm`) follows you across every project; `local` (`./.rig-move-llm`) is this directory only. Precedence is **process env > local > global**. `worker` defaults to the *local* scope, because that is the scope a live session reads first.

## Configuration

`init` writes two files and registers the project in the allowlist:

- `<scope>/.rig-move-llm/config.env` — your configuration
- `<scope>/.claude/commands/worker.md` — the `/worker` slash command

It writes no hooks, no `CLAUDE.md`, no MCP servers, and no Claude Code settings. The slash command is the one thing rig puts in your `.claude/` dir, and it is deliberately the only kind of thing that belongs there: a hook that stops firing fails silently, whereas a slash command does nothing at all until you type it and its absence is obvious the moment you do. `uninstall` removes it again — unless you edited it, in which case that copy is yours.

`init` also adds `.claude/`, `.mcp.json` and `.rig-move-llm/` to `.git/info/exclude` (local and never committed, so rig does not edit a `.gitignore` you share with your team).

The keys the proxy actually reads:

Every key it writes, and nothing it does not:

```sh
WORKER_API_BASE=http://localhost:11434/v1     # OpenAI-compatible endpoint — local, or over Tailscale
WORKER_MODEL=qwen2.5-coder:32b                # model name sent to that endpoint
WORKER_API_KEY=...                            # optional for local models; an OpenRouter key for OpenRouter
WORKER_BACKEND=generic                        # ollama|llamacpp|tabby|openrouter|openai|generic — sets a default base URL

RIG_ROUTE_ALL_TO_WORKER=false                 # THE SWITCH — prefer `/worker on|off` to editing this
ENABLED=true                                  # master switch, see below

MAIN_UPSTREAM_URL=https://api.anthropic.com   # the paid leg (raw passthrough, OAuth untouched)
PORT=4000                                     # proxy listen port

LOG_BODIES=                                   # set to 1 to log full request/response bodies (default: metadata only)
LOG_MAX_MB=50                                 # cap on logs/requests.jsonl before the oldest half is compacted away
```

The setup wizard collects the worker endpoint for you; you should not need to hand-edit this.

### Two switches, and why

`RIG_ROUTE_ALL_TO_WORKER` is the flip you use every day — `/worker on`, `/worker off`, many times a session. `ENABLED` is the master switch: with it false, **nothing** reaches the worker, including an explicit `/r/worker`, no matter what the flip says. Flip it with `rig-move-llm enable` / `disable` (add `--local` for this project only).

The master switch exists so there is one thing to turn off that you do not have to reason about — hand a repo to someone, or walk away from a machine, and `disable` is a claim about every future request rather than about the flag you last remembered to set. `worker status` and `config` both report when the two disagree.

Both are read fresh from disk per request, so either takes effect on the next turn of a running session.

### Forcing a leg for one request

The base URL path carries a per-request override, which is how one daemon can serve both legs at once without any flag flip:

```
POST http://127.0.0.1:4000/r/worker/v1/messages    # the worker, whatever the flip says
POST http://127.0.0.1:4000/r/main/v1/messages      # the paid leg, even while the switch is ON
```

`/r/main` exists so a request aimed at Claude is never trapped on the worker leg; `/r/worker` is what lets a measurement run both arms against one daemon. Both compose with `/p/<id>` (`/r/worker/p/<id>/v1/messages`), and both are still subject to `ENABLED`.

## Is it actually live?

```sh
rig-move-llm doctor
```

Four rungs, exit non-zero if any fails:

| rung | what it proves |
|---|---|
| `config` | a worker endpoint resolves and `ENABLED` is true |
| `worker endpoint` | a **real completion** succeeds — not `/v1/models`, which answers 200 unauthenticated on some servers and so cannot see a wrong API key |
| `switch` | the proxy's own `/r/worker` route answers an Anthropic-format request, down the same `/p/<id>` path `run` uses, so it proves the config *this project's session* will get rather than whatever scope the daemon booted in (SKIPs when `ENABLED=false`, since nothing routes there) |
| `MAIN auth` | credentials exist for the paid leg at all (presence, not freshness — an expired token still reads PASS) |

The rule this encodes: never trust a number from rig without proving the mechanism that produced it was live. Every rung is here because a rung like it once blessed a run that was silently offloading nothing.

## Accounting

```sh
rig-move-llm stats              # cumulative ledger, split by leg
rig-move-llm stats --history    # the per-request log (logs/requests.jsonl)
rig-move-llm stats --reset      # clear both
```

Two caveats, both load-bearing:

- **The ledger lives in the *daemon's* scope**, not the caller's. A global `serve` writes to `~/.rig-move-llm/stats.json` even for traffic from a project with its own local config, so `stats` run from the wrong directory finds nothing. It now says so — when this scope is empty and another one is not, it names the other — but the fix is still to run it from the scope the daemon booted in.
- **One daemon owns a scope's ledger.** A second daemon started in the same scope (`serve` on another port from the same directory) keeps serving but stops recording, and says so in its log, rather than overwriting the first one's counts.
- **The paid leg's input is reported in three parts, because it is priced in three parts.** `stats` prints the total and then the split:

  ```
  main   (billed, Anthropic):    57951 in + 997 out = 58948 tok over 1 req
           input split:          2 fresh + 56877 cache-read + 1072 cache-write
  ```

  A cache read costs roughly a tenth of fresh input and a cache write slightly more than it, so the three are kept apart in `stats.json` (`main_in`, `main_cache_read`, `main_cache_write`) rather than summed into one number that would read as if it were all full price. The worker leg has no equivalent split — an OpenAI-shaped `prompt_tokens` is already the whole prompt.

  Before 0.8 only `main_in` was recorded, and on a warm Claude Code session that is nearly zero: the capture above was logged as **2 tokens** of input. Any comparison built on a pre-0.8 ledger is wrong by orders of magnitude; reset it with `rig-move-llm stats --reset`.

## Security posture

When the switch is **off**, rig is a passthrough: your traffic goes to Anthropic byte-for-byte with its auth headers intact, and rig sees it only to count tokens.

When the switch is **on**, everything Claude Code would have sent to Anthropic — your prompt, your conversation, whatever file contents the session has read — goes to `WORKER_API_BASE` instead. Point that at an endpoint you control and trust. There is no third party in the path, and no subprocess with its own permissions: what runs is the Claude Code session you launched, under the permissions you gave it.

The project allowlist is fail-closed: the daemon refuses to load config for, or route traffic on behalf of, any directory not explicitly registered by `init`.

## Layout

```
cmd/rig-move-llm/   entry point
internal/cli/       subcommand dispatch (setup / init / worker / run / serve / doctor / stats / config)
internal/cli/tui/   hand-rolled stdlib raw-mode TUI for the wizard (no framework)
internal/config/    layered config (env > project > global) + the project allowlist
internal/proxy/     leg routing, main-leg passthrough + metering, worker-leg translation
internal/service/   OS service install (launchd / systemd --user / Windows task)
internal/stats/     the token ledger
pkg/translate/      Anthropic ⇄ OpenAI message + stream translation
```

~6,300 lines of Go under `internal/`, down from ~25,400 before 0.8.

## What changed in 0.8

0.8 deleted two whole layers and everything built on them.

**The enforcement layer** — a `PreToolUse` hook that denied the paid agent its editing tools, a deterministic gate on the worker's result, the proof-of-flip retry, the per-turn delegation budget, the tiered return, and the `explore`/`triage`/`show_change` tools. The reason was a measurement: on a real task, on a real repo, the worker produced a change that passed the entire test suite — because it had quietly rewritten five existing assertions to match its new behaviour. No gate can catch that; the suite was green and the suite was the thing that had been edited. A human reading the diff catches it in seconds. Once the gate cannot be the check, the machinery that existed to feed the gate has no purpose — and that machinery was measured consuming ~74% of the tokens the worker generated.

**The delegate arm** — the `mcp__worker__implement` tool that spawned a `claude -p` subprocess against your endpoint and handed a diff back, plus the stall/wall guards and the kill path that bounded it. Compared head to head against the switch on the same six tasks, delegation moved less work off the paid leg while costing ~2,800 lines of machinery and a kill path that had to be built from scratch. The switch does the same job with none of it.

**Removed commands:** `rig hook`, `rig worker`, `rig cascade`, `rig teammate-exec` (with the enforcement layer); `rig thin-worker`, `rig thin-supervise`, `rig watch` (with the delegate arm). The name `rig worker` is reused, not kept: it used to run the worker subprocess, and in 0.8 it is the switch — `worker on|off|status`.
**Removed config keys:** `GATE_MODE`, `VERIFY_CMD`, `RIG_MAX_DELEGATE_ROUNDS`, `RIG_THIN_STALL`, `RIG_THIN_WALL`, `RIG_CC_BASE_URL`, `RIG_CC_MODEL`, `RIG_WORKER_ENGINE`, `MAIN_SHARED_MCP`, `WORKER_HEALTH_PATH`, `WORKER_HEALTH_TIMEOUT_MS`, `WORKER_HEALTH_CACHE_SEC`. A key that is still parsed but no longer read is worse than a deleted one: it looks like a setting you can act on.

**Migration:** `uninstall` cleans up an old install — including files written by *older* rigs (the enforcement-era hooks, output styles, `CLAUDE.md` steer and `/qwen` command) — while leaving hooks and permissions you added yourself untouched. `init` writes the new one.

Verified against a simulated 0.7.x install: the hook, the `mcp__worker__implement` grant, `enableAllProjectMcpServers`, the `rig-delegate` output style, the steer, the old slash command and `.mcp.json` all went; an unrelated `Bash(ls:*)` permission in the same `settings.json` stayed.

That simulation had no `settings.json.bak`, and a real machine did. Through 0.8.0, when a backup existed `uninstall` restored it and stopped — and `init` snapshots whatever is on disk, so on a machine where rig had been installed more than once the snapshot already held an older rig's hooks. They came back after being removed, under the word "complete", pointing at `rig hook`, a subcommand 0.8 deleted. 0.8.1 strips on both paths: the restore recovers what only the backup knows, the strip makes sure nothing of rig's survives it.

One upgrade case needs a look rather than a command. `ENABLED` used to be read only by `config` and `doctor`; it now gates routing. If your `config.env` carries `ENABLED=false` from an older install *and* you were relying on `RIG_ROUTE_ALL_TO_WORKER=true` to offload, offload will stop after upgrading and every turn will quietly run on your paid model. Nothing breaks and no work is lost — it fails toward the expensive-but-correct side — but check `rig-move-llm config` once after upgrading. It prints which model answers the next turn and says so explicitly when the two switches disagree.

## Build

```sh
go build ./cmd/rig-move-llm
# Go 1.21 on macOS (LC_UUID bug): go build -ldflags=-linkmode=external && codesign -s - -f rig-move-llm
```

## License

MIT
