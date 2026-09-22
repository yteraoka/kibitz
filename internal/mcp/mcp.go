// Package mcp is a minimal Model Context Protocol server over stdio.
//
// It implements the three methods a tool provider needs — initialize,
// tools/list and tools/call — and nothing else. kibitz exposes read-only facts
// about one pull request; it has no resources, no prompts and no sampling, and
// a server that answered those would be claiming capabilities it does not
// have.
//
// The transport is JSON-RPC 2.0, one message per line on stdin and stdout, so
// stdout belongs to the protocol: anything a tool wants to say goes to stderr.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// DefaultProtocolVersion is what the server claims when a client does not ask
// for a version it recognizes.
const DefaultProtocolVersion = "2025-06-18"

// JSON-RPC error codes, from the specification.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// request is one incoming JSON-RPC message. A message without an id is a
// notification and is answered with silence.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Tool is one callable a client may invoke.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// InputSchema is a JSON Schema object. It is required even when the tool
	// takes no arguments, in which case it is an object with no properties.
	InputSchema map[string]any `json:"inputSchema"`
	// Handler runs the tool. The text it returns is what the model reads, so
	// an error worth acting on belongs in there rather than in err; err is for
	// a call that could not be attempted at all.
	Handler func(ctx context.Context, arguments json.RawMessage) (string, error) `json:"-"`
}

// Server answers one client over one pair of streams.
type Server struct {
	Name    string
	Version string
	// Instructions tell the model what this server is for. They are sent once,
	// in the initialize result.
	Instructions string
	Tools        []Tool

	mu sync.Mutex
}

// Serve reads requests until the input ends or ctx is cancelled.
//
// A malformed line is answered with an error and the loop continues: one bad
// message is not a reason to drop a session the client still believes in.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	// A tool result carrying a diff is easily past the default 64 KiB.
	scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			s.write(out, response{
				JSONRPC: "2.0",
				ID:      json.RawMessage("null"),
				Error:   &rpcError{Code: CodeParseError, Message: err.Error()},
			})
			continue
		}
		// No id means a notification: "notifications/initialized" is the one
		// every client sends, and the protocol forbids answering it.
		if len(req.ID) == 0 {
			continue
		}

		result, rpcErr := s.dispatch(ctx, req)
		resp := response{JSONRPC: "2.0", ID: req.ID}
		if rpcErr != nil {
			resp.Error = rpcErr
		} else {
			resp.Result = result
		}
		s.write(out, resp)
	}
	return scanner.Err()
}

func (s *Server) dispatch(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return s.initialize(req.Params), nil
	case "ping":
		// Answered with an empty object, which is what the specification asks
		// for and what keeps a client from declaring the server dead.
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.Tools}, nil
	case "tools/call":
		return s.call(ctx, req.Params)
	default:
		return nil, &rpcError{Code: CodeMethodNotFound, Message: "method " + req.Method + " is not implemented"}
	}
}

// initialize answers the handshake.
//
// The client's protocol version is echoed when it asked for one: this server
// speaks only the three methods that have not changed between versions, so
// agreeing is safer than insisting on a version the client may not know.
func (s *Server) initialize(params json.RawMessage) any {
	version := DefaultProtocolVersion
	var asked struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(params, &asked); err == nil && asked.ProtocolVersion != "" {
		version = asked.ProtocolVersion
	}

	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
		"instructions":    s.Instructions,
	}
}

// call runs one tool.
//
// A tool that fails returns its failure as content with isError set, rather
// than as a protocol error: the model is the one who can do something about
// "no such file", and a JSON-RPC error never reaches it.
func (s *Server) call(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return nil, &rpcError{Code: CodeInvalidParams, Message: err.Error()}
	}

	for _, tool := range s.Tools {
		if tool.Name != call.Name {
			continue
		}
		text, err := tool.Handler(ctx, call.Arguments)
		if err != nil {
			return textResult(err.Error(), true), nil
		}
		return textResult(text, false), nil
	}
	return textResult(fmt.Sprintf("there is no tool called %q", call.Name), true), nil
}

func textResult(text string, isError bool) any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": isError,
	}
}

func (s *Server) write(out io.Writer, resp response) {
	s.mu.Lock()
	defer s.mu.Unlock()

	encoded, err := json.Marshal(resp)
	if err != nil {
		// Nothing useful can be said on stdout, which belongs to the protocol.
		return
	}
	_, _ = out.Write(append(encoded, '\n'))
}

// ErrNoTool reports a call for a tool that was never registered.
var ErrNoTool = errors.New("mcp: no such tool")

// Call invokes a tool directly, which is what the CLI mode uses. The same
// handlers serve both, so the two cannot drift apart.
func (s *Server) Call(ctx context.Context, name string, arguments json.RawMessage) (string, error) {
	for _, tool := range s.Tools {
		if tool.Name == name {
			return tool.Handler(ctx, arguments)
		}
	}
	return "", fmt.Errorf("%w: %s", ErrNoTool, name)
}
