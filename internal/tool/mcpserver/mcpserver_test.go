package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// --- Test helpers ---

// discardLogger returns a logger that writes nothing.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// buildRequest constructs a newline-terminated JSON-RPC 2.0 request string.
func buildRequest(t *testing.T, method string, id any, params any) string {
	t.Helper()
	type req struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id,omitempty"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}
	b, err := json.Marshal(req{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		t.Fatalf("buildRequest(%q): %v", method, err)
	}
	return string(b) + "\n"
}

// buildNotification constructs a notification (no ID field) JSON-RPC request.
func buildNotification(t *testing.T, method string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	if err != nil {
		t.Fatalf("buildNotification(%q): %v", method, err)
	}
	return string(b) + "\n"
}

// parseResponses splits newline-delimited JSON from data into a slice of maps.
func parseResponses(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var results []map[string]any
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("parseResponses: unmarshal %q: %v", line, err)
		}
		results = append(results, m)
	}
	return results
}

// mustServe runs Serve with the given input lines and returns all parsed responses.
func mustServe(t *testing.T, reg *domain.ToolRegistry, input string, version string) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	srv := NewServer(reg, strings.NewReader(input), &buf, discardLogger(), version)
	if err := srv.Serve(context.Background()); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	return parseResponses(t, buf.Bytes())
}

// errorCode returns the numeric code from a JSON-RPC error response.
func errorCode(t *testing.T, resp map[string]any) float64 {
	t.Helper()
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("response has no error object: %v", resp)
	}
	code, ok := errObj["code"].(float64)
	if !ok {
		t.Fatalf("error.code is not a number: %v", errObj)
	}
	return code
}

// stubTool is a minimal [domain.AgentTool] for mcpserver tests.
type stubTool struct {
	name       string
	desc       string
	schema     json.RawMessage
	executeOut json.RawMessage
	executeErr error
}

func (s *stubTool) Name() string                 { return s.name }
func (s *stubTool) Description() string          { return s.desc }
func (s *stubTool) InputSchema() json.RawMessage { return s.schema }
func (s *stubTool) Execute(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	return s.executeOut, s.executeErr
}

var _ domain.AgentTool = (*stubTool)(nil)

// --- Tests ---

func TestServe_Initialize(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	input := buildRequest(t, "initialize", 1, nil)
	resps := mustServe(t, reg, input, "2.3.0")

	if len(resps) != 1 {
		t.Fatalf("response count = %d, want 1", len(resps))
	}
	resp := resps[0]

	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not an object: %v", resp)
	}

	if got := result["protocolVersion"]; got != protocolVersion {
		t.Errorf("protocolVersion = %q, want %q", got, protocolVersion)
	}

	serverInfo, ok := result["serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("serverInfo is not an object: %v", result)
	}
	if serverInfo["name"] != "sortie-tools" {
		t.Errorf("serverInfo.name = %q, want %q", serverInfo["name"], "sortie-tools")
	}
	if serverInfo["version"] != "2.3.0" {
		t.Errorf("serverInfo.version = %q, want %q", serverInfo["version"], "2.3.0")
	}

	// capabilities.tools must be present.
	caps, ok := result["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities is not an object: %v", result)
	}
	if _, ok := caps["tools"]; !ok {
		t.Error("capabilities.tools is absent, want present")
	}

	if resp["error"] != nil {
		t.Errorf("error field is non-nil: %v", resp["error"])
	}
}

func TestServe_ToolsList_Empty(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	input := buildRequest(t, "tools/list", 2, nil)
	resps := mustServe(t, reg, input, "1.0.0")

	if len(resps) != 1 {
		t.Fatalf("response count = %d, want 1", len(resps))
	}

	result, ok := resps[0]["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not an object: %v", resps[0])
	}
	tools, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("tools is not an array: %v", result)
	}
	if len(tools) != 0 {
		t.Errorf("tools count = %d, want 0", len(tools))
	}
}

func TestServe_ToolsList_WithTools(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	reg.Register(&stubTool{
		name:   "my_tool",
		desc:   "does useful things",
		schema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
	})

	input := buildRequest(t, "tools/list", 3, nil)
	resps := mustServe(t, reg, input, "1.0.0")

	if len(resps) != 1 {
		t.Fatalf("response count = %d, want 1", len(resps))
	}

	result, ok := resps[0]["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not an object: %v", resps[0])
	}
	tools, ok := result["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v, want 1 element", result["tools"])
	}

	tool := tools[0].(map[string]any)
	if tool["name"] != "my_tool" {
		t.Errorf("tools[0].name = %q, want %q", tool["name"], "my_tool")
	}
	if tool["description"] != "does useful things" {
		t.Errorf("tools[0].description = %q, want %q", tool["description"], "does useful things")
	}
	if tool["inputSchema"] == nil {
		t.Error("tools[0].inputSchema is nil, want non-nil")
	}
}

