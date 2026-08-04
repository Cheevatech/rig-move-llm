package thin

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Outcome is everything one run produced. It is a plain record on the way to
// text: nothing here is a verdict, a tier, or a score.
type Outcome struct {
	// Status describes the SUBPROCESS and only the subprocess:
	// "finished" (it exited on its own), "killed(<why>)" (we killed it, and why),
	// "error: <what>" (it never ran). All three are facts the OS gave us.
	Status string

	LogDir      string
	Diff        string
	DiffStat    string
	DiffPath    string // full diff on disk; always written, cited when Diff is not inlined
	LastCommand string // the last shell command the worker ran, with the tail of its output
}

const (
	statusFinished = "finished"
)

// Run spawns one `claude -p` against the local worker model, in repo, for task.
// It returns when the subprocess is gone — because it finished, because ctx was
// cancelled, or because a guard killed it — and it returns the diff in every one
// of those cases.
//
// That last part is a requirement, not a nicety (G3): A1 recorded a round that
// walked 48 minutes, produced ~75,900 tokens, was cut by the wall, and threw all
// of it away. Work that reached the working tree belongs to the human who asked
// for it, whatever happened to the process that wrote it.
func Run(ctx context.Context, repo, task string) Outcome {
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return Outcome{Status: "error: resolve repo path: " + err.Error()}
	}

	logDir, err := newRunDir()
	if err != nil {
		return Outcome{Status: "error: create run directory: " + err.Error()}
	}
	out := Outcome{LogDir: logDir}

	// The action log opens BEFORE the checks below, not after the process starts.
	// The return quotes this directory's path, so there has to be something at it
	// explaining what happened — including, and especially, when the run never got
	// as far as a subprocess. Measured: a run refused for a missing base URL left
	// an empty directory, and `rig watch` (the obvious next thing a human does)
	// answered with a filesystem error instead of the reason.
	actions := newActionLog(logDir, absRepo, task)
	defer actions.close()
	// fail records the outcome in the log as well as in the return, so the two
	// never disagree and no exit path can leave the directory silent.
	fail := func(status string) Outcome {
		out.Status = status
		actions.finish(status)
		return out
	}

	// Money safety, kept verbatim from the old path because the reasoning did not
	// change: with no local base URL the subprocess would bill Anthropic from the
	// leg that exists to avoid exactly that.
	base := strings.TrimSpace(os.Getenv("RIG_CC_BASE_URL"))
	if base == "" {
		return fail("error: RIG_CC_BASE_URL is empty — refusing to launch the worker, " +
			"its inference would bill Anthropic instead of the local model")
	}
	bin := strings.TrimSpace(os.Getenv("RIG_CC_BIN"))
	if bin == "" {
		bin = "claude"
	}

	argv := workerArgv(bin, task)
	if sup := supervisorArgv(os.Getpid()); sup != nil {
		argv = append(sup, argv...)
	}
	writeFile(filepath.Join(logDir, "command.txt"), strings.Join(argv, "\n")+"\n")
	writeFile(filepath.Join(logDir, "task.txt"), task+"\n")

	runCtx, cancelRun := context.WithTimeout(ctx, wallCeiling())
	defer cancelRun()

	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = absRepo
	cmd.Env = childEnv(base)
	setProcGroup(cmd)
	cmd.Cancel = func() error { return killProcGroup(cmd) }
	cmd.WaitDelay = 2 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail("error: stdout pipe: " + err.Error())
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return fail("error: launch " + bin + ": " + err.Error())
	}

	live := &liveness{last: time.Now()}
	stopStall := watchStall(cmd, live)
	obs := parseStream(stdout, logDir, live, actions)
	stopStall()
	waitErr := cmd.Wait()

	out.LastCommand = obs.lastCommand
	out.Status = describeExit(ctx, runCtx, live, waitErr, obs, &stderr)
	// The closing line doubles as the signal that lets `rig watch` stop waiting
	// instead of hanging on a file nobody will append to again.
	actions.finish(out.Status)

	// Deliberately NOT runCtx: that context is cancelled precisely in the cases
	// where reading the tree matters most.
	diffCtx, cancelDiff := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelDiff()
	out.Diff, out.DiffStat = collectDiff(diffCtx, absRepo)
	out.DiffPath = filepath.Join(logDir, "diff.patch")
	writeFile(out.DiffPath, out.Diff)

	return out
}

