package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/config"
)

// mcpProtocolVersion is the MCP revision we advertise. 2024-11-05 is broadly
// supported by strict clients (incl. Claude Code); the handshake echoes whatever
// the client asks for when it is one we understand, else this default.
const mcpProtocolVersion = "2024-11-05"

// runTimeout bounds a single implement call end-to-end (the agentic loop can take
// minutes on a local model). Overridable via RIG_WORKER_RUN_TIMEOUT (seconds).
func runTimeout() time.Duration {
	return time.Duration(envInt("RIG_WORKER_RUN_TIMEOUT", 3600)) * time.Second
}

// rpcRequest / rpcResponse are the minimal JSON-RPC 2.0 shapes we handle. id is
// json.RawMessage because JSON-RPC allows string OR number ids, and notifications
// omit it (nil).
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

// Server is the stdio MCP server exposing worker.implement.
type Server struct {
	engine *Engine
	out    *bufio.Writer
}

// Serve runs the MCP stdio loop over r/w until r is closed (EOF). It is the body
// of the `rig-move-llm worker` subcommand. cfg is the resolved worker config.
func Serve(cfg config.Config, r io.Reader, w io.Writer) error {
	s := &Server{engine: NewEngine(cfg), out: bufio.NewWriter(w)}
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			s.handleLine(line)
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func (s *Server) handleLine(line []byte) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		logStderr("mcp: bad json: %v", err)
		return
	}
	// Notifications (no id) get no response.
	isNotification := len(req.ID) == 0

	switch req.Method {
	case "initialize":
		s.reply(req.ID, s.onInitialize(req.Params))
	case "notifications/initialized", "initialized":
		// no-op notification
	case "ping":
		s.reply(req.ID, map[string]any{})
	case "tools/list":
		s.reply(req.ID, map[string]any{"tools": toolList()})
	case "tools/call":
		res, rerr := s.onToolsCall(req.Params)
		if rerr != nil {
			s.replyErr(req.ID, rerr.Code, rerr.Message)
			return
		}
		s.reply(req.ID, res)
	default:
		if !isNotification {
			s.replyErr(req.ID, -32601, "method not found: "+req.Method)
		}
	}
}

func (s *Server) onInitialize(params json.RawMessage) map[string]any {
	proto := mcpProtocolVersion
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(params, &p) == nil && p.ProtocolVersion != "" {
		proto = p.ProtocolVersion // echo the client's requested revision
	}
	return map[string]any{
		"protocolVersion": proto,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": "rig-move-llm-worker", "version": "1"},
	}
}

// toolList is the tools/list payload: the single implement tool.
func toolList() []map[string]any {
	return []map[string]any{{
		"name":        "implement",
		"description": "Resolve a coding task by running an agentic loop on the local worker model: it reads/edits files in the repo and runs tests, then returns a summary, the diff, and the last test output. Delegate all code changes here.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task":     map[string]any{"type": "string", "description": "The task / bug to resolve, with enough detail to act on."},
				"repo":     map[string]any{"type": "string", "description": "Absolute path to the repo checkout. Defaults to the server's working directory."},
				"gate_dir": map[string]any{"type": "string", "description": "Optional path to the frozen .gate/ contract; the worker will not modify it."},
			},
			"required": []string{"task"},
		},
	}}
}

func (s *Server) onToolsCall(params json.RawMessage) (map[string]any, *rpcError) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return nil, &rpcError{Code: -32602, Message: "bad params: " + err.Error()}
	}
	if call.Name != "implement" {
		return nil, &rpcError{Code: -32602, Message: "unknown tool: " + call.Name}
	}

	var args struct {
		Task    string `json:"task"`
		Repo    string `json:"repo"`
		GateDir string `json:"gate_dir"`
	}
	_ = json.Unmarshal(call.Arguments, &args)
	if strings.TrimSpace(args.Task) == "" {
		return toolText(`{"error":"task is required"}`, true), nil
	}
	if args.Repo == "" {
		args.Repo, _ = os.Getwd()
	}
	if args.GateDir == "" {
		if cand := args.Repo + "/.gate"; dirExists(cand) {
			args.GateDir = cand
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), runTimeout())
	defer cancel()
	logStderr("worker.implement repo=%s gate=%s", args.Repo, args.GateDir)
	res := s.engine.Implement(ctx, args.Repo, args.Task, args.GateDir)

	body, _ := json.MarshalIndent(res, "", "  ")
	logStderr("worker.implement done stopped=%s iters=%d in=%d out=%d files=%v",
		res.Stopped, res.Iterations, res.InputTokens, res.OutputTokens, res.FilesChanged)
	return toolText(string(body), res.Stopped == "error"), nil
}

// toolText wraps a text payload in the MCP tools/call result envelope.
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

func (s *Server) write(resp rpcResponse) {
	b, err := json.Marshal(resp)
	if err != nil {
		logStderr("mcp: marshal response: %v", err)
		return
	}
	s.out.Write(b)
	s.out.WriteByte('\n')
	s.out.Flush()
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func logStderr(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[rig-worker] "+format+"\n", args...)
}
