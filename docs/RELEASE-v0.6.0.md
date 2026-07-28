# v0.6.0 — release notes (draft)

Everything since v0.5.0 (20 commits on this branch over `main`).

## Highlights

- **cc worker engine (experimental, opt-in)** — `RIG_WORKER_ENGINE=cc` runs the worker as a
  native `claude -p` subprocess whose inference is pointed at your own endpoint
  (`RIG_CC_BASE_URL`, required — the engine refuses to launch without it so the worker leg can
  never bill the paid account). One delegation carries the full investigate → edit → test →
  self-correct loop instead of bouncing rounds back to the main agent. Offered in the setup
  wizard; the default engine stays the built-in 3-tool loop.
- **Version-skew guard** — a `claude` CLI that does not speak `--output-format stream-json`
  surfaces as an explicit diagnosis, never a silent empty result.
- **Cost-aware gate (map6)** — Gate A triage declaration, Gate B bounded repair window,
  iteration caps; `GATE_MODE=hard|soft`.
- **Worker context budget** — `RIG_WORKER_CTX_LIMIT` checkpoint: reset + rig-assembled digest
  when the worker's real context crosses the limit (anti-hallucination).
- **Stage-0 explore loop** — bounded evidence-gathering pass before implement.
- **Tiered return (experiment-only)** — `RIG_RETURN_TIERING=1` opt-in; the default reply is
  the plain result payload, byte-for-byte what the engine benchmarks were measured against.
- **Health-check auto-fallback** — zero-token pre-flight probe (`WORKER_HEALTH_PATH`) with
  automatic per-turn degradation to plain Claude Code when the worker endpoint is down
  (was in-tree before v0.5.0 but first released here).

## Release checklist (in order)

1. Merge PR #1 (`feat/p1-merge-b5-cc-engine` → `feat/tiered-return`), then
   `feat/tiered-return` → `main`. Both are fast-forward-clean today.
2. `git tag v0.6.0 && git push origin v0.6.0` — CI takes it from there:
   vet + test → cross-compile (darwin/linux/windows × amd64/arm64) → npm publish
   (version stamped from the tag by `npm/scripts/prepare-npm.mjs`).
3. Smoke E2E on a machine that is NOT the dev rig (P10 lesson: a global rig on the dev
   machine hook-denies its own Bash): `npx rig-move-llm@0.6.0` → setup wizard → one
   delegation round-trip. The Tailscale server is a candidate box.
4. cc engine stays **off by default** — flipping it is gated by map10 P4 (evidence gate,
   film checkpoint), not by this release.
