package codex

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/sortie-ai/sortie/internal/agent/mcpconfig"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

// passthroughConfig holds Codex-specific settings extracted from the
// "codex" sub-object in WORKFLOW.md. All fields are optional with
// zero-value meaning "not configured."
type passthroughConfig struct {
	Model             string
	Effort            string
	ApprovalPolicy    string
	ThreadSandbox     string
	TurnSandboxPolicy map[string]any
	Personality       string
}

// parsePassthroughConfig extracts Codex-specific settings from the
// raw config map. Missing or wrong-typed keys use zero-value defaults.
func parsePassthroughConfig(config map[string]any) passthroughConfig {
	return passthroughConfig{
		Model:             typeutil.StringFrom(config, "model"),
		Effort:            typeutil.StringFrom(config, "effort"),
		ApprovalPolicy:    typeutil.StringFrom(config, "approval_policy"),
		ThreadSandbox:     typeutil.StringFrom(config, "thread_sandbox"),
		TurnSandboxPolicy: typeutil.MapFrom(config, "turn_sandbox_policy"),
		Personality:       typeutil.StringFrom(config, "personality"),
	}
}

// buildSSHRemoteCmd returns the remote command string for SSH mode.
// When apiKey is non-empty, CODEX_API_KEY is prepended and the value
// is shell-quoted to prevent injection through the remote shell when
// the key contains metacharacters such as single quotes, dollar signs,
// semicolons, or backticks.
func buildSSHRemoteCmd(remoteCommand, apiKey string) string {
	if apiKey == "" {
		return remoteCommand
	}
	return "CODEX_API_KEY=" + sshutil.ShellQuote(apiKey) + " " + remoteCommand
}

// renderMCPServerOverrides renders each server as a "-c"/
// "mcp_servers.<name>=<inline table>" argument pair on the app-server
// launch arguments, one pair per server so an operator's own
// [mcp_servers] table entries merge rather than being replaced. The
// dotted-path segment naming the server must match the TOML bare-key
// grammar: the runtime splits the dotted path itself before any TOML
// parser sees it, so a quoted segment is rejected outright.
// processEnv is the adapter process's own environment, in os.Environ
// form, used to decide whether a stdio server's environment value is
// delivered by name through the runtime's passthrough or
// rendered literally.
func renderMCPServerOverrides(servers []mcpconfig.Server, processEnv []string) ([]string, error) {
	envLookup := make(map[string]string, len(processEnv))
	for _, entry := range processEnv {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			envLookup[name] = value
		}
	}

	args := make([]string, 0, len(servers)*2)
	for _, server := range servers {
		if !isTOMLBareKeySegment(server.Name) {
			return nil, fmt.Errorf("mcp server %q: name is not a valid codex dotted-path segment", server.Name)
		}
		table, err := renderMCPServerTable(server, envLookup)
		if err != nil {
			return nil, fmt.Errorf("mcp server %q: %w", server.Name, err)
		}
		args = append(args, "-c", "mcp_servers."+server.Name+"="+table)
	}
	return args, nil
}

