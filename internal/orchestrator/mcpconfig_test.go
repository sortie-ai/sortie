package orchestrator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// mcpParams returns a valid MCPConfigParams with the given workspace path.
func mcpParams(workspacePath string) MCPConfigParams {
	return MCPConfigParams{
		BinaryPath:    "/usr/local/bin/sortie",
		WorkflowPath:  "/srv/workflow/WORKFLOW.md",
		WorkspacePath: workspacePath,
		IssueID:       "issue-42",
		Identifier:    "PROJ-42",
		DBPath:        "/var/db/sortie.sqlite",
		SessionID:     "",
	}
}

// readMCPConfig reads and parses the mcp.json from the workspace dir.
func readMCPConfig(t *testing.T, workspacePath string) map[string]any {
	t.Helper()
	path := filepath.Join(workspacePath, ".sortie", "mcp.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("Unmarshal mcp.json: %v", err)
	}
	return m
}

// sortieEntry extracts the "sortie-tools" entry from a parsed mcp.json.
func sortieEntry(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	servers, ok := m["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers is not an object: %v", m["mcpServers"])
	}
	entry, ok := servers["sortie-tools"].(map[string]any)
	if !ok {
		t.Fatalf("sortie-tools entry is not an object: %v", servers["sortie-tools"])
	}
	return entry
}

