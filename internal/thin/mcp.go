package thin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// protocolVersion is the MCP revision advertised when the client does not ask
// for one it and we both understand.
const protocolVersion = "2024-11-05"

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Server is the stdio MCP server. It exposes one tool.
type Server struct {
	writeMu sync.Mutex
	out     *bufio.Writer

	// inflight maps a request id to the cancel function of the call running for
	// it. This is the whole of the cancellation wiring, and it only works because
	// the read loop below never waits for a call to finish.
	mu       sync.Mutex
	inflight map[string]context.CancelFunc
}

// Serve runs the MCP stdio loop until stdin closes or ctx is cancelled.
//
// The shape of this loop IS the fix G1 asked for. The old one read a line,
// handled it to completion, then read the next — so for the 30–50 minutes an
// implement call took, nothing was reading stdin, and the client's
// notifications/cancelled sat in a pipe buffer until the work it wanted stopped
// had already stopped by itself. Adding a handler for that notification would
// have changed nothing; the loop had to stop blocking first.
//
// Three consequences follow, and all three are load-bearing:
//
//  1. tools/call runs in its own goroutine, so reading continues underneath it;
//  2. every in-flight call has a cancel function filed under its request id, so
//     notifications/cancelled has something to pull;
//  3. stdin EOF and ctx cancellation cancel everything in flight, so a client
//     that hangs up — or a SIGTERM the caller turns into ctx cancellation — takes
//     the worker down instead of leaving it running for an audience of nobody.
//
// The fourth case, SIGKILL of this process, cannot be handled here by
// construction. It is handled one process further down, in supervise.go.
func Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	s := &Server{out: bufio.NewWriter(w), inflight: map[string]context.CancelFunc{}}

	// Defer order is load-bearing (LIFO): calls are cancelled, THEN waited for.
	// Waiting first would hang for as long as the run we are trying to abandon.
	var wg sync.WaitGroup
	defer wg.Wait()
	defer s.cancelAll()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Cancelling ctx must interrupt work even while the read loop is parked in
	// ReadBytes, which is not interruptible.
	go func() {
		<-ctx.Done()
		s.cancelAll()
	}()

	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			s.dispatch(ctx, &wg, line)
		}
		if err != nil {
			// EOF is the client hanging up. Anything still running was being run
			// for that client.
			s.cancelAll()
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// dispatch handles one message. Everything cheap is answered inline; the one
// expensive method gets a goroutine.
func (s *Server) dispatch(ctx context.Context, wg *sync.WaitGroup, line []byte) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		logf("bad json on stdin: %v", err)
		return
	}
	isNotification := len(req.ID) == 0

	switch req.Method {
	case "initialize":
		s.reply(req.ID, onInitialize(req.Params))
	case "notifications/initialized", "initialized":
		// no-op
	case "ping":
		s.reply(req.ID, map[string]any{})
	case "tools/list":
		s.reply(req.ID, map[string]any{"tools": toolList()})
	case "notifications/cancelled", "$/cancelRequest":
		s.onCancelled(req.Params)
	case "tools/call":
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.runToolCall(ctx, req)
		}()
	default:
		if !isNotification {
			s.replyErr(req.ID, -32601, "method not found: "+req.Method)
		}
	}
}

// onCancelled is the MCP-spec abort. It carries the id of the request to stop,
// so the lookup is exact: a second call is not cancelled because the first one
// was.
func (s *Server) onCancelled(params json.RawMessage) {
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
		ID        json.RawMessage `json:"id"` // $/cancelRequest spelling
		Reason    string          `json:"reason"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	raw := p.RequestID
	if len(raw) == 0 {
		raw = p.ID
	}
	key := idKey(raw)
	if key == "" {
		return
	}
	s.mu.Lock()
	cancel, ok := s.inflight[key]
	s.mu.Unlock()
	if !ok {
		logf("cancel for request %s: nothing in flight", key)
		return
	}
	logf("cancel for request %s (%s) — killing the worker", key, strings.TrimSpace(p.Reason))
	cancel()
}

func (s *Server) cancelAll() {
	s.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.inflight))
	for _, c := range s.inflight {
		cancels = append(cancels, c)
	}
	s.mu.Unlock()
	for _, c := range cancels {
		c()
	}
}

// idKey normalizes a JSON-RPC id for map lookup. JSON-RPC allows numbers and
// strings, and a client is free to spell the same id either way between the call
// and its cancellation; comparing the raw JSON bytes with whitespace stripped
// matches both without inventing a type.
func idKey(raw json.RawMessage) string {
	return string(bytes.TrimSpace(raw))
}

func (s *Server) runToolCall(ctx context.Context, req rpcRequest) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &call); err != nil {
		s.replyErr(req.ID, -32602, "bad params: "+err.Error())
		return
	}
	if call.Name != "implement" {
		s.replyErr(req.ID, -32602, "unknown tool: "+call.Name)
		return
	}

	var args struct {
		Task string `json:"task"`
		Repo string `json:"repo"`
	}
	_ = json.Unmarshal(call.Arguments, &args)
	if strings.TrimSpace(args.Task) == "" {
		s.reply(req.ID, toolText("status: error: task is required", true))
		return
	}
	if strings.TrimSpace(args.Repo) == "" {
		// Under the product wiring the server's own working directory is rig's
		// checkout, not the repo being worked on, which is why `repo` exists at
		// all (B6 run 2). The cwd is only the fallback.
		args.Repo, _ = os.Getwd()
	}

	callCtx, cancel := context.WithCancel(ctx)
	key := idKey(req.ID)
	s.mu.Lock()
	s.inflight[key] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.inflight, key)
		s.mu.Unlock()
		cancel()
	}()

	logf("implement repo=%s wall=%s stall=%s", args.Repo, wallCeiling(), stallCeiling())
	out := Run(callCtx, args.Repo, args.Task)
	logf("implement %s log=%s diff_bytes=%d", out.Status, out.LogDir, len(out.Diff))

	// isError is left false even for a killed or errored run: the text says what
	// happened, and a killed run still carries the diff of the work that reached
	// disk. Flagging it as a protocol error is how that work gets discarded by a
	// client that only reads the flag.
	s.reply(req.ID, toolText(Render(out), false))
}

// Render turns an outcome into the text the caller reads. Four sections, no
// JSON to unpack, and nothing in it that a machine has to trust (S1).
func Render(out Outcome) string {
	var b strings.Builder
	fmt.Fprintf(&b, "status: %s\n", out.Status)
	if out.LogDir != "" {
		fmt.Fprintf(&b, "log:    %s\n", out.LogDir)
	}

	b.WriteString("\n--- diff ---\n")
	switch {
	case strings.TrimSpace(out.Diff) == "":
		b.WriteString("(the working tree is unchanged)\n")
	case len(out.Diff) <= inlineDiffLimit():
		b.WriteString(strings.TrimRight(out.Diff, "\n") + "\n")
	default:
		// Same bytes either way — this branch just declines to paste them.
		b.WriteString(strings.TrimRight(out.DiffStat, "\n") + "\n")
		fmt.Fprintf(&b, "\n(%d bytes of diff — read it at %s)\n", len(out.Diff), out.DiffPath)
	}

	b.WriteString("\n--- last command ---\n")
	if strings.TrimSpace(out.LastCommand) == "" {
		b.WriteString("(the worker ran no shell command)\n")
	} else {
		b.WriteString(strings.TrimRight(out.LastCommand, "\n") + "\n")
	}
	return b.String()
}

func onInitialize(params json.RawMessage) map[string]any {
	proto := protocolVersion
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(params, &p) == nil && p.ProtocolVersion != "" {
		proto = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": proto,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": "rig-move-llm-thin", "version": "1"},
	}
}

// toolList is one tool. There is no gate_dir and no ceiling parameter: the
// ceiling is film's policy, held in config, not something a caller sets per call.
func toolList() []map[string]any {
	return []map[string]any{{
		"name": "implement",
		"description": "Hand a coding task to the local worker model, which does it in the repo with the full Claude Code harness: it reads and edits files and runs the repo's tests. Returns the diff for you to read. " +
			"Scope the task before sending it; you are not asked to verify the result, the human reads the diff.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task": map[string]any{"type": "string", "description": "The task, in plain language, with enough detail to act on."},
				"repo": map[string]any{"type": "string", "description": "Absolute path to the repo checkout. Defaults to the server's working directory."},
			},
			"required": []string{"task"},
		},
	}}
}

func toolText(text string, isErr bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	}
}

func (s *Server) reply(id json.RawMessage, result interface{}) {
	s.write(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) replyErr(id json.RawMessage, code int, msg string) {
	s.write(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

// write is mutex-guarded because calls now run concurrently with the read loop:
// two goroutines interleaving bytes on stdout would corrupt the framing.
func (s *Server) write(resp rpcResponse) {
	b, err := json.Marshal(resp)
	if err != nil {
		logf("marshal response: %v", err)
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.out.Write(b)
	s.out.WriteByte('\n')
	s.out.Flush()
}
