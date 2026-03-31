# sortie-kb — MCP Knowledge Base Server

MCP server that exposes the Sortie project documentation as tools.
Intended for use with Claude Code or any MCP-compatible client.

## Tools

| Tool | Description |
|------|-------------|
| `list_docs` | Lists all available documentation files |
| `list_sections` | Returns the table of contents for a document |
| `get_section` | Returns the content of a specific section |
| `search_docs` | Searches for a query across all documents and returns matching sections |

Typical usage:

- `list_docs` → `list_sections` → `get_section`
- `search_docs` → `get_section`

## Build

```sh
cd .github/mcp
go build -o sortie-kb .
```

## Run

The server uses stdio transport and auto-discovers the `docs/` directory by
walking up from its own location, so no absolute paths are needed.

```sh
./sortie-kb
```

Override the docs directory:

```sh
SORTIE_DOCS_PATH=/path/to/docs ./sortie-kb
```

## Claude Code integration

Add to `.claude/settings.json` at the repo root:

```json
{
  "mcpServers": {
    "sortie-kb": {
      "type": "stdio",
      "command": ".github/mcp/sortie-kb"
    }
  }
}
```

Add to `.vscode/mcp.json`:

```json
{
  "servers": {
    "sortie-kb": {
      "type": "stdio",
      "command": ".github/mcp/sortie-kb"
    }
  },
  "inputs": []
}
```

## Extending

Add new tools in `main.go` via `s.AddTool(...)`. Each tool handler has the signature:
`func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error)`.

Place additional documentation in `docs/` and it will appear in `list_docs` automatically.
