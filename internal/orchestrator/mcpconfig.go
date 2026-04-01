package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// MCPConfigParams holds the inputs for [GenerateMCPConfig].
type MCPConfigParams struct {
	// BinaryPath is the absolute path to the sortie binary.
	BinaryPath string

	// WorkflowPath is the absolute path to the WORKFLOW.md file.
	WorkflowPath string

	// WorkspacePath is the absolute path to the workspace directory.
	WorkspacePath string

	// IssueID is the tracker-internal issue ID.
	IssueID string

	// Identifier is the human-readable issue key.
	Identifier string

	// DBPath is the absolute path to the SQLite database.
	DBPath string

	// SessionID is the agent session identifier (may be empty).
	SessionID string

	// OperatorMCPConfigPath is the path to the operator-provided MCP
	// config file. Empty when no operator config is specified.
	OperatorMCPConfigPath string
}

// GenerateMCPConfig creates the merged MCP config file for the workspace
// and returns the absolute path to the written file.
//
// If OperatorMCPConfigPath is non-empty, the operator's config is
// read, parsed, and merged with the sortie-tools entry. Returns an
// error if the operator's config is unreadable, contains invalid
// JSON, or contains a server named "sortie-tools" (name collision).
func GenerateMCPConfig(params MCPConfigParams) (string, error) {
	entry := map[string]any{
		"type":    "stdio",
		"command": params.BinaryPath,
		// WorkflowPath is an absolute path supplied by the orchestrator at
		// workspace allocation time. The agent runtime already operates within
		// the workspace directory and has full access to the filesystem, so
		// passing the absolute workflow path here does not expand its access.
		"args": []string{"mcp-server", "--workflow", params.WorkflowPath},
		"env": map[string]string{
			"SORTIE_ISSUE_ID":         params.IssueID,
			"SORTIE_ISSUE_IDENTIFIER": params.Identifier,
			"SORTIE_WORKSPACE":        params.WorkspacePath,
			"SORTIE_DB_PATH":          params.DBPath,
			"SORTIE_SESSION_ID":       params.SessionID,
		},
	}

	var merged map[string]any

	if params.OperatorMCPConfigPath == "" {
		merged = map[string]any{
			"mcpServers": map[string]any{
				"sortie-tools": entry,
			},
		}
	} else {
		data, err := os.ReadFile(params.OperatorMCPConfigPath)
		if err != nil {
			return "", fmt.Errorf("reading operator MCP config: %w", err)
		}

		var parsed map[string]any
		if err := json.Unmarshal(data, &parsed); err != nil {
			return "", fmt.Errorf("parsing operator MCP config: %w", err)
		}

		var servers map[string]any
		raw, hasServers := parsed["mcpServers"]
		if hasServers {
			var ok bool
			servers, ok = raw.(map[string]any)
			if !ok {
				return "", fmt.Errorf("operator MCP config: 'mcpServers' is not an object")
			}
		} else {
			servers = make(map[string]any)
		}

		if _, exists := servers["sortie-tools"]; exists {
			return "", fmt.Errorf("operator MCP config contains reserved server name %q", "sortie-tools")
		}

		servers["sortie-tools"] = entry
		parsed["mcpServers"] = servers
		merged = parsed
	}

	dir := filepath.Join(params.WorkspacePath, ".sortie")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("creating .sortie directory: %w", err)
	}

	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshalling MCP config: %w", err)
	}

	tmpPath := filepath.Join(dir, "mcp.json.tmp")
	outPath := filepath.Join(dir, "mcp.json")

	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return "", fmt.Errorf("writing MCP config temp file: %w", err)
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		return "", fmt.Errorf("renaming MCP config file: %w", err)
	}

	return outPath, nil
}
