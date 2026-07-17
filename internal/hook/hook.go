// Package hook implements rig-move-llm's Claude Code hook logic in Go, replacing
// the force-delegate.sh (PreToolUse) and gate-verdict.sh (PostToolUse) shell
// scripts. Both read the hook payload as JSON on stdin and write a hook decision
// as JSON on stdout.
//
// The design mirrors the validated bash hooks 1:1:
//
//   - Structural force-delegate: the MAIN agent (paid) may plan/delegate/review
//     only. It is denied the mutating/heavy tools and steered to the worker
//     subagent. The subagent (identified by a non-empty agent_id in the payload)
//     is allowed everything.
//   - Deterministic gate: MAIN authors a frozen fail-before contract under a
//     `.gate/` dir; the first delegate freezes it (snapshot + sha256 manifest);
//     when the delegate returns, an external gate runner verifies it and the
//     verdict is injected into MAIN's context.
package hook

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// payload is the subset of the Claude Code hook stdin we consume.
type payload struct {
	AgentID   string `json:"agent_id"`
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		FilePath        string `json:"file_path"`
		RunInBackground *bool  `json:"run_in_background"`
	} `json:"tool_input"`
}

// State locates the hook's on-disk state (log + gate-path ledger) and the
// external gate runner. Paths are resolved by the caller from config/env.
type State struct {
	LogPath    string // append-only decision trail
	GatePaths  string // ledger of authored .gate dirs (one per line)
	GateRunner string // external command: `<runner> <repo>` -> "VERDICT|detail"
	// SharedMCP is the allowlist of MCP server names the MAIN agent may still use
	// (e.g. "serena", "headroom"); everything else mcp__* is denied for MAIN.
	SharedMCP map[string]bool
}

const denyReason = "Main agent is plan/delegate/review only. Delegate this to the worker subagent (Task tool) — it edits files, uses knowledge/search, and runs the tests. The ONLY files you may Write yourself are the gate contract under <repo>/.gate/ (repro + gate.json). Do not do other code work or run commands yourself."

// PreTool implements the force-delegate PreToolUse hook. It reads the payload
// from r, may mutate on-disk state, and writes any decision JSON to w. It always
// returns nil error (a silent allow is an empty stdout + exit 0 by the caller).
func (s *State) PreTool(r io.Reader, w io.Writer) error {
	p := decode(r)
	s.logf("agent_id=%s tool=%s", orMain(p.AgentID), or(p.ToolName, "?"))

	// Subagent: allow everything (it is the worker).
	if p.AgentID != "" {
		return nil
	}

	switch {
	case p.ToolName == "Write" || p.ToolName == "Edit" || p.ToolName == "MultiEdit":
		fp := p.ToolInput.FilePath
		if i := strings.Index(fp, "/.gate/"); i >= 0 {
			// Seam 1: MAIN authors the contract. Remember the gate dir to freeze.
			gateDir := fp[:i] + "/.gate"
			s.appendGatePath(gateDir)
			s.logf("MAIN gate-author %s", fp)
			return nil
		}
		return deny(w)

	case p.ToolName == "Bash" || p.ToolName == "NotebookEdit":
		return deny(w)

	case strings.HasPrefix(p.ToolName, "mcp__"):
		// Shared-MCP tier: MAIN keeps the allowlisted servers, is denied the rest.
		if s.SharedMCP[mcpServer(p.ToolName)] {
			return nil
		}
		return deny(w)

	case p.ToolName == "Task" || p.ToolName == "Agent":
		// Delegates must run synchronously so the gate can verify the result when
		// it returns (a background delegate's PostToolUse fires at spawn-ack only).
		if !(p.ToolInput.RunInBackground != nil && *p.ToolInput.RunInBackground == false) {
			return denyMsg(w, "Delegates must run synchronously so the deterministic gate can verify the result when it returns. Re-issue this exact Task call with \"run_in_background\": false.")
		}
		// Seam 2: first delegate = freeze point. Snapshot every authored .gate dir
		// not yet frozen; the runner trusts only the snapshot.
		s.freezeGateDirs()
		return nil
	}

	return nil
}

// PostTool implements the gate-verdict PostToolUse hook. On a MAIN delegate
// return it runs the external gate runner against every frozen contract and, if
// found, writes an additionalContext block steering MAIN's next move.
func (s *State) PostTool(r io.Reader, w io.Writer) error {
	p := decode(r)

	// Only MAIN's Task/Agent returns are gate points.
	if p.AgentID != "" {
		return nil
	}
	if p.ToolName != "Task" && p.ToolName != "Agent" {
		return nil
	}
	dirs := s.readGatePaths()
	if len(dirs) == 0 {
		return nil
	}

	var ctx strings.Builder
	for _, gd := range dirs {
		repo := strings.TrimSuffix(gd, "/.gate")
		if !isDir(filepath.Join(repo, ".gate.frozen")) {
			continue
		}
		verdict, detail := s.runGate(repo)
		s.logf("GATE %s verdict=%s %s", repo, verdict, detail)
		ctx.WriteString(verdictContext(verdict, detail))
		ctx.WriteByte('\n')
	}

	out := strings.TrimRight(ctx.String(), "\n")
	if out == "" {
		return nil
	}
	return writeJSON(w, map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "PostToolUse",
			"additionalContext": out,
		},
	})
}

