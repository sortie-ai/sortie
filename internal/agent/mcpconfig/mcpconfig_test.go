package mcpconfig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// --- Test helpers ---

// writeConfig writes content to a "mcp.json" file inside a fresh
// temp directory and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
	return path
}

// mustParseError parses path and fails unless a *[Error] is returned.
func mustParseError(t *testing.T, path string) *Error {
	t.Helper()

	servers, err := Parse(path)
	if err == nil {
		t.Fatalf("Parse(%q) = %v, nil, want an error", path, servers)
	}
	var parseErr *Error
	if !errors.As(err, &parseErr) {
		t.Fatalf("Parse(%q) error type = %T, want *mcpconfig.Error", path, err)
	}
	return parseErr
}

// --- Tests ---

func TestParse_StdioEntry(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `{"mcpServers":{"sortie-tools":{"type":"stdio","command":"/usr/local/bin/sortie","args":["mcp-server"],"env":{"SORTIE_ISSUE_ID":"abc-123"}}}}`)

	servers, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse(%q) error = %v", path, err)
	}
	if len(servers) != 1 {
		t.Fatalf("Parse(%q) returned %d servers, want 1", path, len(servers))
	}

	got := servers[0]
	if got.Name != "sortie-tools" {
		t.Errorf("Server.Name = %q, want %q", got.Name, "sortie-tools")
	}
	if got.Transport != TransportStdio {
		t.Errorf("Server.Transport = %q, want %q", got.Transport, TransportStdio)
	}
	if got.Command != "/usr/local/bin/sortie" {
		t.Errorf("Server.Command = %q, want %q", got.Command, "/usr/local/bin/sortie")
	}
	if len(got.Args) != 1 || got.Args[0] != "mcp-server" {
		t.Errorf("Server.Args = %v, want [mcp-server]", got.Args)
	}
	if got.Env["SORTIE_ISSUE_ID"] != "abc-123" {
		t.Errorf("Server.Env[%q] = %q, want %q", "SORTIE_ISSUE_ID", got.Env["SORTIE_ISSUE_ID"], "abc-123")
	}
	if got.Enabled != nil {
		t.Errorf("Server.Enabled = %v, want nil", *got.Enabled)
	}
}

func TestParse_HTTPEntry(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `{"mcpServers":{"remote-tools":{"type":"http","url":"https://example.invalid/mcp","headers":{"Authorization":"Bearer token"}}}}`)

	servers, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse(%q) error = %v", path, err)
	}
	if len(servers) != 1 {
		t.Fatalf("Parse(%q) returned %d servers, want 1", path, len(servers))
	}

	got := servers[0]
	if got.Name != "remote-tools" {
		t.Errorf("Server.Name = %q, want %q", got.Name, "remote-tools")
	}
	if got.Transport != TransportHTTP {
		t.Errorf("Server.Transport = %q, want %q", got.Transport, TransportHTTP)
	}
	if got.URL != "https://example.invalid/mcp" {
		t.Errorf("Server.URL = %q, want %q", got.URL, "https://example.invalid/mcp")
	}
	if got.Headers["Authorization"] != "Bearer token" {
		t.Errorf("Server.Headers[%q] = %q, want %q", "Authorization", got.Headers["Authorization"], "Bearer token")
	}
}

func TestParse_AbsentMCPServersKey(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `{"otherKey":"value"}`)

	servers, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse(%q) error = %v, want nil", path, err)
	}
	if servers != nil {
		t.Errorf("Parse(%q) = %v, want nil servers", path, servers)
	}
}

func TestParse_MalformedDocument(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `{not valid json`)

	parseErr := mustParseError(t, path)
	if parseErr.Kind != ErrorNotJSON {
		t.Errorf("Error.Kind = %q, want %q", parseErr.Kind, ErrorNotJSON)
	}
}

func TestParse_NullIsNotAnObject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
	}{
		{name: "null document", content: `null`},
		{name: "null mcpServers", content: `{"mcpServers":null}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfig(t, tt.content)
			parseErr := mustParseError(t, path)
			if parseErr.Kind != ErrorNotJSON {
				t.Errorf("Error.Kind = %q, want %q", parseErr.Kind, ErrorNotJSON)
			}
		})
	}
}

func TestParse_EntryNotExpressible(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
	}{
		{
			name:    "neither command nor url",
			content: `{"mcpServers":{"broken":{"type":"stdio"}}}`,
		},
		{
			name:    "both command and url",
			content: `{"mcpServers":{"broken":{"command":"/bin/echo","url":"https://example.invalid"}}}`,
		},
		{
			name:    "stdio type carrying a url",
			content: `{"mcpServers":{"broken":{"type":"stdio","url":"https://example.invalid/mcp"}}}`,
		},
		{
			name:    "http type carrying a command",
			content: `{"mcpServers":{"broken":{"type":"http","command":"/bin/echo"}}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfig(t, tt.content)
			parseErr := mustParseError(t, path)
			if parseErr.Kind != ErrorEntryNotExpressible {
				t.Errorf("Error.Kind = %q, want %q", parseErr.Kind, ErrorEntryNotExpressible)
			}
			if parseErr.Server != "broken" {
				t.Errorf("Error.Server = %q, want %q", parseErr.Server, "broken")
			}
		})
	}
}

func TestParse_UnmodeledKey(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `{"mcpServers":{"sortie-tools":{"command":"/bin/echo","cwd":"/tmp"}}}`)

	parseErr := mustParseError(t, path)
	if parseErr.Kind != ErrorUnmodeledKey {
		t.Errorf("Error.Kind = %q, want %q", parseErr.Kind, ErrorUnmodeledKey)
	}
	if parseErr.Server != "sortie-tools" {
		t.Errorf("Error.Server = %q, want %q", parseErr.Server, "sortie-tools")
	}
	if parseErr.Key != "cwd" {
		t.Errorf("Error.Key = %q, want %q", parseErr.Key, "cwd")
	}
}

func TestParse_EnabledFalseRoundTrips(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `{"mcpServers":{"sortie-tools":{"command":"/bin/echo","enabled":false}}}`)

	servers, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse(%q) error = %v", path, err)
	}
	if len(servers) != 1 {
		t.Fatalf("Parse(%q) returned %d servers, want 1", path, len(servers))
	}
	got := servers[0].Enabled
	if got == nil {
		t.Fatal("Server.Enabled = nil, want a pointer to false")
	}
	if *got != false {
		t.Errorf("Server.Enabled = %v, want false", *got)
	}
}

func TestParse_SortedByName(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `{"mcpServers":{"zeta":{"command":"/bin/z"},"alpha":{"command":"/bin/a"}}}`)

	servers, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse(%q) error = %v", path, err)
	}
	if len(servers) != 2 {
		t.Fatalf("Parse(%q) returned %d servers, want 2", path, len(servers))
	}
	if servers[0].Name != "alpha" || servers[1].Name != "zeta" {
		t.Errorf("Parse(%q) order = [%q, %q], want [alpha, zeta]", path, servers[0].Name, servers[1].Name)
	}
}

func TestParse_UnreadablePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
	}{
		{"empty path", ""},
		{"missing file", filepath.Join(t.TempDir(), "does-not-exist.json")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			parseErr := mustParseError(t, tt.path)
			if parseErr.Kind != ErrorUnreadable {
				t.Errorf("Error.Kind = %q, want %q", parseErr.Kind, ErrorUnreadable)
			}
		})
	}
}