func TestServe_ToolsCall_Success(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	reg.Register(&stubTool{
		name:       "echo",
		schema:     json.RawMessage(`{}`),
		executeOut: json.RawMessage(`{"status":"ok"}`),
	})

	params := map[string]any{
		"name":      "echo",
		"arguments": map[string]any{"input": "hello"},
	}
	input := buildRequest(t, "tools/call", 10, params)
	resps := mustServe(t, reg, input, "1.0.0")

	if len(resps) != 1 {
		t.Fatalf("response count = %d, want 1", len(resps))
	}

	result, ok := resps[0]["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not an object: %v", resps[0])
	}

	// isError must be absent (false/omitempty) on success.
	if isErr, ok := result["isError"].(bool); ok && isErr {
		t.Error("isError = true, want false on success")
	}

	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("content = %v, want non-empty array", result["content"])
	}
	contentItem := content[0].(map[string]any)
	if contentItem["type"] != "text" {
		t.Errorf("content[0].type = %q, want %q", contentItem["type"], "text")
	}
	if contentItem["text"] != `{"status":"ok"}` {
		t.Errorf("content[0].text = %q, want %q", contentItem["text"], `{"status":"ok"}`)
	}
}

func TestServe_ToolsCall_ExecuteError(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	reg.Register(&stubTool{
		name:       "fail_tool",
		schema:     json.RawMessage(`{}`),
		executeErr: &testError{"tool exploded"},
	})

	params := map[string]any{"name": "fail_tool"}
	input := buildRequest(t, "tools/call", 11, params)
	resps := mustServe(t, reg, input, "1.0.0")

	if len(resps) != 1 {
		t.Fatalf("response count = %d, want 1", len(resps))
	}

	resp := resps[0]

	// Execute errors must produce a normal tools/call result with isError:true,
	// not a JSON-RPC error envelope.
	if resp["error"] != nil {
		t.Errorf("error field is non-nil: %v (tool exec errors must use isError, not JSON-RPC error)", resp["error"])
	}

	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not an object: %v", resp)
	}
	if result["isError"] != true {
		t.Errorf("isError = %v, want true", result["isError"])
	}

	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("content = %v, want non-empty", result["content"])
	}
	text := content[0].(map[string]any)["text"]
	if text != "tool exploded" {
		t.Errorf("content[0].text = %q, want %q", text, "tool exploded")
	}
}

func TestServe_ToolsCall_UnknownTool(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	params := map[string]any{"name": "nonexistent"}
	input := buildRequest(t, "tools/call", 12, params)
	resps := mustServe(t, reg, input, "1.0.0")

	if len(resps) != 1 {
		t.Fatalf("response count = %d, want 1", len(resps))
	}

	if got := errorCode(t, resps[0]); got != codeInvalidParams {
		t.Errorf("error.code = %v, want %d (invalid params)", got, codeInvalidParams)
	}
}

func TestServe_ToolsCall_EmptyName(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	params := map[string]any{"name": ""}
	input := buildRequest(t, "tools/call", 13, params)
	resps := mustServe(t, reg, input, "1.0.0")

	if len(resps) != 1 {
		t.Fatalf("response count = %d, want 1", len(resps))
	}

	if got := errorCode(t, resps[0]); got != codeInvalidParams {
		t.Errorf("error.code = %v, want %d (invalid params)", got, codeInvalidParams)
	}
}

func TestServe_ToolsCall_InvalidParamsJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	reg := domain.NewToolRegistry()
	// Send raw malformed params by embedding invalid JSON directly.
	raw := `{"jsonrpc":"2.0","id":14,"method":"tools/call","params":"not-an-object"}` + "\n"
	srv := NewServer(reg, strings.NewReader(raw), &buf, discardLogger(), "1.0.0")
	if err := srv.Serve(context.Background()); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	resps := parseResponses(t, buf.Bytes())

	if len(resps) != 1 {
		t.Fatalf("response count = %d, want 1", len(resps))
	}

	if got := errorCode(t, resps[0]); got != codeInvalidParams {
		t.Errorf("error.code = %v, want %d (invalid params)", got, codeInvalidParams)
	}
}

func TestServe_Notification_Skipped(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	// A notification has no ID field.
	input := buildNotification(t, "notifications/initialized")
	var buf bytes.Buffer
	srv := NewServer(reg, strings.NewReader(input), &buf, discardLogger(), "1.0.0")
	if err := srv.Serve(context.Background()); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	resps := parseResponses(t, buf.Bytes())
	if len(resps) != 0 {
		t.Errorf("response count = %d, want 0 (notifications must be silently ignored)", len(resps))
	}
}

