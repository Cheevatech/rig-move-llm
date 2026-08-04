// Package thin is the switch, and nothing else.
//
// map17 (S2) rebuilds the implement path NEXT TO internal/worker rather than
// carving the contract layer out of it. The destination is one sentence: spawn
// `claude -p` pointed at qwen, in this folder, with this task, under a ceiling,
// and hand the diff back to a human. Everything the old path grew around that
// core — tiered returns, proof-of-flip, round budgets, triage, drill — exists to
// serve a machine that had to accept work blind. In this map nobody does: film
// reads the diff.
//
// What the package therefore does NOT have, deliberately:
//
//   - no gate, no proof-of-flip, no round budget, no re-delegation;
//   - no field in the return that a machine has to believe. `status` describes
//     the SUBPROCESS (did it exit on its own, or did we kill it, and why) — an
//     OS fact, not a claim by qwen.
//
// What it does have that the old path could not: a kill path that works. G1
// proved every client-side abort failed — cancelled, SIGTERM, SIGKILL and stdin
// EOF all left qwen burning for up to ~70 minutes — because Serve() read stdin
// synchronously while a tool call ran, so the cancel notification slept in a pipe
// buffer until the work it was cancelling had finished. Here the read loop never
// blocks on work, cancellation is wired to the request id, and a supervisor
// process watches its own parent so that even a SIGKILLed server takes the tree
// with it (see supervise.go).
package thin

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// wallCeiling bounds one implement call end to end. The old ladder
// (stall < wall < gate credit < client) existed because a round could be
// refunded, retried and re-gated; with one round and a human reader the only
// question left is "how long may qwen burn unattended". The number is film's
// policy, not a caller's parameter (S1): it lives here and in the env, never in
// the tool schema.
func wallCeiling() time.Duration {
	return time.Duration(envInt("RIG_THIN_WALL", 3000)) * time.Second
}

// WallCeiling and StallCeiling expose the guards for reporting (doctor). They
// are accessors, not knobs: the values come from the same env the run itself
// reads, so what doctor prints is what a run will actually get.
func WallCeiling() time.Duration  { return wallCeiling() }
func StallCeiling() time.Duration { return stallCeiling() }

// ClientCallTimeout is the wall-clock limit rig asks the CALLING client to apply
// to one implement call — written into the generated .mcp.json as this server's
// `timeout`. It sits above the wall ceiling so the run always kills itself first
// and the caller gets a diagnosis and the partial diff, never a bare client-side
// abort. It does double duty on Claude Code v2.1.203+: a per-server timeout is
// also a floor on the IDLE timeout, and this server answers once, at the end, so
// it is idle for the whole run by construction.
func ClientCallTimeout() time.Duration { return wallCeiling() + 5*time.Minute }

// stallCeiling bounds SILENCE. A live worker emits a stream event per tool call,
// so silence is a real liveness signal — with one honest long form: a Bash tool
// call emits nothing until the command returns, and Claude Code lets a command
// run about ten minutes. 600s sits just past the longest honest silence.
func stallCeiling() time.Duration {
	return time.Duration(envInt("RIG_THIN_STALL", 600)) * time.Second
}

// inlineDiffLimit is the byte threshold between "paste the diff" and "paste the
// stat and point at the file". S1 left the number open until a real task
// produced a real diff; 24 KiB is the starting point, and it is an env knob
// precisely because the number is expected to move.
//
// This is not the tiered return coming back: nothing is synthesized here. Both
// branches carry the same bytes, and the pointer branch points at a file that a
// plain Read opens. No extra tool exists to drill it.
func inlineDiffLimit() int { return envInt("RIG_THIN_DIFF_INLINE_BYTES", 24*1024) }

// deniedTools is the inventory subtraction that keeps qwen from handing the work
// on to someone else. A1 measured 10–23% of rounds spawning a subagent instead
// of editing: the worker had inherited MAIN's output style, whose text says "Do
// not edit files, run tests, or run shell commands yourself" — the exact opposite
// of its job — and it had Task/Workflow/Skill/Cron* available to comply with it.
// The style is fixed with --settings (see run.go); the escape routes are removed
// here, and the result is VERIFIED against the init event rather than assumed
// (checkInventory).
//
// WebFetch/WebSearch stay denied: the worker leg has no business on the network.
var deniedTools = []string{
	"Task", "Workflow", "Skill", "SlashCommand",
	"TaskCreate", "TaskGet", "TaskList", "TaskOutput", "TaskStop", "TaskUpdate",
	"CronCreate", "CronDelete", "CronList",
	"SendMessage", "ScheduleWakeup", "PushNotification", "Monitor",
	"DesignSync", "EnterWorktree", "ExitWorktree", "ReportFindings",
	"WebFetch", "WebSearch",
}

// disallowedTools is the --disallowedTools value. RIG_THIN_DISALLOWED overrides
// it wholesale (empty string = deny nothing), which is a posture change: a run
// made with it is not comparable to one made without.
func disallowedTools() []string {
	if v, ok := os.LookupEnv("RIG_THIN_DISALLOWED"); ok {
		v = strings.TrimSpace(v)
		if v == "" {
			return nil
		}
		return strings.Split(v, ",")
	}
	return deniedTools
}

// systemPrompt is appended to the worker's own. It says what the job is and
// stops there: no gate to satisfy, no evidence to produce, no verdict to
// declare. Anything more would be the contract layer growing back one sentence
// at a time.
const systemPrompt = `You are working directly on a repository checkout.
Do the task by reading and editing the files yourself, and verify your change with the repo's own test or repro command.
Do this work yourself in this session. Do not hand it to a subagent, a task, or another tool.
When you are done, say in one or two lines what you changed and what you ran to check it.`

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
