package fakemodel_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/fakemodel"
)

func singleRequired(property, typ string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{%q:{"type":%s}},"required":[%q]}`, property, typ, property))
}

func batchReadSchema(modes, itemRequired string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"operations":{"type":"array","items":{"type":"object","properties":{"mode":{"type":"string","enum":[%s]},"path":{"type":"string"}},"required":[%s]}}},"required":["operations"]}`, modes, itemRequired))
}

func commandWrite(commands string) []fakemodel.Tool {
	return []fakemodel.Tool{{Name: "edit", Parameters: json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"command":{"type":"string","enum":[%s]},"path":{"type":"string"},"content":{"type":"string"}},"required":["command","path"]}`, commands))}}
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
		{
			"single string path wins over an earlier batch read",
			[]fakemodel.Tool{
				{Name: "read", Parameters: batchReadSchema(`"Line","Directory","Image"`, `"mode"`)},
				{Name: "view", Parameters: singleRequired("path", `"string"`)},
			},
			"view", "path",
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

func TestReadFileBatch(t *testing.T) {
	t.Parallel()

	const path = "/workspace/nonce.txt"
	declared := []fakemodel.Tool{{Name: "read", Parameters: batchReadSchema(`"Line","Directory","Image"`, `"mode"`)}}
	want := decodeObject(t, []byte(`{"operations":[{"mode":"Line","path":"`+path+`"}]}`))

	call, err := fakemodel.ReadFile(path)(declared)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v, want nil", path, err)
	}

	if got := decodeObject(t, call.Arguments); call.Name != "read" || !reflect.DeepEqual(got, want) {
		t.Errorf("ReadFile(%q) = (%q, %s), want (%q, %v)", path, call.Name, call.Arguments, "read", want)
	}
}

func TestReadFileRejectsIneligibleTools(t *testing.T) {
	t.Parallel()

	const wantMessage = "no declared tool requires exactly one string property named file_path, filePath, path or absolute_path, or one array property whose items require only a string mode offering Line and declare one such path property"

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
		{"batch read without the Line mode", []fakemodel.Tool{{Name: "read", Parameters: batchReadSchema(`"Directory","Image"`, `"mode"`)}}},
		{"batch read items require a second property", []fakemodel.Tool{{Name: "read", Parameters: batchReadSchema(`"Line","Directory"`, `"mode","path"`)}}},
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

func TestWriteFile(t *testing.T) {
	t.Parallel()

	const (
		path        = "/workspace/nonce.txt"
		content     = "hello from sortie"
		wantMessage = "no declared tool requires exactly two string properties, one named file_path, filePath, path or absolute_path and one named content or file_text, or such a path property and a command offering create beside one content or file_text property"
	)
	write := func(first, second, secondType string) []fakemodel.Tool {
		schema := fmt.Sprintf(`{"type":"object","properties":{%q:{"type":"string"},%q:{"type":%s}},"required":[%q,%q]}`, first, second, secondType, first, second)
		return []fakemodel.Tool{{Name: "write", Parameters: json.RawMessage(schema)}}
	}

	tests := []struct {
		name     string
		declared []fakemodel.Tool
		want     map[string]string
	}{
		{"file_path and content", write("file_path", "content", `"string"`), map[string]string{"file_path": path, "content": content}},
		{"file_text content", []fakemodel.Tool{{Name: "create", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"file_text":{"type":"string"}},"required":["path","file_text"]}`)}}, map[string]string{"path": path, "file_text": content}},
		{"path alone", []fakemodel.Tool{{Name: "read", Parameters: singleRequired("path", `"string"`)}}, nil},
		{"content is not a string", write("path", "content", `"array"`), nil},
		{"command write", commandWrite(`"create","str_replace"`), map[string]string{"command": "create", "path": path, "content": content}},
		{"path and content wins over an earlier command write", slices.Concat(commandWrite(`"create"`), write("path", "content", `"string"`)), map[string]string{"path": path, "content": content}},
		{"command offers no create", commandWrite(`"str_replace","insert"`), nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			call, err := fakemodel.WriteFile(path, content)(tt.declared)

			if tt.want == nil {
				if err == nil {
					t.Fatalf("WriteFile(%q, %q)(%s) = %+v, want an error", path, content, tt.name, call)
				}
				if err.Error() != wantMessage {
					t.Errorf("WriteFile(%q, %q)(%s) error = %q, want %q", path, content, tt.name, err.Error(), wantMessage)
				}
				return
			}
			if err != nil {
				t.Fatalf("WriteFile(%q, %q)(%s) error = %v, want nil", path, content, tt.name, err)
			}
			if got := decodeArguments(t, call); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("WriteFile(%q, %q)(%s).Arguments = %v, want %v", path, content, tt.name, got, tt.want)
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
		wantFilled   string
		parameters   json.RawMessage
	}{
		{"cmd", "cmd", "/workspace/nonce.txt", "cmd", `'cat' '/workspace/nonce.txt'`, "", nil},
		{"command", "command", "/workspace/nonce.txt", "command", `'cat' '/workspace/nonce.txt'`, "", nil},
		{"space in path", "cmd", "/workspace/my file.txt", "cmd", `'cat' '/workspace/my file.txt'`, "", nil},
		{"quote in path", "command", "/workspace/it's.txt", "command", `'cat' '/workspace/it'\''s.txt'`, "", nil},
		{"shell metacharacters", "cmd", "/workspace/$HOME;rm.txt", "cmd", `'cat' '/workspace/$HOME;rm.txt'`, "", nil},
		{
			"required description", "", "/workspace/nonce.txt", "command", `'cat' '/workspace/nonce.txt'`, "description",
			json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"description":{"type":"string"},"mode":{"type":"string"}},"required":["command","description"]}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			parameters := tt.parameters
			if parameters == nil {
				parameters = singleRequired(tt.property, `"string"`)
			}
			declared := []fakemodel.Tool{{Name: "exec", Parameters: parameters}}

			call, err := fakemodel.CatFile(tt.path)(declared)
			if err != nil {
				t.Fatalf("CatFile(%q)(%s) error = %v, want nil", tt.path, tt.name, err)
			}

			if call.Name != "exec" {
				t.Errorf("CatFile(%q)(%s).Name = %q, want %q", tt.path, tt.name, call.Name, "exec")
			}
			want := map[string]string{tt.wantProperty: tt.wantCommand}
			got := decodeArguments(t, call)
			if tt.wantFilled != "" && got[tt.wantFilled] == "" {
				t.Errorf("CatFile(%q)(%s).Arguments = %v, want a non-empty %q", tt.path, tt.name, got, tt.wantFilled)
			}
			delete(got, tt.wantFilled)
			if !reflect.DeepEqual(got, want) {
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