func TestServe_NullID_Skipped(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	// Explicit null ID must be silently skipped.
	raw := `{"jsonrpc":"2.0","id":null,"method":"tools/list"}` + "\n"
	var buf bytes.Buffer
	srv := NewServer(reg, strings.NewReader(raw), &buf, discardLogger(), "1.0.0")
	if err := srv.Serve(context.Background()); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	resps := parseResponses(t, buf.Bytes())
	if len(resps) != 0 {
		t.Errorf("response count = %d, want 0 (null ID must be silently ignored)", len(resps))
	}
}

func TestServe_ParseError(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	input := "this is not json\n"
	resps := mustServe(t, reg, input, "1.0.0")

	if len(resps) != 1 {
		t.Fatalf("response count = %d, want 1", len(resps))
	}

	if got := errorCode(t, resps[0]); got != codeParseError {
		t.Errorf("error.code = %v, want %d (parse error)", got, codeParseError)
	}
}

func TestServe_InvalidRequest_WrongVersion(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	raw := `{"jsonrpc":"1.0","id":20,"method":"tools/list"}` + "\n"
	var buf bytes.Buffer
	srv := NewServer(reg, strings.NewReader(raw), &buf, discardLogger(), "1.0.0")
	if err := srv.Serve(context.Background()); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	resps := parseResponses(t, buf.Bytes())

	if len(resps) != 1 {
		t.Fatalf("response count = %d, want 1", len(resps))
	}

	if got := errorCode(t, resps[0]); got != codeInvalidRequest {
		t.Errorf("error.code = %v, want %d (invalid request)", got, codeInvalidRequest)
	}
}

func TestServe_MethodNotFound(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	input := buildRequest(t, "prompts/list", 21, nil)
	resps := mustServe(t, reg, input, "1.0.0")

	if len(resps) != 1 {
		t.Fatalf("response count = %d, want 1", len(resps))
	}

	if got := errorCode(t, resps[0]); got != codeMethodNotFound {
		t.Errorf("error.code = %v, want %d (method not found)", got, codeMethodNotFound)
	}
}

func TestServe_MultipleMessages(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	reg.Register(&stubTool{
		name:       "greet",
		schema:     json.RawMessage(`{}`),
		executeOut: json.RawMessage(`{"msg":"hello"}`),
	})

	// Three requests in sequence.
	var sb strings.Builder
	sb.WriteString(buildRequest(t, "initialize", 1, nil))
	sb.WriteString(buildRequest(t, "tools/list", 2, nil))
	sb.WriteString(buildRequest(t, "tools/call", 3, map[string]any{"name": "greet"}))

	resps := mustServe(t, reg, sb.String(), "1.0.0")

	if len(resps) != 3 {
		t.Fatalf("response count = %d, want 3", len(resps))
	}

	// Verify responses correspond to the right request IDs.
	for i, wantID := range []float64{1, 2, 3} {
		gotID, ok := resps[i]["id"].(float64)
		if !ok || gotID != wantID {
			t.Errorf("resps[%d].id = %v, want %v", i, resps[i]["id"], wantID)
		}
	}
}

func TestServe_EOF_ReturnsNil(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	var buf bytes.Buffer
	srv := NewServer(reg, strings.NewReader(""), &buf, discardLogger(), "1.0.0")
	if err := srv.Serve(context.Background()); err != nil {
		t.Errorf("Serve() = %v, want nil on empty input (EOF)", err)
	}
}

func TestServe_ContextCancellation(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	// Use a pipe so Serve blocks waiting for bytes; cancel context mid-read.
	pr, pw := io.Pipe()
	var buf bytes.Buffer

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	srv := NewServer(reg, pr, &buf, discardLogger(), "1.0.0")

	go func() {
		done <- srv.Serve(ctx)
	}()

	// Write one valid request, then cancel.
	line := buildRequest(t, "initialize", 1, nil)
	if _, err := pw.Write([]byte(line)); err != nil {
		t.Fatalf("pipe write: %v", err)
	}
	cancel()

	// Close the write end to unblock the scanner, then collect result.
	_ = pw.Close()

	if err := <-done; err != nil {
		t.Errorf("Serve() = %v, want nil on context cancellation", err)
	}
}

func TestNewServer_NilLogger_DoesNotPanic(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	srv := NewServer(reg, strings.NewReader(""), &bytes.Buffer{}, nil, "1.0.0")
	// Should not panic and should use the default logger.
	if err := srv.Serve(context.Background()); err != nil {
		t.Fatalf("Serve: %v", err)
	}
}

// testError is a simple error type for Execute error tests.
type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }
