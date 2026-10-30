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

const (
	lineMode      = "Line"
	createCommand = "create"
)

// catDescription satisfies a shell tool's required description property.
const catDescription = "Print the file"

// ReadFile returns a [ToolChoice] that reads path through the first declared
// tool requiring exactly one string property named file_path, filePath, path
// or absolute_path, in declaration order. The arguments carry path under that
// property's own name.
//
// When no declared tool does, ReadFile falls back to the first tool requiring
// exactly one array property whose items require only a string mode offering
// Line and declare one such path property. The arguments then carry one item
// reading path in Line mode.
func ReadFile(path string) ToolChoice {
	return func(declared []Tool) (FunctionCall, error) {
		if tool, property, ok := findSingleStringTool(declared, pathPropertyNames...); ok {
			return newCall(tool, property, path), nil
		}
		for _, tool := range declared {
			arrayProperty, modeProperty, pathProperty, ok := batchReadProperties(tool.Parameters)
			if ok {
				operations := []map[string]string{{modeProperty: lineMode, pathProperty: path}}
				return FunctionCall{Name: tool.Name, Arguments: encodeJSON(map[string][]map[string]string{arrayProperty: operations})}, nil
			}
		}
		return FunctionCall{}, errors.New("no declared tool requires exactly one string property named file_path, filePath, path or absolute_path, or one array property whose items require only a string mode offering Line and declare one such path property")
	}
}

// WriteFile returns a [ToolChoice] that writes content to path through the
// first declared tool requiring exactly two string properties, one named
// file_path, filePath, path or absolute_path and one named content or
// file_text, in declaration order. The arguments carry path and content under
// those properties' own names.
//
// When no declared tool does, WriteFile falls back to the first tool requiring
// such a path property and a command offering create, and declaring one content
// or file_text property beside them. The arguments then carry the create
// command, path and content.
func WriteFile(path, content string) ToolChoice {
	return func(declared []Tool) (FunctionCall, error) {
		for _, tool := range declared {
			pathProperty, contentProperty, ok := pathAndContentProperties(tool.Parameters)
			if ok {
				return FunctionCall{Name: tool.Name, Arguments: encodeJSON(map[string]string{pathProperty: path, contentProperty: content})}, nil
			}
		}
		for _, tool := range declared {
			pathProperty, commandProperty, contentProperty, ok := commandWriteProperties(tool.Parameters)
			if ok {
				return FunctionCall{Name: tool.Name, Arguments: encodeJSON(map[string]string{commandProperty: createCommand, pathProperty: path, contentProperty: content})}, nil
			}
		}
		return FunctionCall{}, errors.New("no declared tool requires exactly two string properties, one named file_path, filePath, path or absolute_path and one named content or file_text, or such a path property and a command offering create beside one content or file_text property")
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
	Required   []string                  `json:"required"`
	Properties map[string]propertySchema `json:"properties"`
}

type propertySchema struct {
	Type  string              `json:"type"`
	Enum  []string            `json:"enum"`
	Items *requiredProperties `json:"items"`
}

func (p requiredProperties) isString(property string) bool {
	return strings.EqualFold(p.Properties[property].Type, "string")
}

// soleStringNamed returns the one declared string property whose normalized
// name is one of names, whether or not it is required, and false when none or
// several are.
func (p requiredProperties) soleStringNamed(names []string) (string, bool) {
	var found string
	for property := range p.Properties {
		if p.isString(property) && slices.Contains(names, normalizePropertyName(property)) {
			if found != "" {
				return "", false
			}
			found = property
		}
	}
	return found, found != ""
}

func (p requiredProperties) offers(property, value string) bool {
	return slices.Contains(p.Properties[property].Enum, value)
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

// batchReadProperties returns the names of the array property, the item mode
// property and the item path property of a schema requiring exactly that array,
// whose items require only a string mode offering Line.
func batchReadProperties(parameters json.RawMessage) (arrayProperty, modeProperty, pathProperty string, ok bool) {
	var schema requiredProperties
	if json.Unmarshal(parameters, &schema) != nil || len(schema.Required) != 1 {
		return "", "", "", false
	}
	arrayProperty = schema.Required[0]
	array := schema.Properties[arrayProperty]
	if !strings.EqualFold(array.Type, "array") || array.Items == nil || len(array.Items.Required) != 1 {
		return "", "", "", false
	}
	item := *array.Items
	modeProperty = item.Required[0]
	if normalizePropertyName(modeProperty) != "mode" || !item.isString(modeProperty) || !item.offers(modeProperty, lineMode) {
		return "", "", "", false
	}
	pathProperty, ok = item.soleStringNamed(pathPropertyNames)
	return arrayProperty, modeProperty, pathProperty, ok
}

// commandWriteProperties returns the names of the path, command and content
// properties of a schema requiring exactly a string path property and a string
// command offering create, and declaring one string content property.
func commandWriteProperties(parameters json.RawMessage) (pathProperty, commandProperty, contentProperty string, ok bool) {
	var schema requiredProperties
	if json.Unmarshal(parameters, &schema) != nil || len(schema.Required) != 2 {
		return "", "", "", false
	}
	for _, property := range schema.Required {
		if !schema.isString(property) {
			return "", "", "", false
		}
		switch name := normalizePropertyName(property); {
		case name == "command" && schema.offers(property, createCommand):
			commandProperty = property
		case slices.Contains(pathPropertyNames, name):
			pathProperty = property
		}
	}
	if pathProperty == "" || commandProperty == "" {
		return "", "", "", false
	}
	contentProperty, ok = schema.soleStringNamed(contentPropertyNames)
	return pathProperty, commandProperty, contentProperty, ok
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
