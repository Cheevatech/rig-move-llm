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

	"github.com/Cheevatech/rig-move-llm/internal/gatestate"
)

// payload is the subset of the Claude Code hook stdin we consume.
type payload struct {
	AgentID   string `json:"agent_id"`
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		FilePath        string `json:"file_path"`
		RunInBackground *bool  `json:"run_in_background"`
		// Edit / Write / MultiEdit bodies — used by the soft gate to size-bound
		// MAIN's solo and Gate B repair edits.
		NewString string `json:"new_string"`
		Content   string `json:"content"`
		Edits     []struct {
			NewString string `json:"new_string"`
		} `json:"edits"`
	} `json:"tool_input"`
}

// editSize is the byte size of the new content a MAIN edit would introduce.
func editSize(p payload) int {
	n := len(p.ToolInput.NewString) + len(p.ToolInput.Content)
	for _, e := range p.ToolInput.Edits {
		n += len(e.NewString)
	}
	return n
}

// State locates the hook's on-disk state (log + gate-path ledger) and the
// external gate runner. Paths are resolved by the caller from config/env.
type State struct {
	// Enabled is the master switch. When false the hook passes every tool through
	// (no force-delegate, no gate) so Claude Code behaves exactly as it would
	// without rig — the mode you get by skipping the worker in setup. The global
	// hook is always wired; this gates its behaviour at runtime from config.
	Enabled    bool
	LogPath    string // append-only decision trail
	GatePaths  string // ledger of authored .gate dirs (one per line)
	GateRunner string // external command: `<runner> <repo>` -> "VERDICT|detail"
	// SharedMCP is the allowlist of MCP server names the MAIN agent may still use
	// (e.g. "serena", "headroom"); everything else mcp__* is denied for MAIN.
	SharedMCP map[string]bool
	// HealthMarker is the health.json written by the UserPromptSubmit hook. When it
	// records an unhealthy worker (and is still fresh, per HealthTTL) the per-tool
	// hooks pass everything through — the hybrid degrades to plain Claude Code
	// instead of blocking on a dead worker. Empty disables the check.
	HealthMarker string
	HealthTTL    time.Duration
	// GateMode is config.GateMode ("hard"|"soft"). Soft enables the map6
	// cost-aware windows: a triage-opened solo edit window (Gate A) and a
	// bounded post-worker repair window (Gate B). StateDir is where the worker
	// MCP server persists the explore/triage/repair state files.
	GateMode string
	StateDir string
}

// Soft-gate bounds. Solo covers a declared small change at Stage-0-verified
// sites; repair covers tiny residue fixes after a worker return. A "minor fix"
// that exceeds these is a worker fail that should be re-delegated, not patched.
const (
	soloEditMaxBytes   = 4000
	repairEditMaxBytes = 2000
	repairEditCount    = 3
)

// healthDownActive reports whether the last probe found the worker unreachable and
// that verdict is still fresh. A missing/healthy/stale marker is treated as up, so
// the default (and any transient read error) keeps normal force-delegate behaviour.
func (s *State) healthDownActive() bool {
	if s.HealthMarker == "" {
		return false
	}
	healthy, ts, ok := ReadHealthMarker(s.HealthMarker)
	if !ok || healthy {
		return false
	}
	if s.HealthTTL > 0 && time.Since(ts) > s.HealthTTL {
		return false // stale down-verdict: fail safe back to force-delegate
	}
	return true
}

// effectiveAgentID resolves the subagent identity for the current tool call.
// Classic subagents carry it in the payload's agent_id. Agent-team teammates
// spawned via a terminal backend (tmux/iterm2) run as a separate `claude`
// process whose hook payloads have NO agent_id — so without this they would be
// mistaken for the paid MAIN and denied their tools. rig's teammate-exec
// launcher stamps their identity into RIG_AGENT_ID, which we treat as
// equivalent here. The MAIN (lead) process is launched without RIG_AGENT_ID and
// its payloads have no agent_id, so it keeps its plan/delegate/review-only
// posture (reliability floor #1: the lead can never obtain a worker marker).
func effectiveAgentID(p payload) string {
	if p.AgentID != "" {
		return p.AgentID
	}
	return os.Getenv("RIG_AGENT_ID")
}

// workerTool is the OPTION-2 offload tool (P9): the MAIN agent delegates all code
// work by calling it. Its server key ("worker") is fixed by the generated
// mcp-config; CC exposes it as mcp__worker__implement.
const workerTool = "mcp__worker__implement"

// workerServer is the MCP server name whose tools MAIN is allowed to call.
const workerServer = "worker"

const denyReason = "Main agent is plan/delegate/review only. Delegate this to the worker by calling the implement tool (mcp__worker__implement) — it edits files, runs the tests, and returns a diff. The ONLY files you may Write yourself are the gate contract under <repo>/.gate/ (repro + gate.json). Do not edit files, run commands, or spawn subagents yourself."