// renderMCPServerTable renders one server as a TOML inline table.
// Structural keys, the runtime's own field names (command, args, env,
// env_vars, url, env_http_headers, enabled), are emitted bare. A key
// carrying an operator-controlled name, an environment variable name
// on the stdio branch or a header name on the HTTP branch, is emitted
// as a TOML quoted key: the only form safe for a name outside the
// bare-key set.
func renderMCPServerTable(server mcpconfig.Server, envLookup map[string]string) (string, error) {
	var fields []string

	switch server.Transport {
	case mcpconfig.TransportStdio:
		commandValue, err := tomlString(server.Command)
		if err != nil {
			return "", fmt.Errorf("command: %w", err)
		}
		fields = append(fields, "command="+commandValue)

		if len(server.Args) > 0 {
			argValues := make([]string, 0, len(server.Args))
			for _, arg := range server.Args {
				v, err := tomlString(arg)
				if err != nil {
					return "", fmt.Errorf("args: %w", err)
				}
				argValues = append(argValues, v)
			}
			fields = append(fields, "args=["+strings.Join(argValues, ", ")+"]")
		}

		literalEntries, passthroughNames, err := splitEnvForPassthrough(server.Env, envLookup)
		if err != nil {
			return "", err
		}
		if len(literalEntries) > 0 {
			fields = append(fields, "env={"+strings.Join(literalEntries, ", ")+"}")
		}
		if len(passthroughNames) > 0 {
			quotedNames := make([]string, 0, len(passthroughNames))
			for _, name := range passthroughNames {
				v, err := tomlString(name)
				if err != nil {
					return "", fmt.Errorf("env_vars: %w", err)
				}
				quotedNames = append(quotedNames, v)
			}
			fields = append(fields, "env_vars=["+strings.Join(quotedNames, ", ")+"]")
		}

	case mcpconfig.TransportHTTP:
		urlValue, err := tomlString(server.URL)
		if err != nil {
			return "", fmt.Errorf("url: %w", err)
		}
		fields = append(fields, "url="+urlValue)

		if len(server.Headers) > 0 {
			headerEntries, err := splitHeadersForPassthrough(server.Headers, envLookup)
			if err != nil {
				return "", err
			}
			fields = append(fields, "env_http_headers={"+strings.Join(headerEntries, ", ")+"}")
		}

	default:
		return "", fmt.Errorf("entry carries neither command nor url")
	}

	if server.Enabled != nil {
		fields = append(fields, fmt.Sprintf("enabled=%t", *server.Enabled))
	}

	return "{" + strings.Join(fields, ", ") + "}", nil
}

// splitEnvForPassthrough partitions env into literal "name=value"
// entries and passthrough names, both sorted by name for
// deterministic output. A name whose value matches envLookup under
// the same name is delivered by name only, through the runtime's
// environment passthrough; every other name is rendered with
// its literal value.
func splitEnvForPassthrough(env map[string]string, envLookup map[string]string) (literalEntries, passthroughNames []string, err error) {
	for _, name := range slices.Sorted(maps.Keys(env)) {
		value := env[name]
		if processValue, ok := envLookup[name]; ok && processValue == value {
			passthroughNames = append(passthroughNames, name)
			continue
		}
		keyStr, err := tomlQuotedKey(name)
		if err != nil {
			return nil, nil, fmt.Errorf("env: %w", err)
		}
		valueStr, err := tomlString(value)
		if err != nil {
			return nil, nil, fmt.Errorf("env: %w", err)
		}
		literalEntries = append(literalEntries, keyStr+"="+valueStr)
	}
	return literalEntries, passthroughNames, nil
}

// splitHeadersForPassthrough maps each header name to the name of an
// environment variable holding its value, sorted by header name for
// deterministic output. A header is delivered by variable name so its
// value never reaches the agent's argument list, which any local user
// of the host can read. A header whose value matches no variable
// cannot be delivered that way and fails by name, carrying the header
// name but never the value.
func splitHeadersForPassthrough(headers map[string]string, envLookup map[string]string) ([]string, error) {
	entries := make([]string, 0, len(headers))
	for _, header := range slices.Sorted(maps.Keys(headers)) {
		var matches []string
		for name, value := range envLookup {
			if value == headers[header] {
				matches = append(matches, name)
			}
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("header %q: value is in no environment variable, and delivering it inline would place it in the agent's argument list", header)
		}
		headerKey, err := tomlQuotedKey(header)
		if err != nil {
			return nil, fmt.Errorf("headers: %w", err)
		}
		variable, err := tomlString(slices.Min(matches))
		if err != nil {
			return nil, fmt.Errorf("headers: %w", err)
		}
		entries = append(entries, headerKey+"="+variable)
	}
	return entries, nil
}

// isTOMLBareKeySegment reports whether s matches the TOML bare-key
// grammar the runtime requires for a dotted-path segment.
func isTOMLBareKeySegment(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// tomlString renders s as a TOML basic string, escaping '"', '\', and
// control characters. Fails when s is not valid UTF-8.
func tomlString(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("value is not valid UTF-8")
	}
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String(), nil
}

// tomlQuotedKey renders name as a TOML quoted key: a basic string
// used in key position, required for an operator-controlled name that
// may fall outside the TOML bare-key grammar.
func tomlQuotedKey(name string) (string, error) {
	return tomlString(name)
}
