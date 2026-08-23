// Package mcpconfig parses the worker-generated MCP configuration file
// into a runtime-neutral shape an agent adapter can render into its
// own runtime's configuration form. It lives under internal/agent/
// rather than internal/domain/ because parsing this file is a concern
// shared by adapter packages, not part of the orchestrator's domain
// model, and both translating adapters use it rather than duplicating
// the logic per adapter.
package mcpconfig

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
)

// Transport identifies how a server communicates with the agent
// runtime.
type Transport string

const (
	// TransportStdio identifies a server launched as a child process
	// communicating over stdio.
	TransportStdio Transport = "stdio"

	// TransportHTTP identifies a server reached over streamable HTTP.
	TransportHTTP Transport = "http"
)

// Server is one entry of the generated configuration's mcpServers
// object, normalized so an adapter can render it into the
// configuration form its runtime parses.
type Server struct {
	// Name is the object key the entry was declared under, e.g.
	// "sortie-tools".
	Name string

	// Transport reports whether Command or URL carries the server's
	// launch target.
	Transport Transport

	// Command is the server's executable path. Set only when
	// Transport is TransportStdio.
	Command string

	// Args are the command's arguments. Set only when Transport is
	// TransportStdio.
	Args []string

	// Env holds the server's environment variables. Set only when
	// Transport is TransportStdio.
	Env map[string]string

	// URL is the server's endpoint. Set only when Transport is
	// TransportHTTP.
	URL string

	// Headers holds request headers sent to the server. Set only when
	// Transport is TransportHTTP.
	Headers map[string]string

	// Enabled is the operator's enable flag. Nil when the entry omits
	// it.
	Enabled *bool
}

// ErrorKind classifies why [Parse] failed.
type ErrorKind string

const (
	// ErrorUnreadable reports that the file at the failing [Error]'s
	// Path could not be read.
	ErrorUnreadable ErrorKind = "unreadable"

	// ErrorNotJSON reports that the file's content is not a JSON
	// object, that its mcpServers member is present but not an
	// object, or that an entry within it is not an object.
	ErrorNotJSON ErrorKind = "not_json"

	// ErrorEntryNotExpressible reports that an entry carries neither
	// "command" nor "url", or carries both, so its transport cannot be
	// inferred.
	ErrorEntryNotExpressible ErrorKind = "entry_not_expressible"

	// ErrorUnmodeledKey reports that an entry carries a key outside
	// the set this package models.
	ErrorUnmodeledKey ErrorKind = "unmodeled_key"
)

// modeledKeys is the complete set of keys an entry may carry. Derived
// from the shape the worker's generator writes and the entry forms
// each translating runtime accepts; growing it requires measuring the
// candidate key against both runtimes first.
var modeledKeys = []string{"type", "command", "args", "env", "url", "headers", "enabled"}

// Error reports why [Parse] failed, naming the file and, when the
// fault is scoped to one entry, the server and, for an unmodeled key,
// the offending key.
type Error struct {
	Kind   ErrorKind
	Path   string
	Server string
	Key    string
	Err    error
}

// Error implements the error interface.
func (e *Error) Error() string {
	switch {
	case e.Server != "" && e.Key != "":
		return fmt.Sprintf("mcpconfig %s: server %q: key %q: %s", e.Path, e.Server, e.Key, e.detail())
	case e.Server != "":
		return fmt.Sprintf("mcpconfig %s: server %q: %s", e.Path, e.Server, e.detail())
	default:
		return fmt.Sprintf("mcpconfig %s: %s", e.Path, e.detail())
	}
}

func (e *Error) detail() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	switch e.Kind {
	case ErrorEntryNotExpressible:
		return "entry carries neither command nor url, or carries both"
	case ErrorUnmodeledKey:
		return "entry carries a key this parser does not model"
	default:
		return string(e.Kind)
	}
}

// Unwrap returns the underlying error, when Parse wrapped one.
func (e *Error) Unwrap() error { return e.Err }

