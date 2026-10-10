package fakemodel

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/sshutil"
)

// ReadFile returns a [ToolChoice] that reads path through the first declared
// tool requiring exactly one string property named file_path, filePath, path
// or absolute_path, in declaration order. The arguments carry path under that
// property's own name.
func ReadFile(path string) ToolChoice {
	return func(declared []Tool) (FunctionCall, error) {
		tool, property, ok := findSingleStringTool(declared, "filepath", "path", "absolutepath")
		if !ok {
			return FunctionCall{}, errors.New("no declared tool requires exactly one string property named file_path, filePath, path or absolute_path")
		}
		return newCall(tool, property, path), nil
	}
}

// CatFile returns a [ToolChoice] that prints path through the first declared
// tool requiring exactly one string property named cmd or command, in
// declaration order. The property value is a quoted cat command line.
func CatFile(path string) ToolChoice {
	return func(declared []Tool) (FunctionCall, error) {
		tool, property, ok := findSingleStringTool(declared, "cmd", "command")
		if !ok {
			return FunctionCall{}, errors.New("no declared tool requires exactly one string property named cmd or command")
		}
		return newCall(tool, property, sshutil.QuoteArgv([]string{"cat", path})), nil
	}
}

// Named returns a [ToolChoice] that calls the declared tool named exactly
// name with arguments, which must be a JSON object. It fails, naming name,
// when no declared tool has that name or arguments is not an object.
func Named(name string, arguments json.RawMessage) ToolChoice {
	return func(declared []Tool) (FunctionCall, error) {
		if !isJSONObject(arguments) {
			return FunctionCall{}, fmt.Errorf("arguments for tool %q are not a JSON object", name)
		}
		for _, tool := range declared {
			if tool.Name == name {
				return FunctionCall{Name: name, Arguments: arguments}, nil
			}
		}
		return FunctionCall{}, fmt.Errorf("no declared tool is named %q", name)
	}
}

func newCall(tool Tool, property, value string) FunctionCall {
	return FunctionCall{Name: tool.Name, Arguments: encodeJSON(map[string]string{property: value})}
}

// findSingleStringTool returns the first tool whose schema requires exactly
// one property, of type string, whose name lowercased with underscores and
// hyphens removed is one of names, along with that property's name.
func findSingleStringTool(declared []Tool, names ...string) (Tool, string, bool) {
	for _, tool := range declared {
		property, ok := singleRequiredString(tool.Parameters)
		if ok && slices.Contains(names, normalizePropertyName(property)) {
			return tool, property, true
		}
	}
	return Tool{}, "", false
}

func singleRequiredString(parameters json.RawMessage) (string, bool) {
	var schema struct {
		Required   []string `json:"required"`
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if json.Unmarshal(parameters, &schema) != nil || len(schema.Required) != 1 {
		return "", false
	}
	property := schema.Required[0]
	return property, strings.EqualFold(schema.Properties[property].Type, "string")
}

func normalizePropertyName(name string) string {
	return strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(name))
}

// serverTool returns a [ToolChoice] that calls, with no arguments, the first
// declared tool named name or ending in name after a separator: a runtime
// prefixes a tool an MCP server serves with that server's name.
func serverTool(name string) ToolChoice {
	return func(declared []Tool) (FunctionCall, error) {
		for _, tool := range declared {
			prefix, found := strings.CutSuffix(tool.Name, name)
			if found && (prefix == "" || strings.ContainsAny(prefix[len(prefix)-1:], "_-.:/")) {
				return FunctionCall{Name: tool.Name, Namespace: tool.Namespace, Arguments: json.RawMessage(`{}`)}, nil
			}
		}
		return FunctionCall{}, fmt.Errorf("no declared tool is named %q or ends in it after a separator", name)
	}
}