// PreTool implements the force-delegate PreToolUse hook. It reads the payload
// from r, may mutate on-disk state, and writes any decision JSON to w. It always
// returns nil error (a silent allow is an empty stdout + exit 0 by the caller).
func (s *State) PreTool(r io.Reader, w io.Writer) error {
	p := decode(r)

	// Master switch off, or the worker failed its health check this turn: pass
	// everything through. Claude Code runs normally — no force-delegate, no worker
	// required. The health-down branch is the automatic fallback so a dead worker
	// endpoint degrades to plain Claude Code instead of blocking MAIN.
	if !s.Enabled || s.healthDownActive() {
		return nil
	}

	agentID := effectiveAgentID(p)
	s.logf("agent_id=%s tool=%s", orMain(agentID), or(p.ToolName, "?"))

	// Subagent (or teammate): allow everything (it is the worker).
	if agentID != "" {
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
		if s.GateMode == "soft" {
			if allow, reason := s.softEdit(p); allow {
				return nil
			} else if reason != "" {
				return denyMsg(w, reason)
			}
		}
		return deny(w)

	case p.ToolName == "Bash" || p.ToolName == "NotebookEdit":
		return deny(w)

	case strings.HasPrefix(p.ToolName, "mcp__"):
		// The offload tool (P9): MAIN's sanctioned way to do code work. Calling
		// implement is the delegate/freeze point — snapshot every authored .gate
		// dir before the worker runs, so the deterministic gate trusts only the
		// frozen contract. The Stage-0 explore/triage tools pass through freely.
		if mcpServer(p.ToolName) == workerServer {
			if p.ToolName == workerTool {
				s.freezeGateDirs()
				// Re-delegating supersedes any open Gate B window; a fresh one
				// opens when this call returns.
				if s.StateDir != "" {
					gatestate.ClearRepair(s.StateDir)
				}
			}
			return nil
		}
		// Shared-MCP tier: MAIN keeps the allowlisted servers, is denied the rest.
		if s.SharedMCP[mcpServer(p.ToolName)] {
			return nil
		}
		return deny(w)

	case p.ToolName == "Task" || p.ToolName == "Agent":
		// Under Option 2, native subagents are dead for offload: on CC 2.1.214 they
		// run in-process and never reach an interceptor, and their inference is on
		// the paid main leg (see ticket P9). Steer MAIN to the worker MCP tool.
		return denyMsg(w, "Do not spawn subagents. Delegate code work to the worker by calling mcp__worker__implement — it runs on your worker model and returns a diff + test result.")
	}

	return nil
}

// softEdit is the map6 soft-gate decision for a MAIN Edit/Write. It returns
// (true, "") to allow, (false, reason) to deny with a specific steer, and
// (false, "") to fall through to the standard hard deny.
func (s *State) softEdit(p payload) (bool, string) {
	if s.StateDir == "" {
		return false, ""
	}
	size := editSize(p)

	// Gate A: a triage-accepted solo window, restricted to the files the Stage-0
	// evidence grounded (the divergence check).
	if tr, fresh := gatestate.ReadTriage(s.StateDir); fresh && tr.Decision == "solo" {
		if !fileAllowed(p.ToolInput.FilePath, tr.SoloFiles) {
			s.logf("GATEA divergence file=%s allowed=%v", p.ToolInput.FilePath, tr.SoloFiles)
			return false, "DIVERGENCE: you declared solo but this file is outside the Stage-0 grounded scope (" +
				strings.Join(tr.SoloFiles, ", ") + "). Re-run mcp__worker__explore + mcp__worker__triage, or delegate to mcp__worker__implement."
		}
		if size > soloEditMaxBytes {
			s.logf("GATEA solo-oversize file=%s bytes=%d", p.ToolInput.FilePath, size)
			return false, fmt.Sprintf("Solo edit too large (%d bytes > %d): a declared-solo change this big contradicts the triage. Delegate it to mcp__worker__implement.", size, soloEditMaxBytes)
		}
		s.logf("GATEA solo-edit file=%s bytes=%d", p.ToolInput.FilePath, size)
		return true, ""
	}

	// Gate B: the bounded repair window after a worker return.
	if rep, open := gatestate.ReadRepair(s.StateDir); open {
		if size > repairEditMaxBytes {
			s.logf("GATEB oversize file=%s bytes=%d", p.ToolInput.FilePath, size)
			return false, fmt.Sprintf("Repair edit too large (%d bytes > %d): residue this big means the worker run failed — re-delegate to mcp__worker__implement instead of rewriting it yourself.", size, repairEditMaxBytes)
		}
		rep.EditsLeft--
		_ = gatestate.WriteRepair(s.StateDir, rep)
		s.logf("GATEB repair-edit file=%s bytes=%d left=%d", p.ToolInput.FilePath, size, rep.EditsLeft)
		return true, ""
	}

	return false, ""
}

// fileAllowed matches an edited absolute/relative path against the Stage-0
// grounded file list (repo-relative). Suffix match keeps it working across the
// hook's and the MCP server's differing path roots.
func fileAllowed(edited string, allowed []string) bool {
	edited = filepath.ToSlash(edited)
	for _, a := range allowed {
		a = filepath.ToSlash(a)
		if edited == a || strings.HasSuffix(edited, "/"+a) {
			return true
		}
	}
	return false
}

// PostTool implements the gate-verdict PostToolUse hook. On a MAIN delegate
// return it runs the external gate runner against every frozen contract and, if
// found, writes an additionalContext block steering MAIN's next move.
func (s *State) PostTool(r io.Reader, w io.Writer) error {
	if !s.Enabled || s.healthDownActive() {
		return nil
	}
	p := decode(r)

	// Only MAIN's delegate returns are gate points: the worker MCP tool (Option 2)
	// or a legacy/teammate Task/Agent.
	if effectiveAgentID(p) != "" {
		return nil
	}
	if p.ToolName != workerTool && p.ToolName != "Task" && p.ToolName != "Agent" {
		return nil
	}

	// Gate B: a worker just returned — open the bounded repair window so MAIN can
	// patch tiny residue itself instead of paying another delegation round-trip.
	if s.GateMode == "soft" && s.StateDir != "" && p.ToolName == workerTool {
		_ = gatestate.WriteRepair(s.StateDir, gatestate.Repair{EditsLeft: repairEditCount, OpenedAt: time.Now()})
		s.logf("GATEB window-open edits=%d", repairEditCount)
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