func verdictContext(verdict, detail string) string {
	switch verdict {
	case "GREEN":
		return "GATE VERDICT: GREEN — " + detail + "\n" +
			"Frozen-repro trust anchor satisfied (deterministic: fail-before, repro-after, floor, scoped regression all verified outside both agents). Do NOT review the worker's report and do NOT re-delegate. Reply with ONLY a closing line (max 3 lines): files changed + this verdict. GREEN means the declared contract passed — do not claim more than that."
	case "RED":
		return "GATE VERDICT: RED — " + detail + "\n" +
			"The deterministic gate failed. Review the worker's report and re-delegate a fix (normal review path)."
	case "UNVERIFIABLE", "NO_CONTRACT":
		return "GATE VERDICT: " + verdict + " — " + detail + "\n" +
			"No deterministic gate for this task. Use the normal review path."
	default:
		return "GATE VERDICT: RUNNER_ERROR — " + detail + "\n" +
			"Gate could not run. Use the normal review path; do not treat this as a pass."
	}
}

// runGate shells to the external gate runner. Absent runner or a crash is
// fail-closed (RUNNER_ERROR), never a pass.
func (s *State) runGate(repo string) (verdict, detail string) {
	if s.GateRunner == "" {
		return "RUNNER_ERROR", "no gate runner configured"
	}
	out, err := exec.Command("bash", s.GateRunner, repo).Output()
	if err != nil {
		return "RUNNER_ERROR", "run_gate crashed: " + err.Error()
	}
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '|'); i >= 0 {
		return line[:i], line[i+1:]
	}
	return "RUNNER_ERROR", "malformed runner output"
}

// freezeGateDirs snapshots each authored .gate dir to a sibling .gate.frozen
// (with a sha256 manifest) exactly once.
func (s *State) freezeGateDirs() {
	for _, gd := range s.readGatePaths() {
		if !isDir(gd) {
			continue
		}
		frozen := strings.TrimSuffix(gd, "/.gate") + "/.gate.frozen"
		if isDir(frozen) {
			continue
		}
		if err := copyTree(gd, frozen); err != nil {
			s.logf("FREEZE-ERR %s -> %s: %v", gd, frozen, err)
			continue
		}
		if err := writeManifest(frozen); err != nil {
			s.logf("FREEZE-MANIFEST-ERR %s: %v", frozen, err)
		}
		s.logf("FREEZE %s -> %s", gd, frozen)
	}
}

// --- gate-path ledger ---

func (s *State) appendGatePath(dir string) {
	if s.GatePaths == "" {
		return
	}
	f, err := os.OpenFile(s.GatePaths, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, dir)
}

// readGatePaths returns the unique, sorted set of authored gate dirs.
func (s *State) readGatePaths() []string {
	if s.GatePaths == "" {
		return nil
	}
	f, err := os.Open(s.GatePaths)
	if err != nil {
		return nil
	}
	defer f.Close()
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			seen[line] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- filesystem helpers ---

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

// writeManifest writes a .sha256 file listing "hash  ./relpath" for every file
// under root (excluding the manifest itself), matching the bash `shasum` output.
func writeManifest(root string) error {
	var lines []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || info.Name() == ".sha256" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		lines = append(lines, fmt.Sprintf("%x  ./%s", sum, filepath.ToSlash(rel)))
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(lines)
	return os.WriteFile(filepath.Join(root, ".sha256"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// --- decision / io helpers ---

func decode(r io.Reader) payload {
	var p payload
	data, _ := io.ReadAll(r)
	_ = json.Unmarshal(data, &p)
	return p
}

func deny(w io.Writer) error { return denyMsg(w, denyReason) }

func denyMsg(w io.Writer, reason string) error {
	return writeJSON(w, map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       "deny",
			"permissionDecisionReason": reason,
		},
	})
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	return enc.Encode(v)
}

func (s *State) logf(format string, args ...any) {
	if s.LogPath == "" {
		return
	}
	f, err := os.OpenFile(s.LogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s "+format+"\n", append([]any{time.Now().Format("15:04:05")}, args...)...)
}

func mcpServer(tool string) string {
	// mcp__<server>__<tool> -> <server>
	rest := strings.TrimPrefix(tool, "mcp__")
	if i := strings.Index(rest, "__"); i >= 0 {
		return rest[:i]
	}
	return rest
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func orMain(v string) string { return or(v, "MAIN") }