// workerArgv is the whole of what rig tells Claude Code to be. Read it as the
// switch's actual surface area.
func workerArgv(bin, task string) []string {
	args := []string{
		bin,
		"-p", task,
		"--output-format", "stream-json", "--verbose",
		"--model", model(),
		// rig supplies no MCP servers of its own to the worker, and must not
		// inherit the user's.
		"--strict-mcp-config",
		// It edits and runs tests unattended; its blast radius is this checkout
		// and its inference is local.
		"--dangerously-skip-permissions",
		// A1's finding, fixed at the root: the worker inherited MAIN's settings —
		// including the output style whose text tells it not to edit files or run
		// commands, so it dutifully tried to delegate its own job. Loading NO
		// setting source is the honest form of that fix. Measured on this machine
		// 2026-08-04: without it, two of film's SessionStart hooks fire inside the
		// worker session and one injects an instruction to use a tool that
		// --strict-mcp-config has already taken away; with it, none fire.
		//
		// The worker's instructions come from --append-system-prompt and the
		// repo's own CLAUDE.md, and from nothing else on this machine.
		"--setting-sources", "",
		// Belt and braces: with no sources there is no style to inherit, but this
		// says so out loud and survives a future change to what "no sources" means.
		"--settings", `{"outputStyle":"default"}`,
		"--append-system-prompt", systemPrompt,
	}
	if denied := disallowedTools(); len(denied) > 0 {
		args = append(args, "--disallowedTools", strings.Join(denied, ","))
	}
	return args
}

func model() string {
	if v := strings.TrimSpace(os.Getenv("RIG_CC_MODEL")); v != "" {
		return v
	}
	// Doubles as the routing key when the base URL is the model-routing shim.
	return "haiku"
}

func apiKey() string {
	if v := strings.TrimSpace(os.Getenv("RIG_CC_API_KEY")); v != "" {
		return v
	}
	return "sk-rig-thin-worker-local"
}

// childEnv strips every RIG_ variable on the way down. The worker must not be
// able to read rig's own configuration, and — the reason it was added (#42/#24) —
// an identity that leaks into the processes the worker itself starts comes back
// as tool denials it cannot diagnose.
func childEnv(base string) []string {
	parent := os.Environ()
	env := make([]string, 0, len(parent)+2)
	for _, kv := range parent {
		if strings.HasPrefix(kv, "RIG_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"ANTHROPIC_BASE_URL="+base,
		"ANTHROPIC_API_KEY="+apiKey())
}

// liveness is the stall guard's view of the run: when the stream last said
// anything, and whether we are the ones who killed it.
type liveness struct {
	mu     sync.Mutex
	last   time.Time
	killed string // non-empty once a guard has killed the run, naming the guard
}

func (l *liveness) touch() {
	l.mu.Lock()
	l.last = time.Now()
	l.mu.Unlock()
}

func (l *liveness) silentFor() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return time.Since(l.last)
}

func (l *liveness) kill(reason string) {
	l.mu.Lock()
	if l.killed == "" {
		l.killed = reason
	}
	l.mu.Unlock()
}

func (l *liveness) killReason() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.killed
}

// watchStall kills a run that has gone quiet. Returns a stop function.
func watchStall(cmd *exec.Cmd, live *liveness) func() {
	limit := stallCeiling()
	if limit <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		tick := time.NewTicker(stallTick(limit))
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				if live.silentFor() > limit {
					live.kill(fmt.Sprintf("no output for %s", limit))
					_ = killProcGroup(cmd)
					return
				}
			}
		}
	}()
	return func() { close(done) }
}

// stallTick is how often the guard looks, bounded so a short test limit is
// still checked promptly and a long production one is not polled needlessly.
func stallTick(limit time.Duration) time.Duration {
	t := limit / 10
	if t > 10*time.Second {
		t = 10 * time.Second
	}
	if t < 100*time.Millisecond {
		t = 100 * time.Millisecond
	}
	return t
}

// observed is what the stream told us. It is small on purpose: the stream is
// evidence for a human reading the log, not an input to a decision.
type observed struct {
	sawResult   bool
	lines       int
	jsonLines   int
	lastCommand string
	tools       []string
}

