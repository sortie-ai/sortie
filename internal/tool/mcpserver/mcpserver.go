// Package mcpserver implements an MCP stdio server that exposes
// registered [domain.AgentTool] implementations over JSON-RPC 2.0.
// The server reads newline-delimited JSON-RPC requests from a reader
// and writes responses to a writer. It handles initialize, tools/list,
// and tools/call methods per the Model Context Protocol specification.
package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/domain"
)

// JSON-RPC 2.0 error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// MCP protocol version reported in the initialize response.
const protocolVersion = "2024-11-05"

// jsonRPCRequest is the inbound JSON-RPC 2.0 request envelope.
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// jsonRPCResponse is the outbound JSON-RPC 2.0 response envelope.
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type initializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    capabilities `json:"capabilities"`
	ServerInfo      serverInfo   `json:"serverInfo"`
}

type capabilities struct {
	Tools *toolsCapability `json:"tools,omitempty"`
}

type toolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type toolsListResult struct {
	Tools []mcpTool `json:"tools"`
}

type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type toolCallResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Server handles MCP JSON-RPC requests over stdio. It dispatches
// tools/list and tools/call to a [domain.ToolRegistry]. Construct
// via [NewServer] and run with [Server.Serve].
type Server struct {
	registry *domain.ToolRegistry
	reader   io.Reader
	writer   io.Writer
	logger   *slog.Logger
	version  string
}

// NewServer creates an MCP stdio server that reads JSON-RPC requests
// from r and writes responses to w. The registry provides the tool
// set for tools/list and tools/call dispatch. The version string is
// reported in the initialize response's serverInfo.
func NewServer(registry *domain.ToolRegistry, r io.Reader, w io.Writer, logger *slog.Logger, version string) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		registry: registry,
		reader:   r,
		writer:   w,
		logger:   logger,
		version:  version,
	}
}

// Serve reads JSON-RPC requests from the reader in a loop and writes
// responses to the writer. It blocks until the reader returns io.EOF
// (stdin closed). Context cancellation is checked between messages;
// a blocked read is not interrupted until the next line arrives or
// the reader closes. Returns nil on clean shutdown (EOF or context
// cancellation), or an error on unexpected read failures. Write
// errors are logged but do not terminate the loop.
func (s *Server) Serve(ctx context.Context) error {
	scanner := bufio.NewScanner(s.reader)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 10<<20) // 10 MB max line size

	for {
		if !scanner.Scan() {
			if scanner.Err() == nil {
				return nil // EOF — clean shutdown
			}
			return fmt.Errorf("reading stdin: %w", scanner.Err())
		}

		select {
		case <-ctx.Done():
			return nil
		default:
		}

		line := scanner.Bytes()

		var req jsonRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.writeResponse(errorResponse(nil, codeParseError, "parse error"))
			continue
		}

		if req.JSONRPC != "2.0" {
			s.writeResponse(errorResponse(req.ID, codeInvalidRequest, "invalid request: jsonrpc must be \"2.0\""))
			continue
		}

		// Messages without an ID are notifications or malformed requests.
		// Either way, no response is possible — skip silently.
		if req.ID == nil || string(req.ID) == "null" {
			continue
		}

		var resp jsonRPCResponse
		switch req.Method {
		case "initialize":
			resp = s.handleInitialize(req.ID)
		case "tools/list":
			resp = s.handleToolsList(req.ID)
		case "tools/call":
			resp = s.handleToolsCall(ctx, req.ID, req.Params)
		default:
			resp = errorResponse(req.ID, codeMethodNotFound, fmt.Sprintf("method not found: %s", req.Method))
		}

		s.writeResponse(resp)
	}
}

func (s *Server) handleInitialize(id json.RawMessage) jsonRPCResponse {
	return jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result: initializeResult{
			ProtocolVersion: protocolVersion,
			Capabilities:    capabilities{Tools: &toolsCapability{}},
			ServerInfo:      serverInfo{Name: "sortie-tools", Version: s.version},
		},
	}
}

func (s *Server) handleToolsList(id json.RawMessage) jsonRPCResponse {
	tools := s.registry.List()
	mcpTools := make([]mcpTool, len(tools))
	for i, t := range tools {
		mcpTools[i] = mcpTool{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.InputSchema(),
		}
	}
	return jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  toolsListResult{Tools: mcpTools},
	}
}

func (s *Server) handleToolsCall(ctx context.Context, id json.RawMessage, params json.RawMessage) jsonRPCResponse {
	var p toolsCallParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, codeInvalidParams, "invalid params: "+err.Error())
	}
	if p.Name == "" {
		return errorResponse(id, codeInvalidParams, "invalid params: tool name required")
	}

	tool, ok := s.registry.Get(p.Name)
	if !ok {
		return errorResponse(id, codeInvalidParams, fmt.Sprintf("invalid params: unknown tool %q", p.Name))
	}

	output, err := tool.Execute(ctx, p.Arguments)
	if err != nil {
		return jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      id,
			Result: toolCallResult{
				Content: []toolContent{{Type: "text", Text: err.Error()}},
				IsError: true,
			},
		}
	}

	return jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result: toolCallResult{
			Content: []toolContent{{Type: "text", Text: string(output)}},
		},
	}
}

func (s *Server) writeResponse(resp jsonRPCResponse) {
	encoded, err := json.Marshal(resp)
	if err != nil {
		s.logger.Error("failed to marshal JSON-RPC response", slog.Any("error", err))
		return
	}
	encoded = append(encoded, '\n')
	if _, err := s.writer.Write(encoded); err != nil {
		s.logger.Error("failed to write JSON-RPC response", slog.Any("error", err))
	}
}

func errorResponse(id json.RawMessage, code int, message string) jsonRPCResponse {
	return jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonRPCError{Code: code, Message: message},
	}
}