func TestGenerateMCPConfig(t *testing.T) {
	t.Parallel()

	t.Run("returns_correct_output_path", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		out, err := GenerateMCPConfig(mcpParams(dir))
		if err != nil {
			t.Fatalf("GenerateMCPConfig: %v", err)
		}
		want := filepath.Join(dir, ".sortie", "mcp.json")
		if out != want {
			t.Errorf("GenerateMCPConfig() = %q, want %q", out, want)
		}
	})

	t.Run("sortie_tools_entry_type_and_command", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		_, err := GenerateMCPConfig(mcpParams(dir))
		if err != nil {
			t.Fatalf("GenerateMCPConfig: %v", err)
		}

		entry := sortieEntry(t, readMCPConfig(t, dir))

		if entry["type"] != "stdio" {
			t.Errorf("type = %q, want %q", entry["type"], "stdio")
		}
		if entry["command"] != "/usr/local/bin/sortie" {
			t.Errorf("command = %q, want %q", entry["command"], "/usr/local/bin/sortie")
		}
	})

	t.Run("sortie_tools_entry_args", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		p := mcpParams(dir)
		_, err := GenerateMCPConfig(p)
		if err != nil {
			t.Fatalf("GenerateMCPConfig: %v", err)
		}

		entry := sortieEntry(t, readMCPConfig(t, dir))
		rawArgs, ok := entry["args"].([]any)
		if !ok {
			t.Fatalf("args is not an array: %v", entry["args"])
		}
		wantArgs := []string{"mcp-server", "--workflow", p.WorkflowPath}
		if len(rawArgs) != len(wantArgs) {
			t.Fatalf("args length = %d, want %d: %v", len(rawArgs), len(wantArgs), rawArgs)
		}
		for i, w := range wantArgs {
			if rawArgs[i] != w {
				t.Errorf("args[%d] = %q, want %q", i, rawArgs[i], w)
			}
		}
	})

	t.Run("sortie_tools_entry_env_vars", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		p := mcpParams(dir)
		_, err := GenerateMCPConfig(p)
		if err != nil {
			t.Fatalf("GenerateMCPConfig: %v", err)
		}

		entry := sortieEntry(t, readMCPConfig(t, dir))
		env, ok := entry["env"].(map[string]any)
		if !ok {
			t.Fatalf("env is not an object: %v", entry["env"])
		}

		checks := map[string]string{
			"SORTIE_ISSUE_ID":         p.IssueID,
			"SORTIE_ISSUE_IDENTIFIER": p.Identifier,
			"SORTIE_WORKSPACE":        p.WorkspacePath,
			"SORTIE_DB_PATH":          p.DBPath,
			"SORTIE_SESSION_ID":       p.SessionID,
		}
		for k, want := range checks {
			if got, ok := env[k].(string); !ok || got != want {
				t.Errorf("env[%q] = %q, want %q", k, env[k], want)
			}
		}
		if len(env) != 5 {
			t.Errorf("env key count = %d, want 5: %v", len(env), env)
		}
	})

	t.Run("file_created_at_expected_path", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		_, err := GenerateMCPConfig(mcpParams(dir))
		if err != nil {
			t.Fatalf("GenerateMCPConfig: %v", err)
		}
		path := filepath.Join(dir, ".sortie", "mcp.json")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("mcp.json not found at %q: %v", path, err)
		}
	})

	t.Run("tmp_file_removed_after_atomic_write", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		_, err := GenerateMCPConfig(mcpParams(dir))
		if err != nil {
			t.Fatalf("GenerateMCPConfig: %v", err)
		}
		tmpPath := filepath.Join(dir, ".sortie", "mcp.json.tmp")
		if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
			t.Errorf("tmp file %q still exists after atomic write", tmpPath)
		}
	})

	t.Run("merge_with_operator_config", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		// Write operator config with a separate server entry.
		operatorConfig := map[string]any{
			"mcpServers": map[string]any{
				"operator-service": map[string]any{
					"type":    "stdio",
					"command": "/usr/bin/op-tool",
				},
			},
		}
		operatorPath := filepath.Join(dir, "operator-mcp.json")
		data, _ := json.Marshal(operatorConfig)
		if err := os.WriteFile(operatorPath, data, 0o644); err != nil {
			t.Fatalf("WriteFile operator config: %v", err)
		}

		p := mcpParams(dir)
		p.OperatorMCPConfigPath = operatorPath

		_, err := GenerateMCPConfig(p)
		if err != nil {
			t.Fatalf("GenerateMCPConfig: %v", err)
		}

		merged := readMCPConfig(t, dir)
		servers, ok := merged["mcpServers"].(map[string]any)
		if !ok {
			t.Fatalf("mcpServers is not an object: %v", merged["mcpServers"])
		}

		// Both entries must be present.
		if _, ok := servers["sortie-tools"]; !ok {
			t.Error("sortie-tools missing from merged config")
		}
		if _, ok := servers["operator-service"]; !ok {
			t.Error("operator-service missing from merged config")
		}
	})

	t.Run("merge_operator_without_mcp_servers_key", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		// Operator config has no mcpServers key.
		operatorPath := filepath.Join(dir, "op.json")
		if err := os.WriteFile(operatorPath, []byte(`{"otherKey":"value"}`), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		p := mcpParams(dir)
		p.OperatorMCPConfigPath = operatorPath

		_, err := GenerateMCPConfig(p)
		if err != nil {
			t.Fatalf("GenerateMCPConfig: %v", err)
		}

		merged := readMCPConfig(t, dir)
		// sortie-tools must be present.
		sortieEntry(t, merged)
		// Original key preserved.
		if merged["otherKey"] != "value" {
			t.Errorf("otherKey = %q, want %q", merged["otherKey"], "value")
		}
	})

	t.Run("collision_with_sortie_tools_name_returns_error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		operatorConfig := map[string]any{
			"mcpServers": map[string]any{
				"sortie-tools": map[string]any{"type": "stdio", "command": "/other"},
			},
		}
		operatorPath := filepath.Join(dir, "op.json")
		data, _ := json.Marshal(operatorConfig)
		if err := os.WriteFile(operatorPath, data, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		p := mcpParams(dir)
		p.OperatorMCPConfigPath = operatorPath

		_, err := GenerateMCPConfig(p)
		if err == nil {
			t.Fatal("GenerateMCPConfig() = nil, want error for sortie-tools name collision")
		}
	})

	t.Run("operator_unreadable_returns_error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		p := mcpParams(dir)
		p.OperatorMCPConfigPath = filepath.Join(dir, "nonexistent.json")

		_, err := GenerateMCPConfig(p)
		if err == nil {
			t.Fatal("GenerateMCPConfig() = nil, want error for unreadable operator config")
		}
	})

	t.Run("operator_invalid_json_returns_error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		operatorPath := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(operatorPath, []byte(`{invalid`), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		p := mcpParams(dir)
		p.OperatorMCPConfigPath = operatorPath

		_, err := GenerateMCPConfig(p)
		if err == nil {
			t.Fatal("GenerateMCPConfig() = nil, want error for invalid operator JSON")
		}
	})

	t.Run("operator_mcp_servers_not_object_returns_error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		operatorPath := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(operatorPath, []byte(`{"mcpServers":"not-an-object"}`), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		p := mcpParams(dir)
		p.OperatorMCPConfigPath = operatorPath

		_, err := GenerateMCPConfig(p)
		if err == nil {
			t.Fatal("GenerateMCPConfig() = nil, want error when mcpServers is not an object")
		}
	})
}
