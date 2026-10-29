package fakemodel

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/sshutil"
)

// pathPropertyNames are the normalized names a tool's file path property may
// carry.
var pathPropertyNames = []string{"filepath", "path", "absolutepath"}

var contentPropertyNames = []string{"content", "filetext"}

var commandPropertyNames = []string{"cmd", "command"}

// catDescription satisfies a shell tool's required description property.
const catDescription = "Print the file"

// ReadFile returns a [ToolChoice] that reads path through the first declared
// tool requiring exactly one string property named file_path, filePath, path
// or absolute_path, in declaration order. The arguments carry path under that
// property's own name.
func ReadFile(path string) ToolChoice {
	return func(declared []Tool) (FunctionCall, error) {
		tool, property, ok := findSingleStringTool(declared, pathPropertyNames...)
		if !ok {
			return FunctionCall{}, errors.New("no declared tool requires exactly one string property named file_path, filePath, path or absolute_path")
		}
		return newCall(tool, property, path), nil
	}
}

// WriteFile returns a [ToolChoice] that writes content to path through the
// first declared tool requiring exactly two string properties, one named
// file_path, filePath, path or absolute_path and one named content or
// file_text, in declaration order. The arguments carry path and content under
// those properties' own names.
func WriteFile(path, content string) ToolChoice {
	return func(declared []Tool) (FunctionCall, error) {
		for _, tool := range declared {
			pathProperty, contentProperty, ok := pathAndContentProperties(tool.Parameters)
			if ok {
				return FunctionCall{Name: tool.Name, Arguments: encodeJSON(map[string]string{pathProperty: path, contentProperty: content})}, nil
			}
		}
		return FunctionCall{}, errors.New("no declared tool requires exactly two string properties, one named file_path, filePath, path or absolute_path and one named content or file_text")
	}
}

// CatFile returns a [ToolChoice] that prints path through the first declared
// tool requiring exactly one string property named cmd or command, or that and
// one named description, in declaration order. The command value is a quoted
// cat command line.
func CatFile(path string) ToolChoice {
	return func(declared []Tool) (FunctionCall, error) {
		for _, tool := range declared {
			commandProperty, descriptionProperty, ok := commandProperties(tool.Parameters)
			if !ok {
				continue
			}
			arguments := map[string]string{commandProperty: sshutil.QuoteArgv([]string{"cat", path})}
			if descriptionProperty != "" {
				arguments[descriptionProperty] = catDescription
			}
			return FunctionCall{Name: tool.Name, Arguments: encodeJSON(arguments)}, nil
		}
		return FunctionCall{}, errors.New("no declared tool requires exactly one string property named cmd or command, or exactly two string properties, one named cmd or command and one named description")
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

type requiredProperties struct {
	Required   []string `json:"required"`
	Properties map[string]struct {
		Type string `json:"type"`
	} `json:"properties"`
}

func (p requiredProperties) isString(property string) bool {
	return strings.EqualFold(p.Properties[property].Type, "string")
}

func singleRequiredString(parameters json.RawMessage) (string, bool) {
	var schema requiredProperties
	if json.Unmarshal(parameters, &schema) != nil || len(schema.Required) != 1 {
		return "", false
	}
	property := schema.Required[0]
	return property, schema.isString(property)
}

// pathAndContentProperties returns the names of the file path property and
// the content property of a schema requiring exactly those two string
// properties.
func pathAndContentProperties(parameters json.RawMessage) (pathProperty, contentProperty string, ok bool) {
	var schema requiredProperties
	if json.Unmarshal(parameters, &schema) != nil || len(schema.Required) != 2 {
		return "", "", false
	}
	for _, property := range schema.Required {
		if !schema.isString(property) {
			return "", "", false
		}
		switch name := normalizePropertyName(property); {
		case slices.Contains(contentPropertyNames, name):
			contentProperty = property
		case slices.Contains(pathPropertyNames, name):
			pathProperty = property
		}
	}
	return pathProperty, contentProperty, pathProperty != "" && contentProperty != ""
}

// commandProperties returns the command property and, when the schema requires
// one, the description property; it requires nothing else.
func commandProperties(parameters json.RawMessage) (commandProperty, descriptionProperty string, ok bool) {
	var schema requiredProperties
	if json.Unmarshal(parameters, &schema) != nil || len(schema.Required) < 1 || len(schema.Required) > 2 {
		return "", "", false
	}
	for _, property := range schema.Required {
		if !schema.isString(property) {
			return "", "", false
		}
		switch name := normalizePropertyName(property); {
		case slices.Contains(commandPropertyNames, name):
			commandProperty = property
		case name == "description":
			descriptionProperty = property
		default:
			return "", "", false
		}
	}
	if len(schema.Required) == 2 && descriptionProperty == "" {
		return "", "", false
	}
	return commandProperty, descriptionProperty, commandProperty != ""
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