// Parse reads the worker-generated MCP configuration at path and
// returns its servers in stable order by name.
//
// path must be non-empty and readable. The document must be a JSON
// object; mcpServers must be absent or an object; each entry must be
// an object. Parse performs one file read and no writes, network
// calls, or environment reads.
func Parse(path string) ([]Server, error) {
	if path == "" {
		return nil, &Error{Kind: ErrorUnreadable, Path: path, Err: fmt.Errorf("path is empty")}
	}

	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is the caller-supplied generated config location
	if err != nil {
		return nil, &Error{Kind: ErrorUnreadable, Path: path, Err: err}
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, &Error{Kind: ErrorNotJSON, Path: path, Err: err}
	}
	// A JSON null unmarshals into a nil map without an error, so the
	// null document and the null member both have to be rejected
	// explicitly. Reading either as "no servers" would launch a
	// translating adapter with none of the tools its declaration
	// promises.
	if doc == nil {
		return nil, &Error{Kind: ErrorNotJSON, Path: path, Err: fmt.Errorf("document is null, want an object")}
	}

	serversRaw, ok := doc["mcpServers"]
	if !ok {
		return nil, nil
	}

	var entries map[string]json.RawMessage
	if err := json.Unmarshal(serversRaw, &entries); err != nil {
		return nil, &Error{Kind: ErrorNotJSON, Path: path, Err: fmt.Errorf("mcpServers is not an object: %w", err)}
	}
	if entries == nil {
		return nil, &Error{Kind: ErrorNotJSON, Path: path, Err: fmt.Errorf("mcpServers is null, want an object")}
	}

	names := slices.Sorted(maps.Keys(entries))
	servers := make([]Server, 0, len(names))
	for _, name := range names {
		server, err := parseEntry(path, name, entries[name])
		if err != nil {
			return nil, err
		}
		servers = append(servers, server)
	}
	return servers, nil
}

// urlTransportTypes are the type names that describe a URL-addressed
// server. The set is deliberately partial: a name outside it, and
// outside the stdio name, is left alone rather than judged against a
// vocabulary this package does not own.
var urlTransportTypes = []string{"http", "sse", "streamable-http"}

// checkTransportAgreement rejects an entry whose declared type
// contradicts the transport its own fields imply. The type is optional
// and its absence is not an error; accepting a present one without
// reading it would let a contradictory entry through under a key the
// parser claims to model.
func checkTransportAgreement(path, name string, fields map[string]json.RawMessage, hasCommand bool) error {
	raw, ok := fields["type"]
	if !ok {
		return nil
	}

	var declared string
	if err := json.Unmarshal(raw, &declared); err != nil {
		return &Error{Kind: ErrorNotJSON, Path: path, Server: name, Key: "type", Err: err}
	}

	contradicts := (declared == "stdio" && !hasCommand) ||
		(slices.Contains(urlTransportTypes, declared) && hasCommand)
	if contradicts {
		return &Error{Kind: ErrorEntryNotExpressible, Path: path, Server: name, Key: "type"}
	}
	return nil
}

// parseEntry normalizes one mcpServers object member into a [Server].
func parseEntry(path, name string, raw json.RawMessage) (Server, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Server{}, &Error{Kind: ErrorNotJSON, Path: path, Server: name, Err: err}
	}

	for key := range fields {
		if !slices.Contains(modeledKeys, key) {
			return Server{}, &Error{Kind: ErrorUnmodeledKey, Path: path, Server: name, Key: key}
		}
	}

	_, hasCommand := fields["command"]
	_, hasURL := fields["url"]
	if hasCommand == hasURL {
		return Server{}, &Error{Kind: ErrorEntryNotExpressible, Path: path, Server: name}
	}

	if err := checkTransportAgreement(path, name, fields, hasCommand); err != nil {
		return Server{}, err
	}

	server := Server{Name: name}

	if enabledRaw, ok := fields["enabled"]; ok {
		var enabled bool
		if err := json.Unmarshal(enabledRaw, &enabled); err != nil {
			return Server{}, &Error{Kind: ErrorNotJSON, Path: path, Server: name, Key: "enabled", Err: err}
		}
		server.Enabled = &enabled
	}

	if hasCommand {
		server.Transport = TransportStdio
		if err := json.Unmarshal(fields["command"], &server.Command); err != nil {
			return Server{}, &Error{Kind: ErrorNotJSON, Path: path, Server: name, Key: "command", Err: err}
		}
		if argsRaw, ok := fields["args"]; ok {
			if err := json.Unmarshal(argsRaw, &server.Args); err != nil {
				return Server{}, &Error{Kind: ErrorNotJSON, Path: path, Server: name, Key: "args", Err: err}
			}
		}
		if envRaw, ok := fields["env"]; ok {
			if err := json.Unmarshal(envRaw, &server.Env); err != nil {
				return Server{}, &Error{Kind: ErrorNotJSON, Path: path, Server: name, Key: "env", Err: err}
			}
		}
		return server, nil
	}

	server.Transport = TransportHTTP
	if err := json.Unmarshal(fields["url"], &server.URL); err != nil {
		return Server{}, &Error{Kind: ErrorNotJSON, Path: path, Server: name, Key: "url", Err: err}
	}
	if headersRaw, ok := fields["headers"]; ok {
		if err := json.Unmarshal(headersRaw, &server.Headers); err != nil {
			return Server{}, &Error{Kind: ErrorNotJSON, Path: path, Server: name, Key: "headers", Err: err}
		}
	}
	return server, nil
}
