package fakemodel_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/fakemodel"
)

func singleRequired(property, typ string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{%q:{"type":%s}},"required":[%q]}`, property, typ, property))
}

func decodeArguments(t testing.TB, call fakemodel.FunctionCall) map[string]string {
	t.Helper()
	var arguments map[string]string
	if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
		t.Fatalf("Arguments = %q, want a JSON object of strings: %v", call.Arguments, err)
	}
	return arguments
}

func TestReadFile(t *testing.T) {
	t.Parallel()

	const path = "/workspace/nonce.txt"
	otherRequired := json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`)

	tests := []struct {
		name         string
		declared     []fakemodel.Tool
		wantTool     string
		wantProperty string
	}{
		{"file_path", []fakemodel.Tool{{Name: "read", Parameters: singleRequired("file_path", `"string"`)}}, "read", "file_path"},
		{"filePath", []fakemodel.Tool{{Name: "read", Parameters: singleRequired("filePath", `"string"`)}}, "read", "filePath"},
		{"path", []fakemodel.Tool{{Name: "view", Parameters: singleRequired("path", `"string"`)}}, "view", "path"},
		{"absolute_path", []fakemodel.Tool{{Name: "read", Parameters: singleRequired("absolute_path", `"string"`)}}, "read", "absolute_path"},
		{"absolutePath", []fakemodel.Tool{{Name: "read", Parameters: singleRequired("absolutePath", `"string"`)}}, "read", "absolutePath"},
		{"hyphenated file-path", []fakemodel.Tool{{Name: "read", Parameters: singleRequired("file-path", `"string"`)}}, "read", "file-path"},
		{"capitalized Path", []fakemodel.Tool{{Name: "read", Parameters: singleRequired("Path", `"string"`)}}, "read", "Path"},
		{"upper case type", []fakemodel.Tool{{Name: "read", Parameters: singleRequired("path", `"STRING"`)}}, "read", "path"},
		{
			"optional extra property",
			[]fakemodel.Tool{{Name: "view", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"view_range":{"type":"array"}},"required":["path"]}`)}},
			"view", "path",
		},
		{
			"first declared wins",
			[]fakemodel.Tool{
				{Name: "search", Parameters: otherRequired},
				{Name: "first", Parameters: singleRequired("path", `"string"`)},
				{Name: "second", Parameters: singleRequired("file_path", `"string"`)},
			},
			"first", "path",
		},
		{
			"skips an ineligible tool declared before it",
			[]fakemodel.Tool{
				{Name: "count", Parameters: singleRequired("path", `"integer"`)},
				{Name: "read", Parameters: singleRequired("filePath", `"string"`)},
			},
			"read", "filePath",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			call, err := fakemodel.ReadFile(path)(tt.declared)
			if err != nil {
				t.Fatalf("ReadFile(%q)(%s) error = %v, want nil", path, tt.name, err)
			}

			if call.Name != tt.wantTool {
				t.Errorf("ReadFile(%q)(%s).Name = %q, want %q", path, tt.name, call.Name, tt.wantTool)
			}
			want := map[string]string{tt.wantProperty: path}
			if got := decodeArguments(t, call); !reflect.DeepEqual(got, want) {
				t.Errorf("ReadFile(%q)(%s).Arguments = %v, want %v", path, tt.name, got, want)
			}
		})
	}
}

func TestReadFileRejectsIneligibleTools(t *testing.T) {
	t.Parallel()

	const wantMessage = "no declared tool requires exactly one string property named file_path, filePath, path or absolute_path"

	tests := []struct {
		name     string
		declared []fakemodel.Tool
	}{
		{"no tools", nil},
		{"no parameters", []fakemodel.Tool{{Name: "read"}}},
		{
			"two required properties",
			[]fakemodel.Tool{{Name: "read", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"mode":{"type":"string"}},"required":["path","mode"]}`)}},
		},
		{"non-string type", []fakemodel.Tool{{Name: "read", Parameters: singleRequired("path", `"integer"`)}}},
		{"type array", []fakemodel.Tool{{Name: "read", Parameters: singleRequired("path", `["string","null"]`)}}},
		{
			"optional only",
			[]fakemodel.Tool{{Name: "read", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}},
		},
		{"other property name", []fakemodel.Tool{{Name: "search", Parameters: singleRequired("query", `"string"`)}}},
		{"a command property", []fakemodel.Tool{{Name: "run", Parameters: singleRequired("cmd", `"string"`)}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			call, err := fakemodel.ReadFile("/workspace/nonce.txt")(tt.declared)

			if err == nil {
				t.Fatalf("ReadFile(...)(%s) = %+v, want an error", tt.name, call)
			}
			if err.Error() != wantMessage {
				t.Errorf("ReadFile(...)(%s) error = %q, want %q", tt.name, err.Error(), wantMessage)
			}
		})
	}
}

func TestCatFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		property     string
		path         string
		wantProperty string
		wantCommand  string
	}{
		{"cmd", "cmd", "/workspace/nonce.txt", "cmd", `'cat' '/workspace/nonce.txt'`},
		{"command", "command", "/workspace/nonce.txt", "command", `'cat' '/workspace/nonce.txt'`},
		{"space in path", "cmd", "/workspace/my file.txt", "cmd", `'cat' '/workspace/my file.txt'`},
		{"quote in path", "command", "/workspace/it's.txt", "command", `'cat' '/workspace/it'\''s.txt'`},
		{"shell metacharacters", "cmd", "/workspace/$HOME;rm.txt", "cmd", `'cat' '/workspace/$HOME;rm.txt'`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			declared := []fakemodel.Tool{{Name: "exec", Parameters: singleRequired(tt.property, `"string"`)}}

			call, err := fakemodel.CatFile(tt.path)(declared)
			if err != nil {
				t.Fatalf("CatFile(%q)(%s) error = %v, want nil", tt.path, tt.name, err)
			}

			if call.Name != "exec" {
				t.Errorf("CatFile(%q)(%s).Name = %q, want %q", tt.path, tt.name, call.Name, "exec")
			}
			want := map[string]string{tt.wantProperty: tt.wantCommand}
			if got := decodeArguments(t, call); !reflect.DeepEqual(got, want) {
				t.Errorf("CatFile(%q)(%s).Arguments = %v, want %v", tt.path, tt.name, got, want)
			}
		})
	}
}

func TestCatFileRejectsIneligibleTools(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		declared []fakemodel.Tool
	}{
		{"no tools", nil},
		{"a path property", []fakemodel.Tool{{Name: "read", Parameters: singleRequired("path", `"string"`)}}},
		{"non-string type", []fakemodel.Tool{{Name: "exec", Parameters: singleRequired("cmd", `"array"`)}}},
		{"type array", []fakemodel.Tool{{Name: "exec", Parameters: singleRequired("cmd", `["string","null"]`)}}},
		{
			"two required properties",
			[]fakemodel.Tool{{Name: "exec", Parameters: json.RawMessage(`{"type":"object","properties":{"cmd":{"type":"string"},"cwd":{"type":"string"}},"required":["cmd","cwd"]}`)}},
		},
		{
			"optional only",
			[]fakemodel.Tool{{Name: "exec", Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			call, err := fakemodel.CatFile("/workspace/nonce.txt")(tt.declared)

			if err == nil {
				t.Fatalf("CatFile(...)(%s) = %+v, want an error", tt.name, call)
			}
			for _, property := range []string{"cmd", "command"} {
				if !strings.Contains(err.Error(), property) {
					t.Errorf("CatFile(...)(%s) error = %q, want it to name %q", tt.name, err.Error(), property)
				}
			}
		})
	}
}

func TestNamed(t *testing.T) {
	t.Parallel()

	declared := []fakemodel.Tool{
		{Name: "Read", Parameters: singleRequired("file_path", `"string"`)},
		{Name: "read", Parameters: singleRequired("path", `"string"`)},
	}
	arguments := json.RawMessage(`{"path": "/workspace/nonce.txt"}`)

	call, err := fakemodel.Named("read", arguments)(declared)
	if err != nil {
		t.Fatalf("Named(%q, %s)(...) error = %v, want nil", "read", arguments, err)
	}

	if call.Name != "read" {
		t.Errorf("Named(%q, %s)(...).Name = %q, want %q", "read", arguments, call.Name, "read")
	}
	if string(call.Arguments) != string(arguments) {
		t.Errorf("Named(%q, %s)(...).Arguments = %s, want %s", "read", arguments, call.Arguments, arguments)
	}
}

func TestNamedErrors(t *testing.T) {
	t.Parallel()

	declared := []fakemodel.Tool{{Name: "read", Parameters: singleRequired("path", `"string"`)}}

	tests := []struct {
		name      string
		tool      string
		arguments string
	}{
		{"no such tool", "write", `{}`},
		{"tool name differs in case", "Read", `{}`},
		{"array arguments", "read", `[]`},
		{"string arguments", "read", `"path"`},
		{"null arguments", "read", `null`},
		{"empty arguments", "read", ``},
		{"malformed arguments", "read", `{"path":`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			call, err := fakemodel.Named(tt.tool, json.RawMessage(tt.arguments))(declared)

			if err == nil {
				t.Fatalf("Named(%q, %q)(...) = %+v, want an error", tt.tool, tt.arguments, call)
			}
			if !strings.Contains(err.Error(), tt.tool) {
				t.Errorf("Named(%q, %q)(...) error = %q, want it to name %q", tt.tool, tt.arguments, err.Error(), tt.tool)
			}
		})
	}
}