// parseStream mirrors every line to the run log and picks out the two things the
// return needs: whether the process reached its own end, and the last shell
// command it ran. It also reads the init event's tool inventory, so what the
// worker ACTUALLY got is recorded rather than assumed — the inventory has been
// wrong before (the worker had no Grep or Glob for months and nobody knew).
func parseStream(r interface{ Read([]byte) (int, error) }, logDir string, live *liveness, actions *actionLog) observed {
	var obs observed

	mirror, _ := os.OpenFile(filepath.Join(logDir, "stream.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if mirror != nil {
		defer mirror.Close()
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24) // tool results get large

	bashCmd := map[string]string{} // tool_use id -> command

	for sc.Scan() {
		line := sc.Bytes()
		obs.lines++
		live.touch()
		if mirror != nil {
			mirror.Write(append(append([]byte{}, line...), '\n'))
		}

		var ev struct {
			Type    string          `json:"type"`
			Subtype string          `json:"subtype"`
			Tools   []string        `json:"tools"`
			Message json.RawMessage `json:"message"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		obs.jsonLines++

		switch ev.Type {
		case "system":
			if ev.Subtype == "init" && len(ev.Tools) > 0 {
				obs.tools = ev.Tools
				checkInventory(logDir, ev.Tools)
			}
		case "result":
			obs.sawResult = true
		case "assistant":
			for _, c := range contentBlocks(ev.Message) {
				if c.Type != "tool_use" {
					continue
				}
				if actions != nil {
					actions.toolCall(c.ID, c.Name, c.Input)
				}
				if c.Name == "Bash" {
					var in struct {
						Command string `json:"command"`
					}
					if json.Unmarshal(c.Input, &in) == nil && in.Command != "" {
						bashCmd[c.ID] = in.Command
						obs.lastCommand = "$ " + in.Command
					}
				}
			}
		case "user":
			for _, c := range contentBlocks(ev.Message) {
				if c.Type != "tool_result" {
					continue
				}
				text := resultText(c.Content)
				if actions != nil {
					actions.toolResult(c.ToolUseID, text, c.IsError)
				}
				if cmdText, ok := bashCmd[c.ToolUseID]; ok {
					obs.lastCommand = "$ " + cmdText + "\n" + tail(text, 2000)
				}
			}
		}
	}
	return obs
}

type contentBlock struct {
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

func contentBlocks(msg json.RawMessage) []contentBlock {
	var m struct {
		Content []contentBlock `json:"content"`
	}
	if json.Unmarshal(msg, &m) != nil {
		return nil
	}
	return m.Content
}

// resultText flattens a tool_result content field, which the stream writes
// either as a bare string or as a list of blocks.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, blk := range blocks {
			b.WriteString(blk.Text)
		}
		return b.String()
	}
	return ""
}

// checkInventory writes down the tools the worker really has and says so loudly
// when a tool we asked to remove is present anyway. Believing the flag rather
// than the init event is how the escape routes survived unnoticed.
func checkInventory(logDir string, tools []string) {
	writeFile(filepath.Join(logDir, "inventory.txt"), strings.Join(tools, "\n")+"\n")
	denied := map[string]bool{}
	for _, d := range disallowedTools() {
		denied[strings.TrimSpace(d)] = true
	}
	var leaked []string
	for _, t := range tools {
		if denied[t] {
			leaked = append(leaked, t)
		}
	}
	logf("worker inventory: %d tools", len(tools))
	if len(leaked) > 0 {
		logf("WARNING: --disallowedTools did not remove %s — the worker can still hand its job on",
			strings.Join(leaked, ","))
	}
}

// describeExit turns what happened into the one status line. The order matters:
// a run we killed is reported as killed even if the subprocess also managed to
// exit cleanly on the way down.
func describeExit(clientCtx, runCtx context.Context, live *liveness, waitErr error, obs observed, stderr fmt.Stringer) string {
	if reason := live.killReason(); reason != "" {
		return "killed(" + reason + ")"
	}
	if clientCtx.Err() != nil {
		return "killed(the caller cancelled this call)"
	}
	if runCtx.Err() != nil {
		return fmt.Sprintf("killed(wall ceiling %s)", wallCeiling())
	}
	if obs.sawResult {
		return statusFinished
	}
	// No terminal result event: the CLI is not the one we know how to read, or it
	// died. Say which, because the two need different fixes.
	msg := "error: the worker stream ended without a result"
	switch {
	case obs.lines == 0:
		msg += " and produced no output at all"
	case obs.jsonLines == 0:
		msg += fmt.Sprintf(" — %d lines of output, none of it stream-json (claude CLI version skew?)", obs.lines)
	default:
		msg += fmt.Sprintf(" — %d stream-json lines parsed", obs.jsonLines)
	}
	if waitErr != nil {
		msg += " (" + waitErr.Error() + ")"
	}
	if s := strings.TrimSpace(stderr.String()); s != "" {
		msg += ": " + tail(s, 2000)
	}
	return msg
}

// runRoot is where every run's evidence lives. `rig watch` resolves it the same
// way the run itself does, so following a run never needs to be told where to look.
func runRoot() (string, error) {
	if root := strings.TrimSpace(os.Getenv("RIG_THIN_LOG_ROOT")); root != "" {
		return root, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".rig-move-llm", "runs"), nil
}

// newRunDir makes the folder this run's evidence lives in. The path is quoted
// back in the return, so a human can open it while the next run is starting.
func newRunDir() (string, error) {
	root, err := runRoot()
	if err != nil {
		return "", err
	}
	var b [3]byte
	_, _ = rand.Read(b[:])
	dir := filepath.Join(root, time.Now().Format("20060102-150405")+"-"+hex.EncodeToString(b[:]))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func writeFile(path, body string) {
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		logf("could not write %s: %v", path, err)
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func logf(format string, args ...any) {
	line := fmt.Sprintf("[rig-thin] "+format+"\n", args...)
	fmt.Fprint(os.Stderr, line)
	// A stdio server's stderr belongs to whoever launched it; under Claude Code
	// that is out of reach of anyone measuring.
	if path := strings.TrimSpace(os.Getenv("RIG_THIN_LOG")); path != "" {
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			_, _ = f.WriteString(line)
			_ = f.Close()
		}
	}
}
