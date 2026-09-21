//go:build unix

package probe

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

type jsonRPCEnvelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
}

// runMCPToolServer's wire shape follows clientprotocol's mcpHandshakeScript,
// ported from the Agent Client Protocol to the Model Context Protocol.
func runMCPToolServer(_ []string, params mcpToolServerParams) int {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Bytes()
		var req jsonRPCEnvelope
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		var err error
		switch req.Method {
		case "initialize":
			err = respond(`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":%q,"version":"0.0.0"}}}`, req.ID, toolServerName)
		case "tools/list":
			err = respond(`{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":%q,"description":"records that the qualification probe induced a call","inputSchema":{"type":"object","properties":{}}}]}}`, req.ID, probeToolName)
		case "tools/call":
			if recordErr := recordToolCall(params.RecordPath, line); recordErr != nil {
				fmt.Fprintf(os.Stderr, "record tool call: %v\n", recordErr)
				return 1
			}
			err = respond(`{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"ok"}]}}`, req.ID)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "respond to %s: %v\n", req.Method, err)
			return 1
		}
	}
	return 0
}

func respond(format string, args ...any) error {
	_, err := fmt.Fprintf(os.Stdout, format+"\n", args...)
	return err
}

func recordToolCall(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // path is under the caller's own t.TempDir()
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // best-effort close after a completed write

	if _, err := f.Write(line); err != nil {
		return err
	}
	_, err = f.WriteString("\n")
	return err
}
