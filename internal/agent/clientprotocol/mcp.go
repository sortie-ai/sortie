package clientprotocol

import (
	"fmt"
	"sort"

	"github.com/sortie-ai/sortie/internal/agent/mcpconfig"
	"github.com/sortie-ai/sortie/internal/domain"
)

// parsedMCPServers is the generated MCP configuration, parsed and
// filtered once during StartSession's early launch-target resolution.
// The pinned schema's session-creation request needs to know, before
// the handshake, whether a bad configuration should fail the session at
// all (a parse failure does), but it needs to know, only after the
// handshake, whether the agent advertised the HTTP MCP capability (an
// HTTP server is omitted otherwise). Splitting parsing from rendering
// resolves that ordering: parseMCPServers runs early and can fail
// StartSession outright, and wireServers renders the already-parsed
// list once the handshake's answer is known.
type parsedMCPServers struct {
	servers []mcpconfig.Server
}

// parseMCPServers reads and normalizes the generated configuration at
// mcpConfigPath. On a remote launch it holds no servers regardless of
// mcpConfigPath: the protocol's stdio server names an executable path a
// remote agent resolves on the far side, and the configuration's
// environment block can carry tracker credentials that must not cross
// to a remote host. A server whose Enabled field is present and false is
// dropped here, before rendering, because the protocol carries no
// disabled state of its own.
func parseMCPServers(mcpConfigPath string, remote bool) (parsedMCPServers, *domain.AgentError) {
	if remote || mcpConfigPath == "" {
		return parsedMCPServers{}, nil
	}

	parsed, err := mcpconfig.Parse(mcpConfigPath)
	if err != nil {
		return parsedMCPServers{}, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: fmt.Sprintf("parse MCP config: %v", err),
			Err:     err,
		}
	}

	kept := make([]mcpconfig.Server, 0, len(parsed))
	for _, srv := range parsed {
		if srv.Enabled != nil && !*srv.Enabled {
			continue
		}
		kept = append(kept, srv)
	}
	return parsedMCPServers{servers: kept}, nil
}

// wireServers renders the parsed configuration into the session-
// creation request's server list, in the stable order mcpconfig.Parse
// returned. allowHTTP reports whether the handshake advertised the HTTP
// MCP capability; an HTTP server is omitted when it is false, and
// withheld reports that omission so the caller can lower the
// tool-servers capability entry.
func (p parsedMCPServers) wireServers(allowHTTP bool) (servers []mcpServer, withheld bool) {
	servers = []mcpServer{}
	for _, srv := range p.servers {
		switch srv.Transport {
		case mcpconfig.TransportHTTP:
			if !allowHTTP {
				withheld = true
				continue
			}
			servers = append(servers, mcpServer{HTTP: &mcpServerHttp{
				Name:    srv.Name,
				URL:     srv.URL,
				Headers: sortedHTTPHeaders(srv.Headers),
			}})
		default:
			args := srv.Args
			if args == nil {
				args = []string{}
			}
			servers = append(servers, mcpServer{Stdio: &mcpServerStdio{
				Name:    srv.Name,
				Command: srv.Command,
				Args:    args,
				Env:     sortedEnvVars(srv.Env),
			}})
		}
	}
	return servers, withheld
}

// sortedEnvVars renders a stdio server's environment as an array sorted
// by name, so the request is byte-stable across runs.
func sortedEnvVars(env map[string]string) []envVariable {
	out := make([]envVariable, 0, len(env))
	for name, value := range env {
		out = append(out, envVariable{Name: name, Value: value})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// sortedHTTPHeaders renders an HTTP server's headers as an array sorted
// by name, so the request is byte-stable across runs.
func sortedHTTPHeaders(headers map[string]string) []httpHeader {
	out := make([]httpHeader, 0, len(headers))
	for name, value := range headers {
		out = append(out, httpHeader{Name: name, Value: value})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
